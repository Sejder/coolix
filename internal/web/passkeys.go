package web

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Passkeys (WebAuthn). The relying party ID is the host of the page's
// origin, which must be one of the configured origins (the UI's domain, and
// http://localhost:<port> for SSH tunnels).

const ceremonyTTL = 5 * time.Minute

type ceremony struct {
	kind    string // register | login
	client  string
	session webauthn.SessionData
	expires time.Time
}

type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

// Pending ceremonies per client and in total. When full, the oldest goes:
// a flood can delay a sign-in, but not lock the admin out.
const (
	maxCeremoniesPerClient = 5
	maxCeremonies          = 1000
)

func (c *ceremonies) put(kind, client string, s *webauthn.SessionData) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ceremony{}
	}
	now := time.Now()
	perClient := 0
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		} else if v.client == client {
			perClient++
		}
	}
	for perClient >= maxCeremoniesPerClient || len(c.m) >= maxCeremonies {
		oldest := ""
		for k, v := range c.m {
			if (perClient < maxCeremoniesPerClient || v.client == client) &&
				(oldest == "" || v.expires.Before(c.m[oldest].expires)) {
				oldest = k
			}
		}
		if c.m[oldest].client == client {
			perClient--
		}
		delete(c.m, oldest)
	}
	c.m[token] = ceremony{kind: kind, client: client, session: *s, expires: now.Add(ceremonyTTL)}
	return token, nil
}

func (c *ceremonies) take(kind, token string) (webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[token]
	delete(c.m, token)
	if !ok || cer.kind != kind || time.Now().After(cer.expires) {
		return webauthn.SessionData{}, false
	}
	return cer.session, true
}

func (s *Server) initWebAuthn() error {
	if len(s.WebAuthnOrigins) == 0 {
		return nil
	}
	u, err := url.Parse(s.WebAuthnOrigins[0])
	if err != nil {
		return err
	}
	s.webauthn, err = webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "coolix",
		RPOrigins:     s.WebAuthnOrigins,
	})
	return err
}

// rp returns the relying party ID and origin for a passkey request, from
// its Origin header, which must be a configured origin.
func (s *Server) rp(r *http.Request) (id, origin string, err error) {
	if s.webauthn == nil {
		return "", "", errors.New("passkeys are not configured (services.coolix.ui.domain)")
	}
	origin = r.Header.Get("Origin")
	if !slices.Contains(s.WebAuthnOrigins, origin) {
		return "", "", errors.New("passkeys work only on " + strings.Join(s.WebAuthnOrigins, " or "))
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), origin, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// handlePasskeyRegisterBegin starts adding a passkey to the account.
func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.credentialChangeAllowed(r) {
		jsonError(w, http.StatusForbidden, errors.New(errNotRecent))
		return
	}
	id, origin, err := s.rp(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	a, err := s.Accounts.Update(func(*Account) error { return nil }) // ensures a user ID
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, p := range a.Passkeys {
		exclude = append(exclude, p.Credential.Descriptor())
	}
	creation, session, err := s.webauthn.BeginRegistration(accountUser{a},
		webauthn.WithRegistrationRelyingPartyID(id),
		webauthn.WithRegistrationOrigin(origin),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(exclude))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	token, err := s.ceremonies.put("register", clientIP(r), session)
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "options": creation.Response})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.rp(r); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	session, ok := s.ceremonies.take("register", r.URL.Query().Get("token"))
	if !ok {
		jsonError(w, http.StatusBadRequest, errors.New("passkey request expired; try again"))
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" || len(name) > 64 {
		name = "Passkey"
	}
	body, err := readBody(w, r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.Accounts.Update(func(a *Account) error {
		cred, err := s.webauthn.FinishRegistration(accountUser{a}, session, withBody(r, body))
		if err != nil {
			return err
		}
		if !a.HasCredential() {
			// The first credential claims the account: void every session
			// from before (e.g. one opened with a leaked setup code).
			a.Epoch++
		}
		a.Passkeys = append(a.Passkeys, Passkey{Name: name, Created: time.Now(), Credential: *cred})
		return nil
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, protocolError(err))
		return
	}
	clearSetupCode(s.Config.StateDir)
	cookie, err := s.newSession()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, cookie)
	s.setFlash(w, "Passkey added.")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "redirect": "/account"})
}

// handlePasskeyLoginBegin starts a usernameless (discoverable) login.
func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, errors.New("cross-origin request rejected"))
		return
	}
	id, origin, err := s.rp(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	assertion, session, err := s.webauthn.BeginDiscoverableLogin(
		webauthn.WithLoginRelyingPartyID(id),
		webauthn.WithLoginOrigin(origin),
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	token, err := s.ceremonies.put("login", clientIP(r), session)
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "options": assertion.Response})
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, errors.New("cross-origin request rejected"))
		return
	}
	if _, _, err := s.rp(r); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	ip := clientIP(r)
	if s.logins.blocked(ip, time.Now()) {
		jsonError(w, http.StatusTooManyRequests, errors.New("too many failed attempts; try again later"))
		return
	}
	session, ok := s.ceremonies.take("login", r.URL.Query().Get("token"))
	if !ok {
		jsonError(w, http.StatusBadRequest, errors.New("passkey request expired; try again"))
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.Accounts.Update(func(a *Account) error {
		handler := func(rawID, userHandle []byte) (webauthn.User, error) {
			if len(a.UserID) == 0 || !bytes.Equal(userHandle, a.UserID) {
				return nil, errors.New("unknown passkey")
			}
			return accountUser{a}, nil
		}
		_, cred, err := s.webauthn.FinishPasskeyLogin(handler, session, withBody(r, body))
		if err != nil {
			return err
		}
		if cred.Authenticator.CloneWarning {
			return errors.New("this passkey's signature counter went backwards; it may have been cloned. Remove it and add a new one")
		}
		for i := range a.Passkeys {
			if bytes.Equal(a.Passkeys[i].Credential.ID, cred.ID) {
				a.Passkeys[i].Credential.Authenticator = cred.Authenticator // sign count
				a.Passkeys[i].Credential.Flags = cred.Flags
				a.Passkeys[i].LastUsed = time.Now()
				return nil
			}
		}
		return errors.New("unknown passkey")
	})
	if err != nil {
		s.logins.fail(ip, time.Now())
		jsonError(w, http.StatusUnauthorized, protocolError(err))
		return
	}
	cookie, err := s.newSession()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, cookie)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "redirect": "/"})
}

// protocolError unwraps the detail go-webauthn keeps in DevInfo.
func protocolError(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		return errors.New(pe.Details + ": " + pe.DevInfo)
	}
	return err
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	if _, err := b.ReadFrom(http.MaxBytesReader(w, r.Body, 64<<10)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// withBody gives go-webauthn a request whose body can be read again (the
// account update may retry).
func withBody(r *http.Request, body []byte) *http.Request {
	r2 := r.Clone(r.Context())
	r2.Body = http.NoBody
	if len(body) > 0 {
		r2.Body = readCloser{bytes.NewReader(body)}
	}
	return r2
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }
