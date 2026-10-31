// Package query answers the read-only questions the unprivileged coolix user
// needs root-level data for: app logs and the list of database backups. On a
// host it runs as the socket-activated coolix-query@ service (a DynamicUser
// in the systemd-journal group, with the backup directories bind-mounted
// read-only), so the coolix user itself can't read the rest of the journal.
//
// Responses end with a control.Trailer status line, like coolix-control's.
package query

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"coolix/internal/control"
	"coolix/internal/units"
)

type Request struct {
	Kind   string // logs | backups
	App    string
	Lines  int  // logs
	Follow bool // logs
}

func (r Request) String() string {
	if r.Kind == "logs" {
		f := 0
		if r.Follow {
			f = 1
		}
		return fmt.Sprintf("logs %s %d %d", r.App, r.Lines, f)
	}
	return r.Kind + " " + r.App
}

const MaxLines = 10000

func Parse(line string) (Request, error) {
	f := strings.Fields(line)
	bad := fmt.Errorf("bad request %q", line)
	if len(f) == 0 {
		return Request{}, bad
	}
	r := Request{Kind: f[0]}
	switch {
	case r.Kind == "logs" && len(f) == 4:
		r.App = f[1]
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 0 || n > MaxLines || (f[3] != "0" && f[3] != "1") {
			return r, bad
		}
		r.Lines, r.Follow = n, f[3] == "1"
	case r.Kind == "backups" && len(f) == 2:
		r.App = f[1]
	default:
		return r, bad
	}
	if !units.ValidName(r.App) {
		return r, bad
	}
	return r, nil
}

// Server answers requests. BackupDir is where the per-app backup
// directories are visible (a read-only bind mount on a host).
type Server struct {
	BackupDir string
}

// AppUnits are the journal units of an app: its slots, databases, backups
// and restores.
func AppUnits(app string) []string {
	var out []string
	for _, slot := range units.Slots {
		out = append(out, units.SlotUnit(app, slot))
	}
	for _, kind := range []string{"postgres", "redis"} {
		out = append(out, units.DBUnit(app, kind))
	}
	return append(out, units.BackupUnit(app), units.RestorePrefix+app+"@*.service")
}

func (s *Server) Handle(ctx context.Context, r Request, w io.Writer) error {
	switch r.Kind {
	case "logs":
		args := []string{"--no-pager", fmt.Sprintf("--lines=%d", r.Lines), "--output=short-iso", "--no-hostname"}
		if r.Follow {
			args = append(args, "--follow")
		}
		for _, u := range AppUnits(r.App) {
			args = append(args, "--unit="+u)
		}
		return Journal(ctx, w, args...)
	case "backups":
		files, err := ListBackups(filepath.Join(s.BackupDir, r.App))
		if err != nil {
			return err
		}
		for _, f := range files {
			fmt.Fprintf(w, "%s\t%d\t%d\n", f.Name, f.Size, f.Time.Unix())
		}
		return nil
	}
	return fmt.Errorf("unknown request %q", r.Kind)
}

// Journal runs journalctl; a cancelled context (the client went away) is not
// an error.
func Journal(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type BackupFile struct {
	Name string
	Size int64
	Time time.Time
}

// ListBackups returns the dumps in dir, newest first. A missing directory
// means no backups yet.
func ListBackups(dir string) ([]BackupFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if !units.ValidBackupFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, BackupFile{Name: e.Name(), Size: info.Size(), Time: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Serve handles one connection (stdin/stdout of a socket-activated
// instance): a request line, then the answer and a status trailer. The
// client closing its end cancels long-running answers such as followed logs.
func Serve(ctx context.Context, in io.Reader, out io.Writer, s *Server) error {
	br := bufio.NewReaderSize(in, 256)
	line, err := readLine(br, 256)
	if err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	r, err := Parse(line)
	if err == nil {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			io.Copy(io.Discard, br)
			cancel()
		}()
		err = s.Handle(ctx, r, out)
	}
	if err != nil {
		fmt.Fprintf(out, "%serror: %s\n", control.Trailer, strings.ReplaceAll(err.Error(), "\n", "; "))
		return err
	}
	fmt.Fprintf(out, "%sok\n", control.Trailer)
	return nil
}

// readLine reads one line of at most max bytes.
func readLine(br *bufio.Reader, max int) (string, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > max {
		return "", errors.New("request too long")
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(line)), nil
}

// Client asks a query server. With Socket empty it answers in-process
// (development).
type Client struct {
	Socket string
	Local  *Server
}

func (c Client) Stream(ctx context.Context, r Request, w io.Writer) error {
	if c.Socket == "" {
		return c.Local.Handle(ctx, r, w)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return fmt.Errorf("connecting to coolix-query: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, r.String()+"\n"); err != nil {
		return err
	}
	tw := &control.TrailerWriter{W: w}
	_, err = io.Copy(tw, conn)
	if ctx.Err() != nil {
		return nil // we stopped following
	}
	if err != nil {
		return err
	}
	return tw.Result()
}

func (c Client) Backups(ctx context.Context, app string) ([]BackupFile, error) {
	var b strings.Builder
	if err := c.Stream(ctx, Request{Kind: "backups", App: app}, &b); err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected backup listing line %q", line)
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		out = append(out, BackupFile{Name: f[0], Size: size, Time: time.Unix(ts, 0)})
	}
	return out, nil
}
