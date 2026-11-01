package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
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

func TestFormPayloadAndUnknownRef(t *testing.T) {
	body := "payload=" + url.QueryEscape(`{"ref":"refs/heads/dev"}`)
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("X-GitHub-Event", "push")
	h.Set("X-Hub-Signature-256", "sha256="+sign(body))
	d, err := Verify(h, []byte(body), token)
	if err != nil {
		t.Fatal(err)
	}
	if d.Ref != "refs/heads/dev" || d.Matches("main") {
		t.Fatalf("form payload: %+v", d)
	}
	if (&Delivery{}).Matches("main") {
		t.Fatal("unknown ref must not match a branch filter")
	}
}

func TestPullRequests(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	gh := func(action, headRepo string) string {
		head := `null`
		if headRepo != "" {
			head = `{"full_name":"` + headRepo + `"}`
		}
		return `{"action":"` + action + `","number":7,"pull_request":{"head":{"ref":"feat","sha":"` + sha + `","repo":` + head + `},"base":{"repo":{"full_name":"me/app"}}}}`
	}
	gl := func(action string, src int) string {
		return `{"object_attributes":{"iid":3,"action":"` + action + `","source_branch":"feat","source_project_id":` +
			string(rune('0'+src)) + `,"target_project_id":1,"last_commit":{"id":"` + sha + `"}}}`
	}
	for _, c := range []struct {
		name    string
		headers map[string]string
		body    string
		want    PullRequest
	}{
		{"github opened", map[string]string{"X-GitHub-Event": "pull_request"}, gh("opened", "me/app"), PullRequest{"update", 7, "feat", sha, false}},
		{"github synchronize", map[string]string{"X-GitHub-Event": "pull_request"}, gh("synchronize", "Me/App"), PullRequest{"update", 7, "feat", sha, false}},
		{"github fork", map[string]string{"X-GitHub-Event": "pull_request"}, gh("opened", "evil/app"), PullRequest{"update", 7, "feat", sha, true}},
		{"github deleted fork", map[string]string{"X-GitHub-Event": "pull_request"}, gh("opened", ""), PullRequest{"update", 7, "feat", sha, true}},
		{"github closed", map[string]string{"X-GitHub-Event": "pull_request"}, gh("closed", "me/app"), PullRequest{"close", 7, "feat", sha, false}},
		{"github labeled", map[string]string{"X-GitHub-Event": "pull_request"}, gh("labeled", "me/app"), PullRequest{"", 7, "feat", sha, false}},
		{"forgejo", map[string]string{"X-Forgejo-Event": "pull_request"}, gh("synchronized", "me/app"), PullRequest{"update", 7, "feat", sha, false}},
		{"gitlab open", map[string]string{"X-Gitlab-Event": "Merge Request Hook"}, gl("open", 1), PullRequest{"update", 3, "feat", sha, false}},
		{"gitlab merge", map[string]string{"X-Gitlab-Event": "Merge Request Hook"}, gl("merge", 1), PullRequest{"close", 3, "feat", sha, false}},
		{"gitlab fork", map[string]string{"X-Gitlab-Event": "Merge Request Hook"}, gl("update", 2), PullRequest{"update", 3, "feat", sha, true}},
	} {
		h := http.Header{}
		for k, v := range c.headers {
			h.Set(k, v)
		}
		switch {
		case h.Get("X-GitHub-Event") != "":
			h.Set("X-Hub-Signature-256", "sha256="+sign(c.body))
		case h.Get("X-Forgejo-Event") != "":
			h.Set("X-Forgejo-Signature", sign(c.body))
		default:
			h.Set("X-Gitlab-Token", token)
		}
		d, err := Verify(h, []byte(c.body), token)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d.PR == nil || *d.PR != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, d.PR, c.want)
		}
		if d.Push() {
			t.Errorf("%s: a pull request is not a push", c.name)
		}
	}
}
