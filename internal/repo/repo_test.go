package repo

import (
	"strings"
	"testing"

	"coolix/internal/appdef"
)

func TestGenerateFlake(t *testing.T) {
	r := &Repo{Nixpkgs: "/nix/store/aaa-source", Lib: "/nix/store/bbb-source", System: "x86_64-linux"}
	no := false
	got := r.GenerateFlake([]*appdef.App{
		{Name: "blog", Source: "github:me/blog"},
		{Name: "api", Source: "git+https://example.com/api.git", FollowNixpkgs: &no},
	})
	for _, want := range []string{
		`nixpkgs.url = "path:/nix/store/aaa-source";`,
		`coolix.url = "path:/nix/store/bbb-source";`,
		`app-blog.url = "github:me/blog";`,
		`app-blog.inputs.nixpkgs.follows = "nixpkgs";`,
		`app-api.url = "git+https://example.com/api.git";`,
		`system = "x86_64-linux";`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
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
