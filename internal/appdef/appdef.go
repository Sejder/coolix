// Package appdef defines apps/<name>/app.toml, the per-app definition the UI
// and CLI read and write. The Nix lib (nix/lib/app.nix) consumes the same file.
package appdef

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"coolix/internal/units"
)

type Health struct {
	Path    string `toml:"path,omitempty"`
	Timeout string `toml:"timeout,omitempty"`
}

// Database is a per-app database instance; coolix injects its connection
// settings (DATABASE_URL for postgres, REDIS_URL for redis).
type Database struct {
	Kind    string `toml:"kind"`
	Version string `toml:"version,omitempty"`
}

var dbKinds = map[string]bool{"postgres": true, "redis": true}

// Resources are systemd resource limits for the app's slots.
type Resources struct {
	MemoryMax string `toml:"memory_max,omitempty"`
	CPUQuota  string `toml:"cpu_quota,omitempty"`
	TasksMax  int    `toml:"tasks_max,omitempty"`
}

// Backup schedules postgres dumps (on-demand backups work without it).
type Backup struct {
	Schedule string `toml:"schedule,omitempty"`
	Keep     int    `toml:"keep,omitempty"`
}

// Webhook settings; the token itself lives outside the repo.
type Webhook struct {
	// Branch limits deploys to pushes to this branch ("" deploys on any push).
	Branch string `toml:"branch,omitempty"`
}

type App struct {
	Name    string `toml:"name"`
	Source  string `toml:"source"`
	Package string `toml:"package,omitempty"`
	Command string `toml:"command,omitempty"`
	// PreStart runs (from the package, with the app's environment) before
	// every start of a slot, e.g. database migrations. Must be idempotent.
	PreStart string   `toml:"pre_start,omitempty"`
	Domains  []string `toml:"domains,omitempty"`
	// PortBase is the blue slot's port; green uses PortBase+1.
	PortBase      int   `toml:"port_base"`
	FollowNixpkgs *bool `toml:"follow_nixpkgs,omitempty"`
	// Path lists nixpkgs attributes whose bin/ the app gets on its PATH.
	Path      []string          `toml:"path,omitempty"`
	Health    *Health           `toml:"health,omitempty"`
	Env       map[string]string `toml:"env,omitempty"`
	Secrets   []string          `toml:"secrets,omitempty"`
	Databases []Database        `toml:"databases,omitempty"`
	Resources *Resources        `toml:"resources,omitempty"`
	Backup    *Backup           `toml:"backup,omitempty"`
	Webhook   *Webhook          `toml:"webhook,omitempty"`
	Hardening map[string]any    `toml:"hardening,omitempty"`
}

// HasDatabase reports whether the app has a database of kind.
func (a *App) HasDatabase(kind string) bool {
	for _, d := range a.Databases {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// WebhookBranch returns the branch webhook deploys are limited to.
func (a *App) WebhookBranch() string {
	if a.Webhook == nil {
		return ""
	}
	return a.Webhook.Branch
}

// FollowsNixpkgs reports whether the app's nixpkgs input should follow the
// host's, which avoids fetching a second nixpkgs. Defaults to true.
func (a *App) FollowsNixpkgs() bool {
	return a.FollowNixpkgs == nil || *a.FollowNixpkgs
}

var (
	commandRe = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	attrRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_'+-]*(\.[A-Za-z_][A-Za-z0-9_'+-]*)*$`)
	branchRe  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	healthRe  = regexp.MustCompile(`^/[!-~]*$`)
	versionRe = regexp.MustCompile(`^[0-9]{1,3}$`)
)

func validCommand(c string) bool {
	return commandRe.MatchString(c) && !strings.HasPrefix(c, "/") && !strings.Contains(c, "..")
}

// Validate checks everything that ends up interpolated into flake.nix or the
// profile manifest, so a bad app.toml fails here instead of producing broken
// Nix. Root checks the manifest again (package units) before activating.
func (a *App) Validate() error {
	var errs []error
	if !units.ValidName(a.Name) {
		errs = append(errs, fmt.Errorf("name %q: must be 2-32 chars of a-z, 0-9 and -, starting with a letter", a.Name))
	}
	if a.Source == "" {
		errs = append(errs, errors.New("source: required (a flake reference, e.g. github:me/app)"))
	} else if strings.ContainsAny(a.Source, "\"\\$\n\r\t ") {
		errs = append(errs, fmt.Errorf("source %q: must not contain quotes, backslashes, $ or whitespace", a.Source))
	}
	if a.Package != "" && !attrRe.MatchString(a.Package) {
		errs = append(errs, fmt.Errorf("package %q: invalid attribute name", a.Package))
	}
	if a.Command != "" && !validCommand(a.Command) {
		errs = append(errs, fmt.Errorf("command %q: must be a relative path inside the package, e.g. bin/server", a.Command))
	}
	if a.PreStart != "" && !validCommand(a.PreStart) {
		errs = append(errs, fmt.Errorf("pre_start %q: must be a relative path inside the package, e.g. bin/migrate", a.PreStart))
	}
	for _, d := range a.Domains {
		if !units.ValidDomain(d) {
			errs = append(errs, fmt.Errorf("domain %q: invalid hostname", d))
		}
	}
	if a.PortBase <= 0 || a.PortBase+1 > 65535 {
		errs = append(errs, fmt.Errorf("port_base %d: out of range", a.PortBase))
	}
	for _, p := range a.Path {
		if !attrRe.MatchString(p) {
			errs = append(errs, fmt.Errorf("path %q: want a nixpkgs attribute, e.g. ffmpeg or python3Packages.gunicorn", p))
		}
	}
	if a.Health != nil {
		if a.Health.Path != "" && !healthRe.MatchString(a.Health.Path) {
			errs = append(errs, fmt.Errorf("health path %q: must start with / and contain no spaces", a.Health.Path))
		}
		if err := units.CheckHealthTimeout(a.Health.Timeout); err != nil {
			errs = append(errs, err)
		}
	}
	for k, v := range a.Env {
		switch {
		case !units.ValidEnvKey(k):
			errs = append(errs, fmt.Errorf("env %q: invalid variable name", k))
		case units.ReservedEnv(k):
			errs = append(errs, fmt.Errorf("env %q: reserved by coolix", k))
		case strings.ContainsRune(v, 0):
			errs = append(errs, fmt.Errorf("env %s: value contains a NUL byte", k))
		}
	}
	for _, k := range a.Secrets {
		switch {
		case !units.ValidEnvKey(k):
			errs = append(errs, fmt.Errorf("secret %q: invalid variable name", k))
		case units.ReservedEnv(k):
			errs = append(errs, fmt.Errorf("secret %q: reserved by coolix", k))
		case a.Env[k] != "":
			errs = append(errs, fmt.Errorf("secret %q: also set as a plain env var", k))
		}
	}
	seen := map[string]bool{}
	for _, db := range a.Databases {
		switch {
		case !dbKinds[db.Kind]:
			errs = append(errs, fmt.Errorf("database kind %q: want postgres or redis", db.Kind))
		case seen[db.Kind]:
			errs = append(errs, fmt.Errorf("database kind %q: listed twice", db.Kind))
		case db.Version != "" && (db.Kind != "postgres" || !versionRe.MatchString(db.Version)):
			errs = append(errs, fmt.Errorf("database %s: version %q: only postgres takes a major version, e.g. \"18\"", db.Kind, db.Version))
		}
		seen[db.Kind] = true
	}
	if a.Resources != nil {
		if err := units.CheckResources(units.Resources(*a.Resources)); err != nil {
			errs = append(errs, err)
		}
	}
	if a.Backup != nil {
		if err := units.CheckBackup(units.Backup(*a.Backup), seen["postgres"]); err != nil {
			errs = append(errs, err)
		}
	}
	if b := a.WebhookBranch(); b != "" && (!branchRe.MatchString(b) || strings.Contains(b, "..")) {
		errs = append(errs, fmt.Errorf("webhook branch %q: invalid branch name", b))
	}
	if err := units.CheckHardening(a.Hardening); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func Path(repoDir, name string) string {
	return filepath.Join(repoDir, "apps", name, "app.toml")
}

func Load(path string) (*App, error) {
	var a App
	md, err := toml.DecodeFile(path, &a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, und)
	}
	if want := filepath.Base(filepath.Dir(path)); a.Name != want {
		return nil, fmt.Errorf("%s: name %q does not match directory %q", path, a.Name, want)
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &a, nil
}

// LoadAll returns every app in the repo, sorted by name.
func LoadAll(repoDir string) ([]*App, error) {
	paths, err := filepath.Glob(filepath.Join(repoDir, "apps", "*", "app.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	apps := make([]*App, 0, len(paths))
	for _, p := range paths {
		a, err := Load(p)
		if err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, nil
}

func Save(repoDir string, a *App) error {
	if err := a.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Managed by coolix. Hand edits are fine; keep it valid.\n")
	if err := toml.NewEncoder(&buf).Encode(a); err != nil {
		return err
	}
	p := Path(repoDir, a.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o644)
}

// AllocatePort returns the lowest free even-aligned pair of ports in [lo, hi].
func AllocatePort(apps []*App, lo, hi int) (int, error) {
	used := map[int]bool{}
	for _, a := range apps {
		used[a.PortBase] = true
		used[a.PortBase+1] = true
	}
	for p := lo; p+1 <= hi; p += 2 {
		if !used[p] && !used[p+1] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free ports left in %d-%d", lo, hi)
}
