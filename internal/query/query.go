// Package query answers the few read-only questions the unprivileged coolix
// user needs root-level data for: app logs, the output of activation and
// backup units, and the list of database backups. On a host it runs as the
// socket-activated coolix-query@ service (a DynamicUser in the
// systemd-journal group), so the coolix user itself can't read the rest of
// the journal.
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"coolix/internal/units"
)

type Request struct {
	Kind   string // logs | unit | backups
	App    string // logs, backups
	Unit   string // unit
	Lines  int    // logs
	Follow bool   // logs
}

func (r Request) String() string {
	switch r.Kind {
	case "logs":
		f := 0
		if r.Follow {
			f = 1
		}
		return fmt.Sprintf("logs %s %d %d", r.App, r.Lines, f)
	case "unit":
		return "unit " + r.Unit
	}
	return r.Kind + " " + r.App
}

var (
	activationRe = regexp.MustCompile(`^coolix-activate@[0-9a-df-np-sv-z]{32}-coolix-app-profile\.service$`)
	backupFileRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.dump$`)
)

// ValidBackupFile reports whether name looks like a dump coolix wrote.
func ValidBackupFile(name string) bool { return backupFileRe.MatchString(name) }

// FollowableUnit reports whether the output of unit may be streamed.
func FollowableUnit(unit string) bool {
	if activationRe.MatchString(unit) {
		return true
	}
	if app, ok := strings.CutPrefix(unit, units.BackupPrefix); ok {
		return units.ValidName(strings.TrimSuffix(app, ".service")) && strings.HasSuffix(app, ".service")
	}
	if rest, ok := strings.CutPrefix(unit, units.RestorePrefix); ok {
		app, file, ok := strings.Cut(strings.TrimSuffix(rest, ".service"), "@")
		return ok && strings.HasSuffix(rest, ".service") && units.ValidName(app) && ValidBackupFile(file)
	}
	return false
}

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
		if err != nil || n < 0 || n > 10000 {
			return r, bad
		}
		r.Lines, r.Follow = n, f[3] == "1"
	case r.Kind == "backups" && len(f) == 2:
		r.App = f[1]
	case r.Kind == "unit" && len(f) == 2 && FollowableUnit(f[1]):
		r.Unit = f[1]
		return r, nil
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

func (s *Server) Handle(ctx context.Context, r Request, w io.Writer) error {
	switch r.Kind {
	case "logs":
		args := []string{"--no-pager", fmt.Sprintf("--lines=%d", r.Lines), "--output=short-iso", "--no-hostname"}
		if r.Follow {
			args = append(args, "--follow")
		}
		for _, slot := range units.Slots {
			args = append(args, "--unit="+units.SlotUnit(r.App, slot))
		}
		for _, kind := range []string{"postgres", "redis"} {
			args = append(args, "--unit="+units.DBUnit(r.App, kind))
		}
		args = append(args, "--unit="+units.BackupUnit(r.App), "--unit="+units.RestorePrefix+r.App+"@*.service")
		return journal(ctx, w, args...)
	case "unit":
		return journal(ctx, w, "--no-pager", "--follow", "--output=cat", "--since=-1s", "--unit="+r.Unit)
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

func journal(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil // the client went away
	}
	return err
}

type BackupFile struct {
	Name string
	Size int64
	Time time.Time
}

// ListBackups returns the dumps in dir, newest first.
func ListBackups(dir string) ([]BackupFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if !ValidBackupFile(e.Name()) {
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
// instance): a request line, then the answer. The client closing its end
// cancels long-running answers such as followed logs.
func Serve(ctx context.Context, in io.Reader, out io.Writer, s *Server) error {
	br := bufio.NewReaderSize(in, 512)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	r, err := Parse(strings.TrimSpace(line))
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		io.Copy(io.Discard, br)
		cancel()
	}()
	return s.Handle(ctx, r, out)
}

// Client asks a query server. With Socket empty it answers in-process
// (development, or when running as root).
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
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, r.String()+"\n"); err != nil {
		return err
	}
	_, err = io.Copy(w, conn)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (c Client) Backups(ctx context.Context, app string) ([]BackupFile, error) {
	var b strings.Builder
	if err := c.Stream(ctx, Request{Kind: "backups", App: app}, &b); err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			if line != "" {
				return nil, errors.New(line)
			}
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		out = append(out, BackupFile{Name: f[0], Size: size, Time: time.Unix(ts, 0)})
	}
	return out, nil
}
