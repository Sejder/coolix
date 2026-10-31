package caddy

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	got := Render([]string{"a.example.com", "b.example.com"}, 20001, TLSAcme)
	if !strings.Contains(got, "a.example.com, b.example.com {") || !strings.Contains(got, "reverse_proxy 127.0.0.1:20001") {
		t.Fatal(got)
	}
	if !strings.Contains(Render([]string{"a"}, 1, TLSOff), "http://a {") {
		t.Fatal("tls off must use http:// addresses")
	}
	if !strings.Contains(Render([]string{"a"}, 1, TLSInternal), "tls internal") {
		t.Fatal("tls internal missing")
	}
}

func TestSetRoute(t *testing.T) {
	reloads := 0
	var fail bool
	r := &Router{Dir: t.TempDir(), TLS: TLSOff, Reload: func() error {
		reloads++
		if fail {
			return errors.New("bad config")
		}
		return nil
	}}
	if err := r.SetRoute("blog", []string{"blog.test"}, 20000); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoute("blog", []string{"blog.test"}, 20000); err != nil || reloads != 1 {
		t.Fatalf("unchanged route must not reload (reloads=%d, err=%v)", reloads, err)
	}
	if r.Port("blog") != 20000 {
		t.Fatalf("port = %d", r.Port("blog"))
	}

	fail = true
	if err := r.SetRoute("blog", []string{"blog.test"}, 20001); err == nil {
		t.Fatal("expected reload error")
	}
	if r.Port("blog") != 20000 {
		t.Fatal("failed reload must restore the previous snippet")
	}

	fail = false
	if err := r.SetRoute("blog", nil, 20001); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.path("blog")); !os.IsNotExist(err) {
		t.Fatal("no domains must remove the route")
	}
}
