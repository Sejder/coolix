package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "coolix_session"
	sessionTTL    = 7 * 24 * time.Hour
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

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, s.SessionKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) newSession() *http.Cookie {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    exp + "." + s.sign(exp),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.SecureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	}
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(exp))) {
		return false
	}
	t, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && time.Now().Unix() < t
}

func (s *Server) checkPassword(given string) error {
	want, err := os.ReadFile(s.PasswordFile)
	if err != nil {
		return fmt.Errorf("reading admin password: %w", err)
	}
	w := strings.TrimRight(string(want), "\r\n")
	if w == "" || subtle.ConstantTimeCompare([]byte(given), []byte(w)) != 1 {
		return errors.New("wrong password")
	}
	return nil
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
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if !s.validSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.render(w, r, "login", map[string]any{"Title": "Sign in"})
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := s.checkPassword(r.FormValue("password")); err != nil {
		time.Sleep(time.Second) // slow down guessing
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", map[string]any{"Title": "Sign in", "Error": "Wrong password."})
		return
	}
	http.SetCookie(w, s.newSession())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
