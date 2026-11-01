package units

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// 2-20 chars: systemd caps user names at 31, and DynamicUser is coolix-app-<name>.
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,18}[a-z0-9]$`)
	backupRe   = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.dump$`)
	envKeyRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	labelRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	storeRe    = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[A-Za-z0-9+._?=-]+$`)
	storeExeRe = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[A-Za-z0-9+._?=-]+(/[A-Za-z0-9+._-]+)+$`)
	healthRe   = regexp.MustCompile(`^/[!-~]*$`)
	memoryRe   = regexp.MustCompile(`^([0-9]+[KMGT]?|[0-9]{1,2}%|infinity)$`)
	cpuRe      = regexp.MustCompile(`^[0-9]{1,5}%$`)
	calendarRe = regexp.MustCompile(`^[A-Za-z0-9*:,./~ -]{1,64}$`)
	durationRe = regexp.MustCompile(`^[0-9]+(ms|s|m|h)$`)
)

// ValidName reports whether s is a valid app name (2-20 chars of a-z, 0-9, -).
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidBackupFile reports whether name looks like a dump coolix wrote.
func ValidBackupFile(name string) bool { return backupRe.MatchString(name) }

// ValidDomain reports whether s is a fully qualified hostname coolix may
// route: at least two labels, no IP addresses, no wildcards.
func ValidDomain(s string) bool {
	s = strings.ToLower(s)
	if len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return false
		}
	}
	// A numeric top-level label would make it an IP-ish name.
	tld := labels[len(labels)-1]
	return strings.Trim(tld, "0123456789") != ""
}

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

// MaxPreStartTimeout bounds how long a pre_start command may run.
const MaxPreStartTimeout = 2 * time.Hour

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

// Additive settings: the value is added to coolix's default, never replaces
// it. RestrictAddressFamilies adds families, SystemCallFilter adds syscalls
// or groups to the allow list.
var stringSettings = map[string]*regexp.Regexp{
	"RestrictAddressFamilies": regexp.MustCompile(`^AF_[A-Z0-9]+( +AF_[A-Z0-9]+)*$`),
	"SystemCallFilter":        regexp.MustCompile(`^@?[a-z0-9_]+( +@?[a-z0-9_]+)*$`),
	"UMask":                   regexp.MustCompile(`^0[0-7]{3}$`),
}

// Additions apps may make to RestrictAddressFamilies and SystemCallFilter.
// Everything else (AF_ALG, AF_VSOCK, @known, @privileged, @debug, bpf, ...)
// would reopen kernel attack surface the sandbox exists to close.
var (
	allowedFamilies = map[string]bool{"AF_NETLINK": true}
	allowedSyscalls = map[string]bool{
		"@pkey": true, "@sync": true, "@timer": true, "@keyring": true, "@memlock": true,
		"@resources": true, "@setuid": true, "@chown": true, "@ipc": true,
	}
)

func checkAdditions(key, value string) error {
	allowed := allowedSyscalls
	if key == "RestrictAddressFamilies" {
		allowed = allowedFamilies
	}
	for _, tok := range strings.Fields(value) {
		if !allowed[tok] {
			keys := make([]string, 0, len(allowed))
			for k := range allowed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return fmt.Errorf("hardening %s: %s can't be added (allowed: %s)", key, tok, strings.Join(keys, " "))
		}
	}
	return nil
}

// DefaultAllowedHardening are the [hardening] keys apps may use unless the
// host allows more (services.coolix.apps.allowedHardeningOverrides). The rest
// widen the kernel attack surface (namespaces, devices, kernel interfaces).
var DefaultAllowedHardening = []string{
	"MemoryDenyWriteExecute", "PrivateTmp", "ProtectHome", "ProtectHostname",
	"ProtectClock", "RestrictRealtime", "LockPersonality", "UMask",
}

// HardeningKeys lists every key [hardening] understands.
func HardeningKeys() []string {
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

// CheckHardening validates the syntax of [hardening] overrides.
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
			} else if k != "UMask" {
				if err := checkAdditions(k, s); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		errs = append(errs, fmt.Errorf("hardening %s: not something apps may change (known: %s)", k, strings.Join(HardeningKeys(), ", ")))
	}
	return errors.Join(errs...)
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

func checkDuration(what, s string, max time.Duration) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || !durationRe.MatchString(s) || d <= 0 || d > max {
		return fmt.Errorf("%s %q: want a duration up to %s, e.g. 30s or 5m", what, s, max)
	}
	return nil
}

func CheckHealthTimeout(s string) error { return checkDuration("health timeout", s, MaxHealthTimeout) }
func CheckPreStartTimeout(s string) error {
	return checkDuration("pre_start_timeout", s, MaxPreStartTimeout)
}

// Policy is what root enforces on every profile before activating it.
type Policy struct {
	PortMin, PortMax int
	// ReservedDomains can't be claimed by apps: the UI's domain and the
	// host's other Caddy sites. "*.example.com" reserves every subdomain.
	ReservedDomains []string
	// AllowedHardening lists the [hardening] keys apps may use; nil means
	// DefaultAllowedHardening.
	AllowedHardening []string
	// CheckCalendar validates a backup schedule (systemd-analyze calendar);
	// nil skips the check.
	CheckCalendar func(string) error
}

func (p Policy) allowedHardening() map[string]bool {
	keys := p.AllowedHardening
	if keys == nil {
		keys = DefaultAllowedHardening
	}
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// reserved reports whether domain d is reserved, and by what.
func (p Policy) reserved(d string) (string, bool) {
	d = strings.ToLower(d)
	for _, r := range p.ReservedDomains {
		r = strings.ToLower(r)
		if suffix, ok := strings.CutPrefix(r, "*."); ok {
			if strings.HasSuffix(d, "."+suffix) {
				return r, true
			}
		} else if d == r {
			return r, true
		}
	}
	return "", false
}

// Validate checks every field of m that ends up in a unit, a Caddy snippet
// or a health check. It must not trust anything the Nix lib did.
func (p Policy) Validate(m *Manifest) error {
	errs := p.ValidateApps(m)
	var all []error
	for _, name := range sortedKeys(errs) {
		all = append(all, errs[name])
	}
	return errors.Join(all...)
}

// ValidateApps is Validate, per app. Conflicts (ports, domains) are blamed
// on the app that comes later by name.
func (p Policy) ValidateApps(m *Manifest) map[string]error {
	out := map[string]error{}
	ports := map[int]string{}
	domains := map[string]string{}
	allowed := p.allowedHardening()
	for _, name := range sortedKeys(m.Apps) {
		app := m.Apps[name]
		var errs []error
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("app %s: "+format, append([]any{name}, args...)...))
		}
		if !ValidName(name) {
			out[name] = fmt.Errorf("invalid app name %q (2-20 chars of a-z, 0-9 and -)", name)
			continue
		}
		if app.SecretsFrom != "" && (!ValidName(app.SecretsFrom) || app.SecretsFrom == name) {
			fail("secrets_from %q: must be another app", app.SecretsFrom)
		}
		if !validExe(app.Exe) {
			fail("exe %q: must be a file inside a store path", app.Exe)
		}
		if app.PreStart != "" && !validExe(app.PreStart) {
			fail("pre_start %q: must be a file inside a store path", app.PreStart)
		}
		if err := CheckPreStartTimeout(app.PreStartTimeout); err != nil {
			fail("%v", err)
		}
		if app.PortBase < p.PortMin || app.PortBase+1 > p.PortMax {
			fail("port_base %d: outside the allowed range %d-%d", app.PortBase, p.PortMin, p.PortMax)
		}
		for _, port := range []int{app.PortBase, app.PortBase + 1} {
			if other, ok := ports[port]; ok {
				fail("port %d already used by %s", port, other)
			}
		}
		for _, d := range app.Domains {
			ld := strings.ToLower(d)
			if !ValidDomain(d) {
				fail("domain %q: invalid hostname (a fully qualified name, no IP addresses)", d)
			} else if r, ok := p.reserved(d); ok {
				fail("domain %s is reserved by the host (%s)", d, r)
			} else if other, ok := domains[ld]; ok {
				fail("domain %s already used by %s", d, other)
			}
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
		for _, k := range sortedKeys(app.Hardening) {
			if !allowed[k] {
				fail("hardening %s: not allowed on this host (see services.coolix.apps.allowedHardeningOverrides)", k)
			}
		}
		if err := CheckResources(app.Resources); err != nil {
			fail("%v", err)
		}
		if err := CheckBackup(app.Backup, seen["postgres"]); err != nil {
			fail("%v", err)
		} else if app.Backup.Schedule != "" && p.CheckCalendar != nil {
			if err := p.CheckCalendar(app.Backup.Schedule); err != nil {
				fail("backup schedule %q: %v", app.Backup.Schedule, err)
			}
		}
		if len(errs) > 0 {
			out[name] = errors.Join(errs...)
			continue
		}
		// Only valid apps claim ports and domains.
		ports[app.PortBase], ports[app.PortBase+1] = name, name
		for _, d := range app.Domains {
			domains[strings.ToLower(d)] = name
		}
	}
	return out
}

func validExe(s string) bool {
	return storeExeRe.MatchString(s) && path.Clean(s) == s
}

// CleanRelative reports whether s is a clean relative path (no ., .., //,
// trailing /), as used for commands inside a package.
func CleanRelative(s string) bool {
	return s != "" && !strings.HasPrefix(s, "/") && path.Clean(s) == s && s != "." &&
		!strings.HasPrefix(s, "../") && s != ".."
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
