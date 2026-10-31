package gitpush

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseCommand(t *testing.T) {
	for in, want := range map[string]Command{
		"git-receive-pack 'blog'":       {"git-receive-pack", "blog"},
		"git-receive-pack '/blog.git'":  {"git-receive-pack", "blog"},
		"git-upload-pack '~/blog.git/'": {"git-upload-pack", "blog"},
		"git-receive-pack blog":         {"git-receive-pack", "blog"},
	} {
		got, err := ParseCommand(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "sh", "git-receive-pack", "git-upload-archive 'blog'", "git-receive-pack '../etc'",
		"git-receive-pack 'a/b'", "git-receive-pack 'Blog'", "bash -c 'git-receive-pack blog'",
	} {
		if _, err := ParseCommand(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

func TestBareRepo(t *testing.T) {
	gitDir := t.TempDir()
	dir := Dir(gitDir, "blog")
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir); err != nil {
		t.Fatal("second init:", err)
	}
	if HasBranch(dir, "main") {
		t.Fatal("empty repo has main")
	}
	before, err := Branches(dir)
	if err != nil || len(before) != 0 {
		t.Fatalf("branches of an empty repo: %v %v", before, err)
	}

	work := filepath.Join(t.TempDir(), "work")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-C", work}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", work).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	run("commit", "-q", "--allow-empty", "-m", "one")
	run("push", "-q", dir, "main", "main:feature")

	after, err := Branches(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !HasBranch(dir, "main") || len(after) != 2 {
		t.Fatalf("after push: %v", after)
	}
	if c := Changes(before, after); len(c) != 2 {
		t.Fatalf("changes: %v", c)
	}
	if err := Pin(dir, after["main"]); err != nil {
		t.Fatal(err)
	}
	run("push", "-q", dir, ":feature")
	gone, _ := Branches(dir)
	if c := Changes(after, gone); len(c) != 1 || c[0] != (Update{"feature", ""}) {
		t.Fatalf("delete: %v", c)
	}
	// Pins are hidden from pushers.
	cmd := exec.Command("git", "-C", work, "push", dir, "main:refs/coolix/pushed/x")
	if err := cmd.Run(); err == nil {
		t.Fatal("push to a hidden ref succeeded")
	}
}
