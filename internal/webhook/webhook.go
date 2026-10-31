// Package webhook authenticates deploy webhooks from git forges. Each app has
// its own random token, used as the HMAC secret by GitHub, Gitea and Forgejo,
// as the X-Gitlab-Token by GitLab, or as a bearer token by anything else
// (e.g. a CI job running curl).
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type Delivery struct {
	Provider string // github, gitea, forgejo, gitlab, bearer
	Event    string // as reported by the forge
	Ref      string // pushed ref, e.g. refs/heads/main; "" if unknown
}

// Push reports whether the delivery is a push (the only event that deploys).
func (d *Delivery) Push() bool {
	switch d.Event {
	case "push", "Push Hook":
		return true
	}
	return false
}

// Ping reports whether the delivery is a forge's connectivity test.
func (d *Delivery) Ping() bool { return d.Event == "ping" }

var ErrUnauthorized = errors.New("missing or invalid webhook signature")

func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validMAC(body []byte, token, sigHex string) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// Verify authenticates a delivery and extracts what coolix needs from it.
func Verify(h http.Header, body []byte, token string) (*Delivery, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	d := &Delivery{}
	switch {
	case h.Get("X-Forgejo-Signature") != "":
		d.Provider, d.Event = "forgejo", h.Get("X-Forgejo-Event")
		if !validMAC(body, token, h.Get("X-Forgejo-Signature")) {
			return nil, ErrUnauthorized
		}
	case h.Get("X-Gitea-Signature") != "":
		d.Provider, d.Event = "gitea", h.Get("X-Gitea-Event")
		if !validMAC(body, token, h.Get("X-Gitea-Signature")) {
			return nil, ErrUnauthorized
		}
	case h.Get("X-Hub-Signature-256") != "":
		d.Provider, d.Event = "github", h.Get("X-GitHub-Event")
		sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		if !ok || !validMAC(body, token, sig) {
			return nil, ErrUnauthorized
		}
	case h.Get("X-Gitlab-Token") != "":
		d.Provider, d.Event = "gitlab", h.Get("X-Gitlab-Event")
		if !equal(h.Get("X-Gitlab-Token"), token) {
			return nil, ErrUnauthorized
		}
	default:
		bearer, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
		if !ok || !equal(bearer, token) {
			return nil, ErrUnauthorized
		}
		d.Provider, d.Event = "bearer", "push"
	}
	d.Ref = ref(h, body)
	return d, nil
}

// ref extracts the pushed ref from a JSON body, or from the form-encoded
// "payload" field GitHub sends when the hook's content type is
// application/x-www-form-urlencoded.
func ref(h http.Header, body []byte) string {
	if strings.HasPrefix(h.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if form, err := url.ParseQuery(string(body)); err == nil {
			body = []byte(form.Get("payload"))
		}
	}
	var payload struct {
		Ref string `json:"ref"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return payload.Ref
}

// Matches reports whether a delivery should deploy an app that tracks
// branch ("" tracks every branch). With a branch set, a delivery whose ref
// is unknown doesn't match.
func (d *Delivery) Matches(branch string) bool {
	return branch == "" || d.Ref == "refs/heads/"+branch
}
