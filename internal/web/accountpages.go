package web

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

// handleSetup claims a fresh install: the setup code proves shell access to
// the host. Afterwards the admin adds a passkey and/or a password.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.claimed() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data := map[string]any{"Title": "Set up coolix", "Passkeys": s.webauthn != nil, "MinPassword": MinPasswordLength}
	if r.Method == http.MethodGet {
		s.render(w, r, "setup", data)
		return
	}
	if !sameOrigin(r) {
		s.errorPage(w, r, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	fail := func(status int, msg string) {
		data["Error"] = msg
		s.renderStatus(w, r, status, "setup", data)
	}
	ip := clientIP(r)
	if s.logins.blocked(ip, time.Now()) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	// Check the password first, so a typo doesn't use up the code.
	pw := r.FormValue("password")
	if pw != "" {
		if err := checkNewPassword(pw, r.FormValue("confirm")); err != nil {
			fail(http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if !checkSetupCode(s.Config.StateDir, r.FormValue("code")) {
		s.logins.fail(ip, time.Now())
		time.Sleep(time.Second)
		fail(http.StatusUnauthorized, "Wrong or expired setup code. Run `sudo coolix setup-code` on the server for a new one.")
		return
	}
	clearSetupCode(s.Config.StateDir) // single use
	if pw != "" {
		hash, err := HashPassword(pw)
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		if _, err := s.Accounts.Update(func(a *Account) error { a.PasswordHash = hash; return nil }); err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		clearSetupCode(s.Config.StateDir)
		s.signIn(w, r, "/account", "Account created. You can add a passkey here.")
		return
	}
	// No password: signed in by the setup code, which stays valid until a
	// passkey is added.
	if _, err := s.Accounts.Update(func(*Account) error { return nil }); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.signIn(w, r, "/account", "Now add a passkey to finish setting up your account.")
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.Accounts.Get()
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	keys, err := s.Config.SSHKeys().List()
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.render(w, r, "account", map[string]any{
		"Title":        "Account",
		"Account":      a,
		"SSHKeys":      keys,
		"GitHost":      s.Config.GitHost,
		"PasswordFile": s.PasswordFile != "",
		"Passkeys":     s.webauthn != nil,
		"Origins":      strings.Join(s.WebAuthnOrigins, ", "),
		"MinPassword":  MinPasswordLength,
	})
}

// credentialsAfter counts the ways to sign in that remain after a change.
func (s *Server) credentialsAfter(a *Account) int {
	n := len(a.Passkeys)
	if a.PasswordHash != "" {
		n++
	}
	if s.PasswordFile != "" {
		n++
	}
	return n
}

var errLastCredential = errors.New("that's the only way left to sign in; add a passkey or password first")

func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.credentialChangeAllowed(r) {
		s.redirect(w, r, "/account", errNotRecent)
		return
	}
	pw := r.FormValue("password")
	if err := checkNewPassword(pw, r.FormValue("confirm")); err != nil {
		s.redirect(w, r, "/account", err.Error())
		return
	}
	hash, err := HashPassword(pw)
	if err == nil {
		_, err = s.Accounts.Update(func(a *Account) error {
			a.PasswordHash = hash
			a.Epoch++ // sign out other sessions
			return nil
		})
	}
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	clearSetupCode(s.Config.StateDir)
	s.signIn(w, r, "/account", "Password saved. Other sessions were signed out.")
}

func (s *Server) handleRemovePassword(w http.ResponseWriter, r *http.Request) {
	if !s.credentialChangeAllowed(r) {
		s.redirect(w, r, "/account", errNotRecent)
		return
	}
	_, err := s.Accounts.Update(func(a *Account) error {
		if a.PasswordHash == "" {
			return nil
		}
		a.PasswordHash = ""
		if s.credentialsAfter(a) == 0 {
			return errLastCredential
		}
		a.Epoch++
		return nil
	})
	if err != nil {
		s.redirect(w, r, "/account", err.Error())
		return
	}
	s.signIn(w, r, "/account", "Password removed.")
}

func (s *Server) handleRemovePasskey(w http.ResponseWriter, r *http.Request) {
	if !s.credentialChangeAllowed(r) {
		s.redirect(w, r, "/account", errNotRecent)
		return
	}
	id := r.PathValue("id")
	_, err := s.Accounts.Update(func(a *Account) error {
		i := slices.IndexFunc(a.Passkeys, func(p Passkey) bool { return p.ID() == id })
		if i < 0 {
			return errors.New("no such passkey")
		}
		a.Passkeys = slices.Delete(a.Passkeys, i, i+1)
		if s.credentialsAfter(a) == 0 {
			return errLastCredential
		}
		a.Epoch++
		return nil
	})
	if err != nil {
		s.redirect(w, r, "/account", err.Error())
		return
	}
	s.signIn(w, r, "/account", "Passkey removed. Other sessions were signed out.")
}

// credentialChangeAllowed requires a fresh sign-in to change credentials,
// except while setting up an account that has none yet.
func (s *Server) credentialChangeAllowed(r *http.Request) bool {
	a, err := s.Accounts.Get()
	if err != nil {
		return false
	}
	return !a.HasCredential() || s.recentAuth(r)
}

const errNotRecent = "For your security, sign out and back in (within 15 minutes) to change how you sign in."

// SSH keys may push (deploy) code, so like credentials they need a recent
// sign-in: a stolen session can't leave a key behind.
func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	if !s.credentialChangeAllowed(r) {
		s.redirect(w, r, "/account", errNotRecent)
		return
	}
	k, err := s.Config.SSHKeys().Add(r.FormValue("name"), r.FormValue("key"))
	if err != nil {
		s.redirect(w, r, "/account", "SSH key not added: "+err.Error())
		return
	}
	s.redirect(w, r, "/account", "SSH key "+k.Name+" added.")
}

func (s *Server) handleRemoveSSHKey(w http.ResponseWriter, r *http.Request) {
	if err := s.Config.SSHKeys().Remove(r.PathValue("name")); err != nil {
		s.redirect(w, r, "/account", err.Error())
		return
	}
	s.redirect(w, r, "/account", "SSH key removed.")
}
