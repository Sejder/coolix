package engine

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"coolix/internal/appdef"
)

func testEngine(t *testing.T) *Engine {
	dir := t.TempDir()
	return &Engine{Config: &Config{
		StateDir: dir, Nixpkgs: "/nix/store/a", Lib: "/nix/store/b", System: "x86_64-linux",
		PortMin: 20000, PortMax: 20100, Profile: filepath.Join(dir, "profile"), PreviewDomain: "preview.test",
	}, Out: io.Discard}
}

func TestUpsertPreview(t *testing.T) {
	e := testEngine(t)
	parent := &appdef.App{
		Name: "blog", Source: "github:me/blog", Domains: []string{"blog.test"}, Secrets: []string{"KEY"},
		Databases: []appdef.Database{{Kind: "postgres"}}, Backup: &appdef.Backup{Schedule: "daily"},
		Env: map[string]string{"A": "1"},
	}
	if err := e.CreateApp(parent); err != nil {
		t.Fatal(err)
	}
	pr := Preview{Parent: "blog", Number: 4, Branch: "feat", Rev: strings.Repeat("a", 40)}
	if _, err := e.upsertPreview(pr); err == nil || !strings.Contains(err.Error(), "previews are off") {
		t.Fatalf("previews off: %v", err)
	}
	if _, err := e.ModifyApp("blog", func(a *appdef.App) error {
		a.Preview = &appdef.Preview{Enabled: true, Max: 2}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	name, err := e.upsertPreview(pr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.App(name)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case name != "blog-pr4", p.PreviewOf != "blog", p.PreviewRef != "pr-4":
		t.Errorf("preview %s: %+v", name, p)
	case p.Domains[0] != "blog-pr4.preview.test":
		t.Errorf("domain %v", p.Domains)
	case p.Source != "github:me/blog?rev="+pr.Rev:
		t.Errorf("source %s", p.Source)
	case len(p.Secrets) != 0 || p.PreviewSecrets || p.Backup != nil || p.Preview != nil:
		t.Errorf("preview kept parent-only settings: %+v", p)
	case p.Env["A"] != "1" || !p.HasDatabase("postgres"):
		t.Errorf("preview lost settings: %+v", p)
	case p.PortBase == 20000:
		t.Errorf("preview shares the parent's ports")
	}
	port := p.PortBase

	// An update keeps the ports and follows the new commit.
	pr.Rev = strings.Repeat("b", 40)
	if _, err := e.upsertPreview(pr); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.App(name); p.PortBase != port || !strings.HasSuffix(p.Source, pr.Rev) {
		t.Errorf("update: %+v", p)
	}
	if _, err := e.upsertPreview(Preview{Parent: "blog", Branch: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.upsertPreview(Preview{Parent: "blog", Branch: "three"}); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Errorf("max: %v", err)
	}
	if _, err := e.upsertPreview(Preview{Parent: "blog-pr4", Branch: "x"}); err == nil {
		t.Error("preview of a preview")
	}
	if got, ok := e.FindPreview("blog", "pr-4"); !ok || got != name {
		t.Errorf("find: %q %v", got, ok)
	}
	if ps, _ := e.Previews("blog"); len(ps) != 2 {
		t.Errorf("previews: %d", len(ps))
	}
	if err := e.RemoveApp("blog"); err != nil {
		t.Fatal(err)
	}
	if apps, _ := e.Apps(); len(apps) != 0 {
		t.Errorf("previews survived their parent: %d apps", len(apps))
	}
}

func TestPreviewNameCollision(t *testing.T) {
	e := testEngine(t)
	for _, a := range []*appdef.App{
		{Name: "blog", Source: "github:me/blog", Preview: &appdef.Preview{Enabled: true, Secrets: true}},
		{Name: "blog-pr1", Source: "github:me/other"},
	} {
		if err := e.CreateApp(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.upsertPreview(Preview{Parent: "blog", Number: 1, Branch: "x"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("collision: %v", err)
	}
	name, err := e.upsertPreview(Preview{Parent: "blog", Branch: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := e.App(name); !p.PreviewSecrets {
		t.Error("secrets opt-in not passed on")
	}
}
