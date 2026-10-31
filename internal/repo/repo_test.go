package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"coolix/internal/appdef"
)

func TestGenerateFlake(t *testing.T) {
	r := &Repo{Nixpkgs: "/nix/store/aaa-source", Lib: "/nix/store/bbb-source", System: "x86_64-linux", GitDir: "/var/lib/coolix/git"}
	no := false
	got := r.GenerateFlake([]*appdef.App{
		{Name: "blog", Source: "github:me/blog"},
		{Name: "api", Source: "git+https://example.com/api.git", FollowNixpkgs: &no},
		{Name: "shop", Source: "push", Branch: "prod"},
		{Name: "new", Source: "push"},
	}, []string{"new"})
	for _, want := range []string{
		`nixpkgs.url = "path:/nix/store/aaa-source";`,
		`coolix.url = "path:/nix/store/bbb-source";`,
		`app-blog.url = "github:me/blog";`,
		`app-blog.inputs.nixpkgs.follows = "nixpkgs";`,
		`app-api.url = "git+https://example.com/api.git";`,
		`system = "x86_64-linux";`,
		`app-shop.url = "git+file:///var/lib/coolix/git/shop.git?ref=refs/heads/prod";`,
		`skip = [ "new" ];`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "app-new") {
		t.Error("unpushed app got an input")
	}
	if strings.Contains(got, "app-api.inputs.nixpkgs.follows") {
		t.Error("api opted out of following nixpkgs")
	}
}

func TestInitAndCommit(t *testing.T) {
	r := &Repo{Dir: t.TempDir(), Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux"}
	if err := r.Init(); err != nil {
		t.Fatal(err)
	}
	if err := appdef.Save(r.Dir, &appdef.App{Name: "blog", Source: "github:me/blog", PortBase: 20000}); err != nil {
		t.Fatal(err)
	}
	apps, err := r.Sync()
	if err != nil || len(apps) != 1 {
		t.Fatalf("sync: %v %v", apps, err)
	}
	if err := r.Commit("add blog"); err != nil {
		t.Fatal(err)
	}
	// Second commit with no changes must be a no-op, not an error.
	if err := r.Commit("nothing"); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreTree(t *testing.T) {
	r := &Repo{Dir: t.TempDir(), Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux"}
	if err := r.Init(); err != nil {
		t.Fatal(err)
	}
	head := func() string {
		out, err := exec.Command("git", "-C", r.Dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	base := head()
	if err := appdef.Save(r.Dir, &appdef.App{Name: "blog", Source: "github:me/blog", PortBase: 20000}); err != nil {
		t.Fatal(err)
	}
	r.Sync()
	if err := r.Commit("add blog"); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreTree(base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(appdef.Path(r.Dir, "blog")); !os.IsNotExist(err) {
		t.Fatal("app added after base revision should be gone")
	}
	if err := r.Commit("rollback"); err != nil {
		t.Fatal(err)
	}
	if err := r.RestoreTree("deadbeef"); err == nil {
		t.Fatal("expected error for unknown revision")
	}
}

func TestCloneAndAdopt(t *testing.T) {
	r := &Repo{Dir: filepath.Join(t.TempDir(), "repo"), Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux"}
	if err := r.Init(); err != nil {
		t.Fatal(err)
	}
	base, _ := r.Head()

	// Untouched main repo: fast-forward.
	c, err := r.Clone(filepath.Join(t.TempDir(), "clone"), base)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(c.Dir, "flake.lock"), []byte("lock1"), 0o644)
	if err := c.Commit("Deploy"); err != nil {
		t.Fatal(err)
	}
	built, err := r.Adopt(c, base, "Deploy")
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := r.Head(); head != built {
		t.Fatalf("not fast-forwarded: head %s built %s", head, built)
	}

	// Edits meanwhile: only flake.lock is taken over.
	base = built
	c, _ = r.Clone(filepath.Join(t.TempDir(), "clone"), base)
	os.WriteFile(filepath.Join(c.Dir, "flake.lock"), []byte("lock2"), 0o644)
	c.Commit("Deploy")
	os.WriteFile(filepath.Join(r.Dir, "apps", "note"), []byte("edit"), 0o644)
	r.Commit("Edit")
	built, err = r.Adopt(c, base, "Deploy")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(r.Dir, "flake.lock")); string(data) != "lock2" {
		t.Fatalf("lock not adopted: %q", data)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "apps", "note")); err != nil {
		t.Fatal("edit lost")
	}
	if head, _ := r.Head(); head == built {
		t.Fatal("built commit should be the clone's, not the merged head")
	}
}
