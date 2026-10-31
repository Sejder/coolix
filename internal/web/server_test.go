package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coolix/internal/appdef"
	"coolix/internal/engine"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	srv, c, _ := newTestServerWithEngine(t)
	return srv, c
}

func newTestServerWithEngine(t *testing.T) (*httptest.Server, *http.Client, *engine.Engine) {
	t.Helper()
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("hunter2\n"), 0o600)
	s := &Server{
		Config: &engine.Config{
			StateDir: dir, Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux",
			PortMin: 20000, PortMax: 20100, Profile: filepath.Join(dir, "profile"),
		},
		PasswordFile: pw,
		SessionKey:   []byte("0123456789abcdef0123456789abcdef"),
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return srv, &http.Client{Jar: jar}, &engine.Engine{Config: s.Config, Out: os.Stderr}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

func TestAuthAndPages(t *testing.T) {
	srv, c := newTestServer(t)

	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(t, resp), "Admin password") {
		t.Fatal("unauthenticated request not sent to login")
	}

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"hunter2"}})
	if b := body(t, resp); !strings.Contains(b, "Nothing deployed yet") {
		t.Fatalf("dashboard after login:\n%s", b)
	}

	resp, _ = c.PostForm(srv.URL+"/apps", url.Values{
		"name": {"blog"}, "source": {"github:me/blog"}, "domains": {"blog.example.com\nwww.example.com"},
		"env": {"LOG_LEVEL=debug"}, "db_postgres": {"1"}, "health_path": {"/healthz"},
	})
	b := body(t, resp)
	for _, want := range []string{"App created", "blog.example.com", "LOG_LEVEL=debug", "not deployed", `name="db_postgres" value="1" checked`} {
		if !strings.Contains(b, want) {
			t.Errorf("app page missing %q", want)
		}
	}

	resp, _ = c.PostForm(srv.URL+"/apps/blog", url.Values{"source": {"github:me/blog"}, "env": {"not an env line"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body(t, resp), "want KEY=VALUE") {
		t.Fatal("invalid env not rejected")
	}

	resp, _ = c.PostForm(srv.URL+"/apps", url.Values{"name": {"Bad Name"}, "source": {"x"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid app accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, p := range []string{"/", "/generations", "/apps/new", "/apps/blog"} {
		resp, _ := c.Get(srv.URL + p)
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	srv, c := newTestServer(t)
	resp, _ := c.PostForm(srv.URL+"/login", url.Values{"password": {"hunter2"}})
	resp.Body.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/deploy", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestForgedSessionRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "99999999999.forged"})
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("forged session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestWebhookAuth(t *testing.T) {
	srv, _, e := newTestServerWithEngine(t)
	post := func(app string, h map[string]string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/hooks/"+app, strings.NewReader(`{"ref":"refs/heads/main"}`))
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("nope", nil); got != 404 {
		t.Errorf("unknown app: %d", got)
	}
	if err := e.CreateApp(&appdef.App{Name: "blog", Source: "github:me/blog"}); err != nil {
		t.Fatal(err)
	}
	if got := post("blog", map[string]string{"Authorization": "Bearer x"}); got != 404 {
		t.Errorf("webhook off: %d", got)
	}
	token, err := e.EnableWebhook("blog")
	if err != nil {
		t.Fatal(err)
	}
	if got := post("blog", map[string]string{"Authorization": "Bearer wrong"}); got != 401 {
		t.Errorf("bad token: %d", got)
	}
	if got := post("blog", map[string]string{"X-Gitlab-Token": token, "X-Gitlab-Event": "Tag Push Hook"}); got != 202 {
		t.Errorf("ignored event: %d", got)
	}
	if got := post("blog", map[string]string{"X-Gitlab-Token": token, "X-Gitlab-Event": "ping"}); got != 200 {
		t.Errorf("ping: %d", got)
	}
	if err := e.RemoveApp("blog"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := e.WebhookToken("blog"); tok != "" {
		t.Error("token survived app removal")
	}
}
