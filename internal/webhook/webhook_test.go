package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

const token = "s3cret"

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerify(t *testing.T) {
	body := `{"ref":"refs/heads/main"}`
	cases := []struct {
		name    string
		headers map[string]string
		ok      bool
		push    bool
	}{
		{"github", map[string]string{"X-Hub-Signature-256": "sha256=" + sign(body), "X-GitHub-Event": "push"}, true, true},
		{"github ping", map[string]string{"X-Hub-Signature-256": "sha256=" + sign(body), "X-GitHub-Event": "ping"}, true, false},
		{"github bad sig", map[string]string{"X-Hub-Signature-256": "sha256=" + sign("other"), "X-GitHub-Event": "push"}, false, false},
		{"github no prefix", map[string]string{"X-Hub-Signature-256": sign(body), "X-GitHub-Event": "push"}, false, false},
		{"gitea", map[string]string{"X-Gitea-Signature": sign(body), "X-Gitea-Event": "push"}, true, true},
		{"forgejo", map[string]string{"X-Forgejo-Signature": sign(body), "X-Forgejo-Event": "push"}, true, true},
		{"forgejo bad", map[string]string{"X-Forgejo-Signature": "zz", "X-Forgejo-Event": "push"}, false, false},
		{"gitlab", map[string]string{"X-Gitlab-Token": token, "X-Gitlab-Event": "Push Hook"}, true, true},
		{"gitlab bad", map[string]string{"X-Gitlab-Token": "nope", "X-Gitlab-Event": "Push Hook"}, false, false},
		{"bearer", map[string]string{"Authorization": "Bearer " + token}, true, true},
		{"bearer bad", map[string]string{"Authorization": "Bearer x"}, false, false},
		{"nothing", map[string]string{}, false, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		d, err := Verify(h, []byte(body), token)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if c.ok && (d.Push() != c.push || d.Ref != "refs/heads/main") {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	if _, err := Verify(http.Header{"Authorization": {"Bearer "}}, nil, ""); err == nil {
		t.Error("empty token accepted")
	}
}

func TestMatches(t *testing.T) {
	d := &Delivery{Ref: "refs/heads/main"}
	if !d.Matches("") || !d.Matches("main") || d.Matches("prod") {
		t.Error("branch filter")
	}
}
