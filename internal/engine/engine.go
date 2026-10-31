// Package engine implements coolix operations (editing apps, deploying,
// rolling back, activating) for both the CLI and the web UI.
package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"coolix/internal/activate"
	"coolix/internal/query"
	"coolix/internal/repo"
	"coolix/internal/units"
)

type Config struct {
	StateDir string
	// User is the unprivileged user owning StateDir; empty runs activation
	// in-process (development).
	User             string
	Nixpkgs, Lib     string
	System           string
	PortMin, PortMax int
	Profile, UnitDir string
	CaddyDir         string // empty disables routing
	TLS              string
	Drain            time.Duration
	KeepGenerations  int
	SecretsDir       string // root-owned encrypted credentials
	// RenderDir holds root-rendered units; Exec is the coolix binary they call.
	RenderDir, Exec string
	// UIDomain can't be claimed by apps.
	UIDomain string
	// QuerySocket is the coolix-query socket; empty answers in-process.
	QuerySocket string
	// BackupDir is where root sees app backups (for in-process queries).
	BackupDir string
	// ControlSocket is coolix-control's socket (privileged requests).
	ControlSocket string
	// ReservedDomains are the host's own Caddy sites (besides UIDomain).
	ReservedDomains []string
	// AllowedHardening overrides units.DefaultAllowedHardening when set.
	AllowedHardening []string
	// DefaultResources apply to apps without their own limits.
	DefaultResources units.Resources
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ConfigFromEnv reads the COOLIX_* variables set by the NixOS module's wrapper.
func ConfigFromEnv() (*Config, error) {
	c := &Config{
		StateDir:         envOr("COOLIX_STATE_DIR", "/var/lib/coolix"),
		User:             os.Getenv("COOLIX_USER"),
		Nixpkgs:          os.Getenv("COOLIX_NIXPKGS"),
		Lib:              os.Getenv("COOLIX_LIB"),
		System:           os.Getenv("COOLIX_SYSTEM"),
		Profile:          envOr("COOLIX_PROFILE", activate.DefaultProfile),
		UnitDir:          envOr("COOLIX_UNIT_DIR", activate.DefaultUnitDir),
		CaddyDir:         os.Getenv("COOLIX_CADDY_DIR"),
		TLS:              envOr("COOLIX_TLS", "acme"),
		SecretsDir:       envOr("COOLIX_SECRETS_DIR", "/var/lib/coolix-secrets"),
		RenderDir:        envOr("COOLIX_RENDER_DIR", activate.DefaultRenderDir),
		Exec:             os.Getenv("COOLIX_EXEC"),
		UIDomain:         os.Getenv("COOLIX_UI_DOMAIN"),
		QuerySocket:      os.Getenv("COOLIX_QUERY_SOCKET"),
		BackupDir:        envOr("COOLIX_BACKUP_DIR", "/var/lib/private/coolix-backups"),
		ControlSocket:    os.Getenv("COOLIX_CONTROL_SOCKET"),
		ReservedDomains:  reservedDomains(os.Getenv("COOLIX_RESERVED_DOMAINS")),
		DefaultResources: units.Resources{MemoryMax: os.Getenv("COOLIX_DEFAULT_MEMORY_MAX")},
	}
	var err error
	ints := []struct {
		dst *int
		key string
		def string
	}{
		{&c.PortMin, "COOLIX_PORT_MIN", "20000"},
		{&c.PortMax, "COOLIX_PORT_MAX", "29999"},
		{&c.KeepGenerations, "COOLIX_KEEP_GENERATIONS", "20"},
		{&c.DefaultResources.TasksMax, "COOLIX_DEFAULT_TASKS_MAX", "0"},
	}
	for _, i := range ints {
		if *i.dst, err = strconv.Atoi(envOr(i.key, i.def)); err != nil {
			return nil, fmt.Errorf("%s: %w", i.key, err)
		}
	}
	if v, ok := os.LookupEnv("COOLIX_ALLOWED_HARDENING"); ok {
		c.AllowedHardening = strings.Fields(v)
	}
	if c.Drain, err = time.ParseDuration(envOr("COOLIX_DRAIN", "2s")); err != nil {
		return nil, fmt.Errorf("COOLIX_DRAIN: %w", err)
	}
	return c, nil
}

func (c *Config) Repo() (*repo.Repo, error) {
	if c.Nixpkgs == "" || c.Lib == "" || c.System == "" {
		return nil, errors.New("COOLIX_NIXPKGS, COOLIX_LIB and COOLIX_SYSTEM must be set (the NixOS module's wrapper does this)")
	}
	return &repo.Repo{Dir: c.StateDir + "/repo", Nixpkgs: c.Nixpkgs, Lib: c.Lib, System: c.System}, nil
}

func (c *Config) PendingDir() string { return c.StateDir + "/secrets-pending" }

// Engine runs operations, writing progress to Out.
type Engine struct {
	*Config
	Out io.Writer
	// OnDeployStart, if set, is called once a deploy holds the deploy lock.
	OnDeployStart func()
}

func (e *Engine) logf(format string, args ...any) {
	fmt.Fprintf(e.Out, "coolix: "+format+"\n", args...)
}

func ShortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// Query returns a client for root-level read-only data (logs, backups).
func (c *Config) Query() query.Client {
	return query.Client{Socket: c.QuerySocket, Local: &query.Server{BackupDir: c.BackupDir}}
}

// Policy is what the root activator enforces on profiles.
func (c *Config) Policy() units.Policy {
	p := units.Policy{
		PortMin: c.PortMin, PortMax: c.PortMax,
		ReservedDomains:  slices.Clone(c.ReservedDomains),
		AllowedHardening: c.AllowedHardening,
		CheckCalendar:    checkCalendar,
	}
	if c.UIDomain != "" {
		p.ReservedDomains = append(p.ReservedDomains, c.UIDomain)
	}
	return p
}

func (c *Config) Options() units.Options {
	return units.Options{Exec: c.Exec, SecretsDir: c.SecretsDir, DefaultResources: c.DefaultResources}
}

// checkCalendar validates a systemd calendar expression with systemd-analyze
// (skipped where it isn't installed, e.g. in unit tests).
func checkCalendar(expr string) error {
	path, err := exec.LookPath("systemd-analyze")
	if err != nil {
		return nil
	}
	out, err := exec.Command(path, "calendar", "--", expr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("not a valid calendar expression: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// reservedDomains turns Caddy site addresses ("http://a.example.com:8080",
// "*.example.com") into hostnames.
func reservedDomains(s string) []string {
	var out []string
	for _, addr := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		if _, rest, ok := strings.Cut(addr, "://"); ok {
			addr = rest
		}
		addr, _, _ = strings.Cut(addr, "/")
		if h, _, err := net.SplitHostPort(addr); err == nil {
			addr = h
		}
		addr = strings.TrimSuffix(strings.ToLower(addr), ".")
		if addr != "" {
			out = append(out, addr)
		}
	}
	return out
}
