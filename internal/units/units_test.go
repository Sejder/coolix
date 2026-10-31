package units

import (
	"strings"
	"testing"
)

const (
	exe = "/nix/store/0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-hello-1.0/bin/hello"
	pg  = "/nix/store/1jzvzdyhmwjm2xdw2iak5p5x8qj2pkgr-postgresql-18.1"
)

var policy = Policy{PortMin: 20000, PortMax: 29999, ReservedDomains: []string{"coolix.example.com", "*.internal.example.com"}}

func manifest() *Manifest {
	return &Manifest{Version: ManifestVersion, Apps: map[string]App{
		"blog": {
			Exe: exe, PortBase: 20000, Domains: []string{"blog.example.com"},
			Health:    Health{Path: "/healthz", Timeout: "5s"},
			Env:       map[string]string{"LOG_LEVEL": "info"},
			Secrets:   true,
			Databases: []Database{{Kind: "postgres", Package: pg}},
			Hardening: map[string]any{"MemoryDenyWriteExecute": false},
			Resources: Resources{MemoryMax: "512M", CPUQuota: "50%", TasksMax: 64},
			Backup:    Backup{Schedule: "daily", Keep: 3},
		},
	}}
}

func TestValidManifest(t *testing.T) {
	if err := policy.Validate(manifest()); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRejects(t *testing.T) {
	cases := map[string]func(a *App){
		"exe outside store":   func(a *App) { a.Exe = "/bin/sh" },
		"exe escaping store":  func(a *App) { a.Exe = "/nix/store/0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-hello/../../../bin/sh" },
		"exe with args":       func(a *App) { a.Exe = exe + " --evil" },
		"exe with newline":    func(a *App) { a.Exe = exe + "\nUser=root" },
		"pre_start escaping":  func(a *App) { a.PreStart = "/etc/passwd" },
		"root user":           func(a *App) { a.Hardening = map[string]any{"DynamicUser": false} },
		"user override":       func(a *App) { a.Hardening = map[string]any{"User": "root"} },
		"capabilities":        func(a *App) { a.Hardening = map[string]any{"AmbientCapabilities": "CAP_SYS_ADMIN"} },
		"new privileges":      func(a *App) { a.Hardening = map[string]any{"NoNewPrivileges": false} },
		"writable system":     func(a *App) { a.Hardening = map[string]any{"ProtectSystem": false} },
		"privileged exec":     func(a *App) { a.Hardening = map[string]any{"ExecStartPre": "+/bin/sh"} },
		"string as bool":      func(a *App) { a.Hardening = map[string]any{"PrivateTmp": "no\nUser=root"} },
		"syscall injection":   func(a *App) { a.Hardening = map[string]any{"SystemCallFilter": "@system-service\nUser=root"} },
		"port out of range":   func(a *App) { a.PortBase = 22 },
		"ui domain":           func(a *App) { a.Domains = []string{"COOLIX.example.com"} },
		"caddy injection":     func(a *App) { a.Domains = []string{"a.com {\n}\nevil.com"} },
		"reserved env":        func(a *App) { a.Env = map[string]string{"PATH": "/tmp"} },
		"env key injection":   func(a *App) { a.Env = map[string]string{"A\nUser": "root"} },
		"db package":          func(a *App) { a.Databases = []Database{{Kind: "postgres", Package: "/tmp/pg"}} },
		"db kind":             func(a *App) { a.Databases = []Database{{Kind: "mysql", Package: pg}} },
		"path entry":          func(a *App) { a.Path = []string{"/usr"} },
		"health path":         func(a *App) { a.Health.Path = "/ok\r\nHost: x" },
		"long health timeout": func(a *App) { a.Health.Timeout = "1h" },
		"memory injection":    func(a *App) { a.Resources.MemoryMax = "1G\nUser=root" },
		"backup schedule":     func(a *App) { a.Backup.Schedule = "daily\nOnBootSec=1" },
		"backup without pg":   func(a *App) { a.Databases = nil },
		"reserved wildcard":   func(a *App) { a.Domains = []string{"x.internal.example.com"} },
		"single label":        func(a *App) { a.Domains = []string{"localhost"} },
		"ip literal":          func(a *App) { a.Domains = []string{"127.0.0.1"} },
		"trailing dot":        func(a *App) { a.Domains = []string{"blog.example.com."} },
		"not allowed here":    func(a *App) { a.Hardening = map[string]any{"RestrictNamespaces": false} },
		"syscall deny list":   func(a *App) { a.Hardening = map[string]any{"SystemCallFilter": "~@mount"} },
		"family deny list":    func(a *App) { a.Hardening = map[string]any{"RestrictAddressFamilies": "~AF_INET"} },
		"pre_start forever":   func(a *App) { a.PreStartTimeout = "3h" },
	}
	for name, mutate := range cases {
		m := manifest()
		a := m.Apps["blog"]
		mutate(&a)
		m.Apps["blog"] = a
		if err := policy.Validate(m); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	m := manifest()
	m.Apps["Evil"] = m.Apps["blog"]
	if policy.Validate(m) == nil {
		t.Error("invalid app name accepted")
	}

	m = manifest()
	other := m.Apps["blog"]
	other.Domains = []string{"other.example.com"}
	m.Apps["other"] = other
	if err := policy.Validate(m); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("port conflict: %v", err)
	}
}

func TestRender(t *testing.T) {
	r := Render(manifest(), Options{Exec: "/etc/coolix/coolix", SecretsDir: "/var/lib/coolix-secrets"})
	app := r.Files["coolix-app-blog@.service"]
	for _, want := range []string{
		"DynamicUser=yes\n",
		"NoNewPrivileges=yes\n",
		"CapabilityBoundingSet=\n",
		"MemoryDenyWriteExecute=no\n",
		"User=coolix-app-blog\n",
		"ExecStart=/etc/coolix/coolix exec-app --slot %i --port-base 20000 -- " + exe + "\n",
		`Environment="DATABASE_URL=postgresql:///app?host=/run/coolix-db/blog-postgres"` + "\n",
		`Environment="LOG_LEVEL=info"` + "\n",
		"LoadCredentialEncrypted=env:/var/lib/coolix-secrets/blog.cred\n",
		"Wants=coolix-db-blog-postgres.service\n",
		"SocketBindAllow=tcp:20000\n", "SocketBindAllow=tcp:20001\n", "SocketBindDeny=any\n",
		"MemoryMax=512M\n", "CPUQuota=50%\n", "TasksMax=64\n",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app unit lacks %q:\n%s", want, app)
		}
	}
	if strings.Count(app, "MemoryDenyWriteExecute=") != 1 {
		t.Errorf("duplicate setting:\n%s", app)
	}
	for _, f := range []string{"coolix-db-blog-postgres.service", "coolix-backup-blog.service", "coolix-backup-blog.timer", "coolix-restore-blog@.service"} {
		if r.Files[f] == "" {
			t.Errorf("missing %s", f)
		}
	}
	if !strings.Contains(r.Files["coolix-backup-blog.service"], " 3\n") {
		t.Errorf("backup keep not rendered:\n%s", r.Files["coolix-backup-blog.service"])
	}
	if len(r.Services) != 1 || len(r.Timers) != 1 {
		t.Errorf("services %v timers %v", r.Services, r.Timers)
	}
	for name := range r.Files {
		if !Owned(name) {
			t.Errorf("rendered unit %s is not recognised as coolix's", name)
		}
	}
}

func TestQuoteEnv(t *testing.T) {
	cases := map[string]string{
		`plain`:             `"K=plain"`,
		`a "b" c`:           `"K=a \"b\" c"`,
		`back\slash`:        `"K=back\\slash"`,
		`100% $HOME`:        `"K=100%% $HOME"`,
		"two\nlines\tand\r": `"K=two\nlines\tand\x0d"`,
	}
	for in, want := range cases {
		if got := QuoteEnv("K", in); got != want {
			t.Errorf("QuoteEnv(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestLongNamesRejected(t *testing.T) {
	m := manifest()
	m.Apps["a-twenty-one-chars-xx"] = m.Apps["blog"]
	delete(m.Apps, "blog")
	if policy.Validate(m) == nil {
		t.Fatal("21-char app name accepted (DynamicUser names are capped at 31 chars)")
	}
}

func TestAdditiveHardening(t *testing.T) {
	m := manifest()
	a := m.Apps["blog"]
	a.Hardening = map[string]any{"SystemCallFilter": "@pkey", "RestrictAddressFamilies": "AF_NETLINK"}
	m.Apps["blog"] = a
	if err := policy.Validate(m); err != nil {
		t.Fatal(err)
	}
	unit := Render(m, Options{Exec: "/x"}).Files["coolix-app-blog@.service"]
	for _, want := range []string{
		"SystemCallFilter=@system-service\nSystemCallFilter=@pkey\n",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("want %q in\n%s", want, unit)
		}
	}
	// The host can allow more.
	a.Hardening = map[string]any{"RestrictNamespaces": false}
	m.Apps["blog"] = a
	p := policy
	p.AllowedHardening = append([]string{"RestrictNamespaces"}, DefaultAllowedHardening...)
	if err := p.Validate(m); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAppsBlamesOneApp(t *testing.T) {
	m := manifest()
	bad := m.Apps["blog"]
	bad.Exe = "/bin/sh"
	m.Apps["evil"] = bad
	errs := policy.ValidateApps(m)
	if len(errs) != 1 || errs["evil"] == nil {
		t.Fatalf("got %v", errs)
	}
}

func TestDefaultResources(t *testing.T) {
	m := manifest()
	a := m.Apps["blog"]
	a.Resources = Resources{}
	m.Apps["blog"] = a
	unit := Render(m, Options{Exec: "/x", DefaultResources: Resources{TasksMax: 1024, MemoryMax: "1G"}}).Files["coolix-app-blog@.service"]
	if !strings.Contains(unit, "TasksMax=1024\n") || !strings.Contains(unit, "MemoryMax=1G\n") {
		t.Fatalf("defaults not applied:\n%s", unit)
	}
}

func TestDatabasesHaveNoNetwork(t *testing.T) {
	r := Render(manifest(), Options{Exec: "/x"})
	for _, u := range []string{"coolix-db-blog-postgres.service", "coolix-backup-blog.service", "coolix-restore-blog@.service"} {
		if !strings.Contains(r.Files[u], "PrivateNetwork=yes\n") {
			t.Errorf("%s has network access:\n%s", u, r.Files[u])
		}
	}
}
