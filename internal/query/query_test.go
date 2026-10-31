package query

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	ok := []string{
		"logs blog 200 1",
		"backups blog",
		"unit coolix-activate@0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-coolix-app-profile.service",
		"unit coolix-backup-blog.service",
		"unit coolix-restore-blog@20261031T120000Z.dump.service",
	}
	for _, l := range ok {
		r, err := Parse(l)
		if err != nil {
			t.Errorf("%q: %v", l, err)
		} else if r.String() != l {
			t.Errorf("round trip %q -> %q", l, r.String())
		}
	}
	bad := []string{
		"", "logs", "logs ../etc 1 1", "logs blog -1 0", "logs Blog 1 1",
		"unit sshd.service", "unit coolix-app-blog@blue.service",
		"unit coolix-restore-blog@../../x.service", "backups ../x", "exec sh",
		"unit coolix-backup-blog.service --since=-1y",
	}
	for _, l := range bad {
		if _, err := Parse(l); err == nil {
			t.Errorf("%q accepted", l)
		}
	}
}

func TestServeBackups(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "blog"), 0o755)
	for _, n := range []string{"20261030T030000Z.dump", "20261031T030000Z.dump", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, "blog", n), []byte("x"), 0o600)
	}
	sock := filepath.Join(t.TempDir(), "q.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				Serve(context.Background(), c, c, &Server{BackupDir: dir})
			}()
		}
	}()
	// Serve waits for the client to hang up, so Backups must work without a
	// half-close: the server finishes its answer and closes.
	files, err := Client{Socket: sock}.Backups(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "20261031T030000Z.dump" || files[0].Size != 1 {
		t.Fatalf("got %+v", files)
	}
}
