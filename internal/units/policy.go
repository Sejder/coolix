package units

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	envKeyRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	domainRe   = regexp.MustCompile(`^(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?))*$`)
	storeRe    = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[A-Za-z0-9+._?=-]+$`)
	storeExeRe = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[A-Za-z0-9+._?=-]+(/[A-Za-z0-9+._-]+)+$`)
	healthRe   = regexp.MustCompile(`^/[!-~]*$`)
	memoryRe   = regexp.MustCompile(`^([0-9]+[KMGT]?|[0-9]{1,2}%|infinity)$`)
	cpuRe      = regexp.MustCompile(`^[0-9]{1,5}%$`)
	calendarRe = regexp.MustCompile(`^[A-Za-z0-9*:,./~ -]{1,64}$`)
)

// ValidName reports whether s is a valid app name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidDomain reports whether s is a hostname coolix may route.
func ValidDomain(s string) bool { return len(s) <= 253 && domainRe.MatchString(s) }

// ValidEnvKey reports whether s is a valid environment variable name.
func ValidEnvKey(s string) bool { return envKeyRe.MatchString(s) }

// ReservedEnv reports whether coolix sets the variable itself.
func ReservedEnv(k string) bool {
	switch k {
	case "PORT", "PATH", "DATABASE_URL", "REDIS_URL", "PGHOST", "PGUSER", "PGDATABASE", "CREDENTIALS_DIRECTORY":
		return true
	}
	return strings.HasPrefix(k, "COOLIX_")
}

// MaxHealthTimeout bounds how long an activation waits for one app.
const MaxHealthTimeout = 10 * time.Minute

// Hardening settings an app may change. Everything that would let it gain
// privileges (DynamicUser, NoNewPrivileges, capabilities, ProtectSystem,
// RestrictSUIDSGID, ...) is fixed.
var relaxable = map[string]bool{
	"MemoryDenyWriteExecute": true, "PrivateDevices": true, "PrivateTmp": true,
	"ProtectHome": true, "ProtectClock": true, "ProtectHostname": true,
	"ProtectKernelLogs": true, "ProtectKernelModules": true, "ProtectKernelTunables": true,
	"ProtectControlGroups": true, "RestrictNamespaces": true, "RestrictRealtime": true,
	"LockPersonality": true,
}

var stringSettings = map[string]*regexp.Regexp{
	"RestrictAddressFamilies": regexp.MustCompile(`^(none|~?AF_[A-Z0-9]+( +AF_[A-Z0-9]+)*)$`),
	"SystemCallFilter":        regexp.MustCompile(`^~?@?[a-z0-9_-]+( +@?[a-z0-9_-]+)*$`),
	"UMask":                   regexp.MustCompile(`^0[0-7]{3}$`),
}

// CheckHardening validates [hardening] overrides.
func CheckHardening(h map[string]any) error {
	var errs []error
	for _, k := range sortedKeys(h) {
		v := h[k]
		if relaxable[k] {
			if _, ok := v.(bool); !ok {
				errs = append(errs, fmt.Errorf("hardening %s: want true or false", k))
			}
			continue
		}
		if re, ok := stringSettings[k]; ok {
			if s, ok := v.(string); !ok || !re.MatchString(s) {
				errs = append(errs, fmt.Errorf("hardening %s: invalid value %v", k, v))
			}
			continue
		}
		errs = append(errs, fmt.Errorf("hardening %s: not something apps may change (allowed: %s)", k, strings.Join(allowedHardening(), ", ")))
	}
	return errors.Join(errs...)
}

func allowedHardening() []string {
	var out []string
	for k := range relaxable {
		out = append(out, k)
	}
	for k := range stringSettings {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func CheckResources(r Resources) error {
	var errs []error
	if r.MemoryMax != "" && !memoryRe.MatchString(r.MemoryMax) {
		errs = append(errs, fmt.Errorf("memory_max %q: want e.g. 512M, 2G or 50%%", r.MemoryMax))
	}
	if r.CPUQuota != "" && !cpuRe.MatchString(r.CPUQuota) {
		errs = append(errs, fmt.Errorf("cpu_quota %q: want a percentage, e.g. 50%% or 200%%", r.CPUQuota))
	}
	if r.TasksMax < 0 || r.TasksMax > 1_000_000 {
		errs = append(errs, fmt.Errorf("tasks_max %d: out of range", r.TasksMax))
	}
	return errors.Join(errs...)
}

func CheckBackup(b Backup, hasPostgres bool) error {
	if b.Schedule == "" && b.Keep == 0 {
		return nil
	}
	var errs []error
	if !hasPostgres {
		errs = append(errs, errors.New("backup: only postgres databases are backed up; add one first"))
	}
	if b.Schedule != "" && !calendarRe.MatchString(b.Schedule) {
		errs = append(errs, fmt.Errorf("backup schedule %q: want a systemd calendar expression, e.g. daily or *-*-* 03:00", b.Schedule))
	}
	if b.Keep < 0 || b.Keep > 1000 {
		errs = append(errs, fmt.Errorf("backup keep %d: out of range", b.Keep))
	}
	return errors.Join(errs...)
}

func CheckHealthTimeout(s string) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > MaxHealthTimeout {
		return fmt.Errorf("health timeout %q: want a duration up to %s, e.g. 30s", s, MaxHealthTimeout)
	}
	return nil
}

// Policy is what root enforces on every profile before activating it.
type Policy struct {
	PortMin, PortMax int
	// ReservedDomains can't be claimed by apps (e.g. the UI's domain).
	ReservedDomains []string
}

// Validate checks every field of m that ends up in a unit, a Caddy snippet
// or a health check. It must not trust anything the Nix lib did.
func (p Policy) Validate(m *Manifest) error {
	var errs []error
	ports := map[int]string{}
	domains := map[string]string{}
	for _, d := range p.ReservedDomains {
		domains[strings.ToLower(d)] = "coolix itself"
	}
	for _, name := range sortedKeys(m.Apps) {
		app := m.Apps[name]
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("app %s: "+format, append([]any{name}, args...)...))
		}
		if !ValidName(name) {
			errs = append(errs, fmt.Errorf("invalid app name %q", name))
			continue
		}
		if !validExe(app.Exe) {
			fail("exe %q: must be a file inside a store path", app.Exe)
		}
		if app.PreStart != "" && !validExe(app.PreStart) {
			fail("pre_start %q: must be a file inside a store path", app.PreStart)
		}
		if app.PortBase < p.PortMin || app.PortBase+1 > p.PortMax {
			fail("port_base %d: outside the allowed range %d-%d", app.PortBase, p.PortMin, p.PortMax)
		}
		for _, port := range []int{app.PortBase, app.PortBase + 1} {
			if other, ok := ports[port]; ok {
				fail("port %d already used by %s", port, other)
			}
			ports[port] = name
		}
		for _, d := range app.Domains {
			if !ValidDomain(d) {
				fail("domain %q: invalid hostname", d)
				continue
			}
			if other, ok := domains[strings.ToLower(d)]; ok {
				fail("domain %s already used by %s", d, other)
			}
			domains[strings.ToLower(d)] = name
		}
		if app.Health.Path != "" && !healthRe.MatchString(app.Health.Path) {
			fail("health path %q: must start with / and contain no spaces", app.Health.Path)
		}
		if err := CheckHealthTimeout(app.Health.Timeout); err != nil {
			fail("%v", err)
		}
		for _, k := range sortedKeys(app.Env) {
			if !ValidEnvKey(k) || ReservedEnv(k) {
				fail("env %q: invalid or reserved name", k)
			}
			if strings.ContainsRune(app.Env[k], 0) {
				fail("env %s: value contains a NUL byte", k)
			}
		}
		for _, s := range app.Path {
			if !storeRe.MatchString(s) {
				fail("path entry %q: must be a store path", s)
			}
		}
		seen := map[string]bool{}
		for _, db := range app.Databases {
			if db.Kind != "postgres" && db.Kind != "redis" {
				fail("database kind %q: want postgres or redis", db.Kind)
			}
			if seen[db.Kind] {
				fail("database %s listed twice", db.Kind)
			}
			seen[db.Kind] = true
			if !storeRe.MatchString(db.Package) {
				fail("database %s: package %q must be a store path", db.Kind, db.Package)
			}
		}
		if err := CheckHardening(app.Hardening); err != nil {
			fail("%v", err)
		}
		if err := CheckResources(app.Resources); err != nil {
			fail("%v", err)
		}
		if err := CheckBackup(app.Backup, seen["postgres"]); err != nil {
			fail("%v", err)
		}
	}
	return errors.Join(errs...)
}

func validExe(s string) bool {
	return storeExeRe.MatchString(s) && path.Clean(s) == s && !strings.Contains(s, "/../") && !strings.HasSuffix(s, "/..")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
