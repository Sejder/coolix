package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coolix/internal/appdef"
	"coolix/internal/engine"

	"github.com/go-webauthn/webauthn/webauthn"
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
	if !strings.Contains(body(t, resp), "Sign in") {
		t.Fatal("unauthenticated request not sent to login")
	}

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"hunter2"}})
	if b := body(t, resp); !strings.Contains(b, "No apps yet") {
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
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "99999999999.forged"})
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
	if got := post("nope", nil); got != 401 {
		t.Errorf("unknown app: %d", got)
	}
	if err := e.CreateApp(&appdef.App{Name: "blog", Source: "github:me/blog"}); err != nil {
		t.Fatal(err)
	}
	if got := post("blog", map[string]string{"Authorization": "Bearer x"}); got != 401 {
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

func TestLogoutRevokesSessions(t *testing.T) {
	srv, c := newTestServer(t)
	resp, _ := c.PostForm(srv.URL+"/login", url.Values{"password": {"hunter2"}})
	resp.Body.Close()
	u, _ := url.Parse(srv.URL)
	stolen := c.Jar.Cookies(u)
	if len(stolen) == 0 {
		t.Fatal("no session cookie")
	}
	resp, _ = c.PostForm(srv.URL+"/logout", nil)
	resp.Body.Close()

	// Replaying the old cookie must not work.
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.AddCookie(stolen[0])
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoked session still valid: %d", resp.StatusCode)
	}
}

func TestLoginLimiter(t *testing.T) {
	var l loginLimiter
	now := time.Now()
	for range maxFailures {
		if l.blocked("1.2.3.4", now) {
			t.Fatal("blocked too early")
		}
		l.fail("1.2.3.4", now)
	}
	if !l.blocked("1.2.3.4", now) || l.blocked("5.6.7.8", now) {
		t.Fatal("lockout must apply to the failing client only")
	}
	if l.blocked("1.2.3.4", now.Add(failureWindow+time.Second)) {
		t.Fatal("lockout must expire")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	// Over TCP (an app on the same host could connect), XFF is ignored.
	if got := clientIP(r); got != "127.0.0.1" {
		t.Fatalf("XFF trusted over TCP: %s", got)
	}
	// Through the unix socket only Caddy can open, the last entry counts.
	r = r.WithContext(context.WithValue(r.Context(), viaSocket, true))
	r.RemoteAddr = "@"
	if got := clientIP(r); got != "1.2.3.4" {
		t.Fatalf("behind Caddy: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "2001:db8:1:2:3:4:5:6")
	if got := clientIP(r); got != "2001:db8:1:2::/64" {
		t.Fatalf("IPv6 per /64: %s", got)
	}
}

func newSetupServer(t *testing.T) (*httptest.Server, *http.Client, string) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{
		Config:          &engine.Config{StateDir: dir, Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux", Profile: filepath.Join(dir, "profile")},
		SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
		WebAuthnOrigins: []string{"http://localhost:8090"},
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return srv, &http.Client{Jar: jar}, dir
}

func TestSetupFlow(t *testing.T) {
	srv, c, dir := newSetupServer(t)
	resp, _ := c.Get(srv.URL + "/")
	if b := body(t, resp); !strings.Contains(b, "sudo coolix setup-code") {
		t.Fatal("unclaimed install must show the setup page")
	}
	resp, _ = c.PostForm(srv.URL+"/setup", url.Values{"code": {"AAAA-BBBB-CCCC-DDDD"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no code generated yet, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	code, err := NewSetupCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = c.PostForm(srv.URL+"/setup", url.Values{"code": {code}, "password": {"short"}, "confirm": {"short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("short password accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()
	pw := "correct horse battery"
	resp, _ = c.PostForm(srv.URL+"/setup", url.Values{"code": {strings.ToLower(code)}, "password": {pw}, "confirm": {pw}})
	if b := body(t, resp); !strings.Contains(b, "Account created") {
		t.Fatalf("setup failed:\n%s", b)
	}
	// Claimed: the code is gone and setup is closed.
	if checkSetupCode(dir, code) {
		t.Fatal("setup code still valid after claiming")
	}
	other, _ := cookiejar.New(nil)
	resp, _ = (&http.Client{Jar: other}).Get(srv.URL + "/setup")
	if b := body(t, resp); strings.Contains(b, "setup-code") {
		t.Fatal("setup page still offered after claiming")
	}
	// The password works; removing it is refused (it's the only credential).
	c2 := &http.Client{Jar: other}
	resp, _ = c2.PostForm(srv.URL+"/login", url.Values{"password": {pw}})
	if b := body(t, resp); !strings.Contains(b, "No apps yet") {
		t.Fatal("login with the account password failed")
	}
	resp, _ = c2.PostForm(srv.URL+"/account/password/delete", nil)
	if b := body(t, resp); !strings.Contains(b, "only way left") {
		t.Fatalf("last credential removed:\n%s", b)
	}
	// Changing the password signs out other sessions.
	resp, _ = c2.PostForm(srv.URL+"/account/password", url.Values{"password": {pw + "2"}, "confirm": {pw + "2"}})
	resp.Body.Close()
	resp, _ = c.Get(srv.URL + "/account")
	if b := body(t, resp); !strings.Contains(b, "Sign in") {
		t.Fatal("other session survived a password change")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "correct horse batterY") || VerifyPassword("garbage", "x") {
		t.Fatal("password verification")
	}
}

func TestPasskeyEndpointsCheckOrigin(t *testing.T) {
	srv, c, _ := newSetupServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/login/passkey/begin", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("passkey ceremony started for a foreign origin")
	}
}

func TestCeremoniesEvictInsteadOfRefusing(t *testing.T) {
	var c ceremonies
	var admin string
	for i := range maxCeremonies + 50 {
		client := fmt.Sprintf("10.0.%d.%d", i/250, i%250)
		tok, err := c.put("login", client, &webauthn.SessionData{})
		if err != nil {
			t.Fatal(err)
		}
		if i == maxCeremonies+49 {
			admin = tok
		}
	}
	if _, ok := c.take("login", admin); !ok {
		t.Fatal("the newest ceremony must survive a flood")
	}
	for range 20 {
		c.put("login", "6.6.6.6", &webauthn.SessionData{})
	}
	n := 0
	for _, v := range c.m {
		if v.client == "6.6.6.6" {
			n++
		}
	}
	if n > maxCeremoniesPerClient {
		t.Fatalf("one client holds %d ceremonies", n)
	}
}

func TestSaveRejectsDuplicateDomain(t *testing.T) {
	_, _, e := newTestServerWithEngine(t)
	if err := e.CreateApp(&appdef.App{Name: "zeta", Source: "github:me/z", Domains: []string{"x.example.com"}}); err != nil {
		t.Fatal(err)
	}
	err := e.CreateApp(&appdef.App{Name: "alpha", Source: "github:me/a", Domains: []string{"X.example.com"}})
	if err == nil || !strings.Contains(err.Error(), "already used by zeta") {
		t.Fatalf("got %v", err)
	}
}
