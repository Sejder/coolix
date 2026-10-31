package appdef

import (
	"strings"
	"testing"
)

func valid() *App {
	return &App{Name: "blog", Source: "github:me/blog", PortBase: 20000}
}

func TestValidateOK(t *testing.T) {
	a := valid()
	a.Domains = []string{"blog.example.com"}
	a.Env = map[string]string{"LOG_LEVEL": "info"}
	a.Command = "bin/blog"
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*App){
		"bad name":         func(a *App) { a.Name = "Blog" },
		"nix injection":    func(a *App) { a.Source = `github:x/y"; evil = "` },
		"interpolation":    func(a *App) { a.Source = "github:x/${y}" },
		"absolute command": func(a *App) { a.Command = "/bin/sh" },
		"escaping command": func(a *App) { a.Command = "../../bin/sh" },
		"reserved env":     func(a *App) { a.Env = map[string]string{"PORT": "1"} },
		"coolix env":       func(a *App) { a.Env = map[string]string{"COOLIX_SLOT": "x"} },
		"bad domain":       func(a *App) { a.Domains = []string{"bad domain"} },
		"no port":          func(a *App) { a.PortBase = 0 },
	}
	for name, mutate := range cases {
		a := valid()
		mutate(a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := valid()
	a.Env = map[string]string{"A": "1"}
	if err := Save(dir, a); err != nil {
		t.Fatal(err)
	}
	apps, err := LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "blog" || apps[0].Env["A"] != "1" {
		t.Fatalf("round trip mismatch: %+v", apps)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, valid()); err != nil {
		t.Fatal(err)
	}
	p := Path(dir, "blog")
	writeAppend(t, p, "typo_key = 1\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("expected unknown keys error, got %v", err)
	}
}

func TestAllocatePort(t *testing.T) {
	apps := []*App{{PortBase: 20000}, {PortBase: 20004}}
	p, err := AllocatePort(apps, 20000, 20010)
	if err != nil || p != 20002 {
		t.Fatalf("got %d, %v", p, err)
	}
	if _, err := AllocatePort(apps, 20000, 20001); err == nil {
		t.Fatal("expected exhaustion error")
	}
}

func TestValidateDatabasesAndSecrets(t *testing.T) {
	a := valid()
	a.Databases = []Database{{Kind: "postgres", Version: "18"}, {Kind: "redis"}}
	a.Secrets = []string{"API_KEY"}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*App){
		"unknown kind":    func(a *App) { a.Databases = []Database{{Kind: "mysql"}} },
		"duplicate":       func(a *App) { a.Databases = []Database{{Kind: "redis"}, {Kind: "redis"}} },
		"bad version":     func(a *App) { a.Databases = []Database{{Kind: "postgres", Version: "18; rm"}} },
		"redis version":   func(a *App) { a.Databases = []Database{{Kind: "redis", Version: "7"}} },
		"reserved secret": func(a *App) { a.Secrets = []string{"DATABASE_URL"} },
		"secret is env":   func(a *App) { a.Secrets = []string{"A"}; a.Env = map[string]string{"A": "x"} },
		"reserved db env": func(a *App) { a.Env = map[string]string{"PGHOST": "x"} },
	}
	for name, mutate := range cases {
		a := valid()
		mutate(a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
