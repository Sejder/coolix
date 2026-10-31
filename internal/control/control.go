// Package control is how the unprivileged coolix user asks root to do the
// few privileged things it may do: activate a built app profile, and back up
// or restore an app's database. It replaces polkit, which can't tell
// "start this existing unit" from "create a transient unit of that name".
//
// On a host, coolix-control.socket (group coolix) spawns a root
// coolix-control@ instance per connection. The client sends one request line
// and reads the output until the connection closes; the last line is a
// status trailer starting with Trailer.
package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"coolix/internal/units"
)

// Trailer starts the final status line of a response. journalctl never
// prints it inside a log line (it shows control characters as blob data),
// so app output can't forge a status.
const Trailer = "\x1e"

var (
	profileRe = regexp.MustCompile(`^[0-9a-df-np-sv-z]{32}-coolix-app-profile$`)
)

type Request struct {
	Verb string // activate | backup | restore
	Arg  string // profile store name (activate) or app name
	File string // restore: backup file name
}

func (r Request) String() string {
	if r.Verb == "restore" {
		return fmt.Sprintf("restore %s %s", r.Arg, r.File)
	}
	return r.Verb + " " + r.Arg
}

func Parse(line string) (Request, error) {
	f := strings.Fields(line)
	bad := fmt.Errorf("bad request %q", line)
	switch {
	case len(f) == 2 && f[0] == "activate" && profileRe.MatchString(f[1]):
		return Request{Verb: f[0], Arg: f[1]}, nil
	case len(f) == 2 && f[0] == "backup" && units.ValidName(f[1]):
		return Request{Verb: f[0], Arg: f[1]}, nil
	case len(f) == 3 && f[0] == "restore" && units.ValidName(f[1]) && units.ValidBackupFile(f[2]):
		return Request{Verb: f[0], Arg: f[1], File: f[2]}, nil
	}
	return Request{}, bad
}

// Handler performs a request, writing progress to w.
type Handler func(ctx context.Context, r Request, w io.Writer) error

// detachedWriter keeps the server going when the client disappears: a
// half-finished activation is worse than one nobody watches.
type detachedWriter struct {
	mu     sync.Mutex
	w      io.Writer
	broken bool
}

func (d *detachedWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.broken {
		if _, err := d.w.Write(p); err != nil {
			d.broken = true
		}
	}
	return len(p), nil
}

// Serve handles one connection. log also receives all output (the
// service's journal keeps an audit trail of what root did).
func Serve(ctx context.Context, in io.Reader, out io.Writer, log io.Writer, h Handler) error {
	line, err := readRequest(in, 10*time.Second)
	if err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	w := io.MultiWriter(&detachedWriter{w: out}, log)
	r, err := Parse(strings.TrimSpace(line))
	if err == nil {
		fmt.Fprintf(log, "request: %s\n", r)
		err = h(ctx, r, Sanitize(w))
	}
	if err != nil {
		fmt.Fprintf(w, "%serror: %s\n", Trailer, strings.ReplaceAll(err.Error(), "\n", "; "))
		return err
	}
	fmt.Fprintf(w, "%sok\n", Trailer)
	return nil
}

// TrailerWriter forwards complete lines to W and turns a status trailer
// into Err. Shared by the control and query clients.
type TrailerWriter struct {
	W    io.Writer
	buf  []byte
	Done bool
	Err  error
}

func (t *TrailerWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		i := strings.IndexByte(string(t.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(t.buf[:i+1])
		t.buf = t.buf[i+1:]
		if status, ok := strings.CutPrefix(line, Trailer); ok {
			t.Done = true
			status = strings.TrimSpace(status)
			if status != "ok" {
				t.Err = errors.New(strings.TrimPrefix(status, "error: "))
			}
			continue
		}
		if _, err := io.WriteString(t.W, line); err != nil {
			return len(p), err
		}
	}
}

// Result returns the outcome once the stream ended.
func (t *TrailerWriter) Result() error {
	if len(t.buf) > 0 {
		io.WriteString(t.W, string(t.buf))
		t.buf = nil
	}
	if !t.Done {
		return errors.New("connection closed without a result")
	}
	return t.Err
}

// Do sends r to the control socket and streams its output to w.
func Do(ctx context.Context, socket string, r Request, w io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("connecting to coolix-control: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, r.String()+"\n"); err != nil {
		return err
	}
	tw := &TrailerWriter{W: w}
	if _, err := io.Copy(tw, conn); err != nil && ctx.Err() == nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tw.Result()
}

// Sanitize escapes what could forge a status trailer or split lines in the
// web UI's event streams: output passes through it, the server's own
// trailer doesn't.
func Sanitize(w io.Writer) io.Writer { return sanitizer{w} }

type sanitizer struct{ w io.Writer }

var sanitizeReplacer = strings.NewReplacer(Trailer, "␞", "\r", "")

func (s sanitizer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, sanitizeReplacer.Replace(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// readRequest reads the request line, giving up after timeout.
func readRequest(in io.Reader, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReaderSize(io.LimitReader(in, 512), 512).ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-time.After(timeout):
		return "", errors.New("timed out waiting for a request")
	}
}
