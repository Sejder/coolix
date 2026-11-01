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
	// PR is set for pull/merge request events.
	PR *PullRequest
}

// PullRequest is what previews need from a pull (or merge) request event.
type PullRequest struct {
	// Action is "update" (opened, reopened or new commits), "close" (closed
	// or merged) or "" (anything else, e.g. a label change).
	Action string
	Number int
	Branch string // the head branch
	Rev    string // the head commit
	// Fork: the head branch lives in another repository, so the code is
	// from someone without push access. Never deployed.
	Fork bool
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
	d.PR = pullRequest(d, payload(h, body))
	return d, nil
}

func payload(h http.Header, body []byte) []byte {
	if strings.HasPrefix(h.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if form, err := url.ParseQuery(string(body)); err == nil {
			return []byte(form.Get("payload"))
		}
	}
	return body
}

// pullRequest parses GitHub, Gitea and Forgejo pull_request events and
// GitLab merge request events.
func pullRequest(d *Delivery, body []byte) *PullRequest {
	switch {
	case d.Event == "pull_request" && d.Provider != "gitlab" && d.Provider != "bearer":
		var p struct {
			Action      string `json:"action"`
			Number      int    `json:"number"`
			PullRequest struct {
				Head struct {
					Ref  string `json:"ref"`
					Sha  string `json:"sha"`
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"head"`
				Base struct {
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"base"`
			} `json:"pull_request"`
		}
		if json.Unmarshal(body, &p) != nil || p.Number <= 0 {
			return nil
		}
		head, base := p.PullRequest.Head, p.PullRequest.Base
		pr := &PullRequest{
			Number: p.Number, Branch: head.Ref, Rev: head.Sha,
			Fork: head.Repo == nil || base.Repo == nil || !strings.EqualFold(head.Repo.FullName, base.Repo.FullName),
		}
		switch p.Action {
		case "opened", "reopened", "synchronize", "synchronized":
			pr.Action = "update"
		case "closed":
			pr.Action = "close"
		}
		return pr
	case d.Event == "Merge Request Hook":
		var p struct {
			Attrs struct {
				IID             int    `json:"iid"`
				Action          string `json:"action"`
				SourceBranch    string `json:"source_branch"`
				SourceProjectID int    `json:"source_project_id"`
				TargetProjectID int    `json:"target_project_id"`
				LastCommit      struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
		}
		if json.Unmarshal(body, &p) != nil || p.Attrs.IID <= 0 {
			return nil
		}
		a := p.Attrs
		pr := &PullRequest{
			Number: a.IID, Branch: a.SourceBranch, Rev: a.LastCommit.ID,
			Fork: a.SourceProjectID != a.TargetProjectID,
		}
		switch a.Action {
		case "open", "reopen", "update":
			pr.Action = "update"
		case "close", "merge":
			pr.Action = "close"
		}
		return pr
	}
	return nil
}

// ref extracts the pushed ref from a JSON body, or from the form-encoded
// "payload" field GitHub sends when the hook's content type is
// application/x-www-form-urlencoded.
func ref(h http.Header, body []byte) string {
	body = payload(h, body)
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
