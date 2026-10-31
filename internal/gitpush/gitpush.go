// Package gitpush hosts the bare repos of push apps (source = "push"): the
// code `git push coolix@host:APP` sends, which the app's flake input then
// points at. Pushes arrive over SSH as the coolix user (sshd's ForceCommand
// runs `coolix ssh-serve`).
package gitpush

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"coolix/internal/units"
)

// Dir is the bare repo of an app.
func Dir(gitDir, app string) string { return filepath.Join(gitDir, app+".git") }

// FlakeURL is the flake reference of branch in the bare repo dir.
func FlakeURL(dir, branch string) string {
	return "git+file://" + dir + "?ref=refs/heads/" + branch
}

// PinPrefix keeps every pushed commit reachable: flake.lock and older
// generations refer to them even after a force push.
const PinPrefix = "refs/coolix/pushed/"

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--git-dir", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, ee.Stderr)
		}
		return "", err
	}
	return string(out), nil
}

// Init creates the bare repo if it doesn't exist yet.
func Init(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w\n%s", err, out)
	}
	for _, kv := range [][2]string{
		// Pushers can't see or move coolix's own refs.
		{"transfer.hideRefs", "refs/coolix"},
		{"receive.fsckObjects", "true"},
	} {
		if _, err := git(dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// HasBranch reports whether branch exists in the bare repo dir.
func HasBranch(dir, branch string) bool {
	_, err := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	return err == nil
}

// Branches maps the branches of dir to their commits.
func Branches(dir string) (map[string]string, error) {
	out, err := git(dir, "for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, rev, ok := strings.Cut(line, " "); ok {
			m[name] = rev
		}
	}
	return m, nil
}

// Update is a branch a push changed; Rev is "" when it was deleted.
type Update struct {
	Branch, Rev string
}

// Changes lists the branches that differ between two Branches results.
func Changes(before, after map[string]string) []Update {
	var out []Update
	for b, rev := range after {
		if before[b] != rev {
			out = append(out, Update{b, rev})
		}
	}
	for b := range before {
		if _, ok := after[b]; !ok {
			out = append(out, Update{b, ""})
		}
	}
	return out
}

// Pin keeps rev reachable (see PinPrefix).
func Pin(dir, rev string) error {
	_, err := git(dir, "update-ref", PinPrefix+rev, rev)
	return err
}

// Command is a git request from an SSH client (SSH_ORIGINAL_COMMAND).
type Command struct {
	Service string // git-receive-pack (push) or git-upload-pack (fetch)
	App     string
}

// ParseCommand accepts what git sends for the remotes coolix@host:APP,
// coolix@host:APP.git and ssh://coolix@host/APP.
func ParseCommand(s string) (Command, error) {
	bad := fmt.Errorf("coolix only accepts git push and fetch (e.g. git push coolix@host:APP main), not %q", s)
	service, path, ok := strings.Cut(strings.TrimSpace(s), " ")
	if !ok {
		return Command{}, bad
	}
	switch service {
	case "git-receive-pack", "git-upload-pack":
	default:
		return Command{}, bad
	}
	path = strings.TrimSpace(path)
	if len(path) >= 2 && path[0] == '\'' && path[len(path)-1] == '\'' {
		path = path[1 : len(path)-1]
	}
	path = strings.TrimPrefix(strings.TrimPrefix(path, "~/"), "/")
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if !units.ValidName(path) {
		return Command{}, fmt.Errorf("%q is not an app name", path)
	}
	return Command{Service: service, App: path}, nil
}
