package query

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	ok := []string{"logs blog 200 1", "logs blog 0 0", "backups blog"}
	for _, l := range ok {
		r, err := Parse(l)
		if err != nil {
			t.Errorf("%q: %v", l, err)
		} else if r.String() != l {
			t.Errorf("round trip %q -> %q", l, r.String())
		}
	}
	bad := []string{
		"", "logs", "logs ../etc 1 1", "logs blog -1 0", "logs Blog 1 1", "logs blog 1 2",
		"logs blog 20000 0", "unit sshd.service", "backups ../x", "exec sh",
	}
	for _, l := range bad {
		if _, err := Parse(l); err == nil {
			t.Errorf("%q accepted", l)
		}
	}
}

func serve(t *testing.T, s *Server) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "q.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				Serve(context.Background(), c, c, s)
			}()
		}
	}()
	return sock
}

func TestBackups(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "blog"), 0o755)
	for _, n := range []string{"20261030T030000Z.dump", "20261031T030000Z.dump", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, "blog", n), []byte("x"), 0o600)
	}
	c := Client{Socket: serve(t, &Server{BackupDir: dir})}
	files, err := c.Backups(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "20261031T030000Z.dump" || files[0].Size != 1 {
		t.Fatalf("got %+v", files)
	}
	// No directory yet: no backups, no error.
	if files, err := c.Backups(context.Background(), "other"); err != nil || len(files) != 0 {
		t.Fatalf("missing dir: %v %v", files, err)
	}
	// An unreadable directory is an error, not "no backups".
	os.Chmod(filepath.Join(dir, "blog"), 0)
	defer os.Chmod(filepath.Join(dir, "blog"), 0o755)
	if os.Geteuid() != 0 {
		if _, err := c.Backups(context.Background(), "blog"); err == nil {
			t.Fatal("listing error was hidden")
		}
	}
}

func TestErrorsReachTheClient(t *testing.T) {
	sock := serve(t, &Server{})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte(strings.Repeat("x", 1000) + "\n"))
	buf := make([]byte, 100)
	n, _ := conn.Read(buf)
	conn.Close()
	_ = n // an over-long request just gets the connection closed

	var b strings.Builder
	err = Client{Socket: sock}.Stream(context.Background(), Request{Kind: "logs", App: "blog", Lines: 99999}, &b)
	if err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("want bad request error, got %v (output %q)", err, b.String())
	}
}
