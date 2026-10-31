package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "coolix_session"
	sessionTTL        = 7 * 24 * time.Hour

	// MinPasswordLength is enforced at startup.
	MinPasswordLength = 12

	// After maxFailures failed logins from one client within failureWindow,
	// it is refused without checking until the window has passed.
	maxFailures   = 10
	failureWindow = 15 * time.Minute
)

// LoadSessionKey reads (or creates) the HMAC key that signs session cookies.
func LoadSessionKey(path string) ([]byte, error) {
	if key, err := os.ReadFile(path); err == nil && len(key) >= 32 {
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}

func readPassword(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading admin password: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// CheckPasswordFile refuses to start with an unreadable or short declarative
// admin password (services.coolix.ui.adminPasswordFile).
func CheckPasswordFile(path string) error {
	pw, err := readPassword(path)
	if err != nil {
		return err
	}
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("the admin password must be at least %d characters", MinPasswordLength)
	}
	return nil
}

// signingKey binds sessions to the account's credentials: changing the
// password (file or account), or removing a passkey, signs everyone out.
func (s *Server) signingKey() ([]byte, error) {
	a, err := s.Accounts.Get()
	if err != nil {
		return nil, err
	}
	var pw string
	if s.PasswordFile != "" {
		if pw, err = readPassword(s.PasswordFile); err != nil {
			return nil, err
		}
	}
	m := hmac.New(sha256.New, s.SessionKey)
	fmt.Fprintf(m, "%x\x00%d\x00%s\x00%s", a.Salt, a.Epoch, a.PasswordHash, pw)
	return m.Sum(nil), nil
}

func (s *Server) sign(payload string) (string, error) {
	key, err := s.signingKey()
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

func (s *Server) newSession() (*http.Cookie, error) {
	now := time.Now()
	payload := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10) + "." + strconv.FormatInt(now.UnixNano(), 10)
	sig, err := s.sign(payload)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     s.sessionCookie(),
		Value:    payload + "." + sig,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.SecureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	}, nil
}

// notBeforePath records the last logout: sessions issued earlier are void.
func (s *Server) notBeforePath() string {
	return filepath.Join(s.Config.StateDir, "sessions-not-before")
}

func (s *Server) notBefore() int64 {
	data, err := os.ReadFile(s.notBeforePath())
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(s.sessionCookie())
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(c.Value, '.')
	if i < 0 {
		return false
	}
	payload, sig := c.Value[:i], c.Value[i+1:]
	want, err := s.sign(payload)
	if err != nil || !hmac.Equal([]byte(sig), []byte(want)) {
		return false
	}
	expS, issuedS, ok := strings.Cut(payload, ".")
	if !ok {
		return false
	}
	exp, err1 := strconv.ParseInt(expS, 10, 64)
	issued, err2 := strconv.ParseInt(issuedS, 10, 64)
	return err1 == nil && err2 == nil && time.Now().Unix() < exp && issued > s.notBefore()
}

// checkPassword accepts the declarative password file or the account's
// password.
func (s *Server) checkPassword(given string) error {
	if s.PasswordFile != "" {
		want, err := readPassword(s.PasswordFile)
		if err != nil {
			return err
		}
		if want != "" && subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1 {
			return nil
		}
	}
	a, err := s.Accounts.Get()
	if err != nil {
		return err
	}
	if a.PasswordHash != "" && VerifyPassword(a.PasswordHash, given) {
		return nil
	}
	return errors.New("wrong password")
}

// claimed reports whether the admin account has a way to sign in. Until
// then the UI only offers the setup page.
func (s *Server) claimed() bool {
	if s.PasswordFile != "" {
		return true
	}
	a, err := s.Accounts.Get()
	return err != nil || a.HasCredential() // on errors, fail closed
}

type ctxKey int

const viaSocket ctxKey = 0

// ConnContext marks requests that came in over the unix socket, which only
// Caddy (and root) can connect to. Only their X-Forwarded-For is trusted:
// apps share the host network and could otherwise forge it over TCP.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, viaSocket, c.LocalAddr().Network() == "unix")
}

// clientIP is the address failed logins are counted against: the address
// Caddy saw (last X-Forwarded-For entry) for requests through the socket,
// the peer otherwise. IPv6 addresses count per /64.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if trusted, _ := r.Context().Value(viaSocket).(bool); trusted {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			host = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	if host == "" || host == "@" {
		return "local"
	}
	return host
}

// loginLimiter locks a client out after too many failures, and bounds how
// many (memory-hungry, argon2id) password checks run at once. Clients don't
// queue behind each other's failures: the 1s penalty is per request.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	slots    chan struct{}
	once     sync.Once
}

const maxConcurrentChecks = 4

// acquire takes a check slot, waiting at most 5s.
func (l *loginLimiter) acquire() bool {
	l.once.Do(func() { l.slots = make(chan struct{}, maxConcurrentChecks) })
	select {
	case l.slots <- struct{}{}:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func (l *loginLimiter) release() { <-l.slots }

func (l *loginLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.failures[ip][:0]
	for _, t := range l.failures[ip] {
		if now.Sub(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = recent
	}
	return len(recent) >= maxFailures
}

func (l *loginLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	if len(l.failures) > 10000 { // bound memory: forget expired clients first
		for k, ts := range l.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= failureWindow {
				delete(l.failures, k)
			}
		}
		for k := range l.failures { // still too many: forget arbitrary ones
			if len(l.failures) <= 5000 {
				break
			}
			delete(l.failures, k)
		}
	}
	l.failures[ip] = append(l.failures[ip], now)
}

// sameOrigin rejects cross-site form posts (defense in depth next to the
// SameSite=Strict cookie).
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !sameOrigin(r) {
			s.errorPage(w, r, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		if !s.validSession(r) {
			// htmx requests (polling, boosted links) get a full-page redirect
			// instead of a login page swapped into a fragment.
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !s.claimed() {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.claimed() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, r, "login", map[string]any{"Title": "Sign in", "Passkeys": s.webauthn != nil})
		return
	}
	if !sameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	ip := clientIP(r)
	if s.logins.blocked(ip, time.Now()) {
		s.renderStatus(w, r, http.StatusTooManyRequests, "login", map[string]any{"Title": "Sign in", "Passkeys": s.webauthn != nil, "Error": "Too many failed attempts. Try again later."})
		return
	}
	if !s.logins.acquire() {
		s.renderStatus(w, r, http.StatusTooManyRequests, "login", map[string]any{"Title": "Sign in", "Passkeys": s.webauthn != nil, "Error": "Too busy. Try again in a moment."})
		return
	}
	err := s.checkPassword(r.FormValue("password"))
	s.logins.release()
	if err != nil {
		s.logins.fail(ip, time.Now())
		time.Sleep(time.Second) // slow down guessing
	}
	if err != nil {
		s.renderStatus(w, r, http.StatusUnauthorized, "login", map[string]any{"Title": "Sign in", "Passkeys": s.webauthn != nil, "Error": "Wrong password."})
		return
	}
	s.signIn(w, r, "/", "")
}

// handleLogout signs out every session (there is one admin).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(s.notBeforePath(), []byte(now+"\n"), 0o600); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Value: "", Path: "/", MaxAge: -1, Secure: s.SecureCookies})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// signIn issues a session cookie and redirects to target with a flash.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, target, msg string) {
	s.setFlash(w, msg)
	cookie, err := s.newSession()
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// sessionCookie uses the __Host- prefix over HTTPS, so an app on a sibling
// subdomain can't plant a cookie that shadows it.
func (s *Server) sessionCookie() string {
	if s.SecureCookies {
		return "__Host-" + sessionCookieName
	}
	return sessionCookieName
}

// recentAuth reports whether the session was issued in the last 15
// minutes. Changing how one signs in requires it, so a stolen session can't
// add its own passkey.
func (s *Server) recentAuth(r *http.Request) bool {
	c, err := r.Cookie(s.sessionCookie())
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return false
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && time.Since(time.Unix(0, issued)) < 15*time.Minute
}
