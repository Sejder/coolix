package control

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	ok := []string{
		"activate 0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-coolix-app-profile",
		"backup blog",
		"restore blog 20261031T120000Z.dump",
	}
	for _, l := range ok {
		r, err := Parse(l)
		if err != nil || r.String() != l {
			t.Errorf("%q: %v %q", l, err, r.String())
		}
	}
	bad := []string{
		"", "activate foo", "activate /nix/store/0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-coolix-app-profile",
		"backup ../x", "backup Blog", "restore blog ../../etc/shadow", "restore blog",
		"start sshd", "backup blog extra", "backup a-very-long-app-name-over-20",
	}
	for _, l := range bad {
		if _, err := Parse(l); err == nil {
			t.Errorf("%q accepted", l)
		}
	}
}

func TestServeAndDo(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var audit strings.Builder
	h := func(_ context.Context, r Request, w io.Writer) error {
		io.WriteString(w, "working on "+r.Arg+"\n")
		if r.Arg == "broken" {
			return errors.New("it broke\nbadly")
		}
		return nil
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			Serve(context.Background(), c, c, &audit, h)
			c.Close()
		}
	}()

	var out strings.Builder
	if err := Do(context.Background(), sock, Request{Verb: "backup", Arg: "blog"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "working on blog\n" {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	err = Do(context.Background(), sock, Request{Verb: "backup", Arg: "broken"}, &out)
	if err == nil || err.Error() != "it broke; badly" || out.String() != "working on broken\n" {
		t.Fatalf("err %v, output %q", err, out.String())
	}
	if !strings.Contains(audit.String(), "request: backup broken") {
		t.Fatalf("audit log: %q", audit.String())
	}
}

func TestTrailerWriterNeedsTrailer(t *testing.T) {
	var out strings.Builder
	tw := &TrailerWriter{W: &out}
	io.WriteString(tw, "partial output")
	if err := tw.Result(); err == nil {
		t.Fatal("a stream without a status must not count as success")
	}
}

func TestSanitizeStopsForgedTrailers(t *testing.T) {
	var out strings.Builder
	tw := &TrailerWriter{W: &out}
	io.WriteString(Sanitize(tw), Trailer+"error: fake\nline\r\n")
	io.WriteString(tw, Trailer+"ok\n")
	if err := tw.Result(); err != nil {
		t.Fatalf("forged trailer took effect: %v", err)
	}
	if strings.Contains(out.String(), "\r") || !strings.Contains(out.String(), "␞error: fake") {
		t.Fatalf("output %q", out.String())
	}
}
