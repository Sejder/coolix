package units

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	AppPrefix     = "coolix-app-"
	DBPrefix      = "coolix-db-"
	BackupPrefix  = "coolix-backup-"
	RestorePrefix = "coolix-restore-"
)

var Slots = [2]string{"blue", "green"}

func User(app string) string           { return AppPrefix + app }
func AppUnit(app string) string        { return AppPrefix + app + "@.service" }
func SlotUnit(app, slot string) string { return AppPrefix + app + "@" + slot + ".service" }
func DBUnit(app, kind string) string   { return DBPrefix + app + "-" + kind + ".service" }
func BackupUnit(app string) string     { return BackupPrefix + app + ".service" }
func BackupTimer(app string) string    { return BackupPrefix + app + ".timer" }
func RestoreUnit(app string) string    { return RestorePrefix + app + "@.service" }
func RestoreInstance(app, file string) string {
	return RestorePrefix + app + "@" + file + ".service"
}

// DBSocketDir is where an app's database listens (unix socket only).
func DBSocketDir(app, kind string) string { return "/run/coolix-db/" + app + "-" + kind }

// Owned reports whether a unit file name is one coolix manages.
func Owned(unit string) bool {
	for _, p := range []string{AppPrefix, DBPrefix, BackupPrefix, RestorePrefix} {
		if strings.HasPrefix(unit, p) && (strings.HasSuffix(unit, ".service") || strings.HasSuffix(unit, ".timer")) {
			return true
		}
	}
	return false
}

// AppOfUnit returns the app of an app template unit name, or "".
func AppOfUnit(unit string) string {
	if strings.HasPrefix(unit, AppPrefix) && strings.HasSuffix(unit, "@.service") {
		return strings.TrimSuffix(strings.TrimPrefix(unit, AppPrefix), "@.service")
	}
	return ""
}

// Options are host settings that go into rendered units.
type Options struct {
	// Exec is the coolix binary units run as wrapper (a stable path, so
	// host upgrades don't change every unit).
	Exec       string
	SecretsDir string
	// DefaultResources apply to apps that don't set their own limits.
	DefaultResources Resources
}

type Rendered struct {
	Files    map[string]string // unit file name -> contents
	Services []string          // databases: started before apps, restarted when changed
	Timers   []string
}

// baseHardening applies to every unit coolix renders. Apps may relax the
// keys in relaxable/stringSettings; the rest is fixed.
var baseHardening = [][2]string{
	{"DynamicUser", "yes"},
	{"NoNewPrivileges", "yes"},
	{"CapabilityBoundingSet", ""},
	{"AmbientCapabilities", ""},
	{"ProtectSystem", "strict"},
	{"ProtectHome", "yes"},
	{"PrivateTmp", "yes"},
	{"PrivateDevices", "yes"},
	{"ProtectKernelTunables", "yes"},
	{"ProtectKernelModules", "yes"},
	{"ProtectKernelLogs", "yes"},
	{"ProtectControlGroups", "yes"},
	{"ProtectClock", "yes"},
	{"ProtectHostname", "yes"},
	{"ProtectProc", "invisible"},
	{"RestrictNamespaces", "yes"},
	{"RestrictRealtime", "yes"},
	{"RestrictSUIDSGID", "yes"},
	{"RemoveIPC", "yes"},
	{"LockPersonality", "yes"},
	{"RestrictAddressFamilies", "AF_INET AF_INET6 AF_UNIX"},
	{"SystemCallArchitectures", "native"},
	{"SystemCallFilter", "@system-service"},
	{"UMask", "0077"},
	// The root nix-daemon: apps could build outside their limits or fill
	// the store through it.
	{"InaccessiblePaths", "-/nix/var/nix/daemon-socket"},
}

type unit struct {
	b       strings.Builder
	section string
}

func (u *unit) sect(name string) {
	if u.section != "" {
		u.b.WriteString("\n")
	}
	fmt.Fprintf(&u.b, "[%s]\n", name)
	u.section = name
}

func (u *unit) set(k, v string) { fmt.Fprintf(&u.b, "%s=%s\n", k, v) }

func (u *unit) hardening(overrides map[string]any, extra ...[2]string) {
	for _, kv := range slices.Concat(baseHardening, extra) {
		v := kv[1]
		if o, ok := overrides[kv[0]]; ok {
			switch kv[0] {
			case "RestrictAddressFamilies":
				v += " " + settingValue(o) // additive
			case "SystemCallFilter":
				// A second allow-list line is merged with the first.
				u.set(kv[0], v)
				v = settingValue(o)
			default:
				v = settingValue(o)
			}
		}
		u.set(kv[0], v)
	}
}

func settingValue(v any) string {
	switch v := v.(type) {
	case bool:
		if v {
			return "yes"
		}
		return "no"
	case string:
		return v
	}
	return fmt.Sprint(v)
}

// QuoteEnv renders KEY=value as a quoted Environment= assignment: systemd
// expands specifiers (%) and C escapes inside double quotes.
func QuoteEnv(k, v string) string {
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(k)
	b.WriteByte('=')
	for _, r := range []byte(v) {
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteByte(r)
		case r == '%':
			b.WriteString("%%")
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteByte(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Render turns a validated manifest into unit files.
func Render(m *Manifest, o Options) *Rendered {
	r := &Rendered{Files: map[string]string{}}
	for _, name := range sortedKeys(m.Apps) {
		app := m.Apps[name]
		var dbUnits []string
		for _, db := range app.Databases {
			u := DBUnit(name, db.Kind)
			dbUnits = append(dbUnits, u)
			r.Files[u] = renderDB(name, db, o)
			r.Services = append(r.Services, u)
		}
		r.Files[AppUnit(name)] = renderApp(name, app, dbUnits, o)
		if pg, ok := app.Database("postgres"); ok {
			keep := app.Backup.Keep
			if keep == 0 {
				keep = 7
			}
			r.Files[BackupUnit(name)] = renderBackup(name, pg, keep, o)
			r.Files[RestoreUnit(name)] = renderRestore(name, pg, o)
			if app.Backup.Schedule != "" {
				r.Files[BackupTimer(name)] = renderTimer(name, app.Backup.Schedule)
				r.Timers = append(r.Timers, BackupTimer(name))
			}
		}
	}
	return r
}

func renderApp(name string, app App, dbUnits []string, o Options) string {
	u := &unit{}
	u.sect("Unit")
	u.set("Description", "coolix app "+name+" (%i)")
	u.set("After", strings.Join(append([]string{"network.target"}, dbUnits...), " "))
	if len(dbUnits) > 0 {
		// Wants, not Requires: restarting or removing a database must not
		// restart or stop a serving slot behind the blue/green switch.
		u.set("Wants", strings.Join(dbUnits, " "))
	}

	u.sect("Service")
	u.hardening(app.Hardening, [2]string{"MemoryDenyWriteExecute", "yes"})
	u.set("User", User(name))
	u.set("Type", "simple")

	env := map[string]string{"COOLIX_APP": name}
	for k, v := range app.Env {
		env[k] = v
	}
	if _, ok := app.Database("postgres"); ok {
		dir := DBSocketDir(name, "postgres")
		env["DATABASE_URL"] = "postgresql:///app?host=" + dir
		env["PGHOST"] = dir
		env["PGUSER"] = User(name)
		env["PGDATABASE"] = "app"
	}
	if _, ok := app.Database("redis"); ok {
		env["REDIS_URL"] = "unix://" + DBSocketDir(name, "redis") + "/redis.sock"
	}
	if len(app.Path) > 0 {
		bins := make([]string, len(app.Path))
		for i, p := range app.Path {
			bins[i] = p + "/bin"
		}
		env["COOLIX_PATH"] = strings.Join(bins, ":")
	}
	for _, k := range sortedKeys(env) {
		u.set("Environment", QuoteEnv(k, env[k]))
	}

	// The wrapper sets PORT/COOLIX_SLOT for the instance, loads secrets and
	// execs the app.
	wrap := fmt.Sprintf("%s exec-app --slot %%i --port-base %d -- ", o.Exec, app.PortBase)
	if app.PreStart != "" {
		u.set("ExecStartPre", wrap+app.PreStart)
		timeout := app.PreStartTimeout
		if timeout == "" {
			timeout = "15m"
		}
		// ExecStartPre counts against the start timeout (90s by default).
		u.set("TimeoutStartSec", timeout)
	}
	u.set("ExecStart", wrap+app.Exe)
	u.set("Restart", "on-failure")
	u.set("RestartSec", "2")
	u.set("StateDirectory", "coolix-apps/"+name)
	u.set("WorkingDirectory", "%S/coolix-apps/"+name)
	u.set("SyslogIdentifier", "coolix-"+name)
	if app.Secrets {
		u.set("LoadCredentialEncrypted", "env:"+o.SecretsDir+"/"+name+".cred")
	}
	res := app.Resources
	if res.MemoryMax == "" {
		res.MemoryMax = o.DefaultResources.MemoryMax
	}
	if res.CPUQuota == "" {
		res.CPUQuota = o.DefaultResources.CPUQuota
	}
	if res.TasksMax == 0 {
		res.TasksMax = o.DefaultResources.TasksMax
	}
	if res.MemoryMax != "" {
		u.set("MemoryMax", res.MemoryMax)
	}
	if res.CPUQuota != "" {
		u.set("CPUQuota", res.CPUQuota)
	}
	if res.TasksMax > 0 {
		u.set("TasksMax", strconv.Itoa(res.TasksMax))
	}
	// An app may only listen on its own two ports, so it can't squat
	// another app's idle slot (or the UI) and answer its health checks.
	u.set("SocketBindAllow", fmt.Sprintf("tcp:%d", app.PortBase))
	u.set("SocketBindAllow", fmt.Sprintf("tcp:%d", app.PortBase+1))
	u.set("SocketBindDeny", "any")
	return u.b.String()
}

func dbService(u *unit, name, kind string) {
	dir := "coolix-db/" + name + "-" + kind
	u.hardening(nil)
	u.set("User", User(name))
	u.set("StateDirectory", dir)
	u.set("StateDirectoryMode", "0700")
	u.set("RuntimeDirectory", dir)
	u.set("RuntimeDirectoryMode", "0700")
	u.set("SyslogIdentifier", "coolix-"+name+"-"+kind)
	// Unix sockets only.
	u.set("PrivateNetwork", "yes")
	u.set("SocketBindDeny", "any")
	u.set("Restart", "on-failure")
	u.set("Type", "notify")
}

func renderDB(name string, db Database, o Options) string {
	dir := "coolix-db/" + name + "-" + db.Kind
	u := &unit{}
	u.sect("Unit")
	u.set("Description", "coolix "+db.Kind+" for "+name)
	u.sect("Service")
	dbService(u, name, db.Kind)
	switch db.Kind {
	case "postgres":
		u.set("ExecStartPre", fmt.Sprintf("%s db-init %s %s", o.Exec, db.Package, User(name)))
		u.set("ExecStart", fmt.Sprintf("%s/bin/postgres -D %%S/%s -k %%t/%s -c listen_addresses= -c unix_socket_permissions=0700 -c jit=off", db.Package, dir, dir))
		u.set("ExecStartPost", fmt.Sprintf("%s db-createdb %s %%t/%s", o.Exec, db.Package, dir))
		u.set("KillMode", "mixed")
		u.set("KillSignal", "SIGINT")
		u.set("TimeoutSec", "120")
	case "redis":
		u.set("ExecStart", fmt.Sprintf("%s/bin/redis-server --port 0 --unixsocket %%t/%s/redis.sock --unixsocketperm 700 --dir %%S/%s --supervised systemd --daemonize no", db.Package, dir, dir))
	}
	return u.b.String()
}

func dbJob(u *unit, name string) {
	pg := DBUnit(name, "postgres")
	u.set("Requires", pg)
	u.set("After", pg)
	u.sect("Service")
	u.set("Type", "oneshot")
	u.hardening(nil)
	u.set("User", User(name))
	// Readable by coolix-query (for listing) through a bind mount; the dumps
	// themselves stay 0600.
	u.set("StateDirectory", "coolix-backups/"+name)
	u.set("StateDirectoryMode", "0755")
	u.set("PrivateNetwork", "yes")
	u.set("SocketBindDeny", "any")
	u.set("TimeoutStartSec", "6h")
}

func renderBackup(name string, pg Database, keep int, o Options) string {
	u := &unit{}
	u.sect("Unit")
	u.set("Description", "coolix postgres backup for "+name)
	dbJob(u, name)
	u.set("SyslogIdentifier", "coolix-"+name+"-backup")
	u.set("ExecStart", fmt.Sprintf("%s db-backup %s %s %%S/coolix-backups/%s %d", o.Exec, pg.Package, DBSocketDir(name, "postgres"), name, keep))
	return u.b.String()
}

func renderRestore(name string, pg Database, o Options) string {
	u := &unit{}
	u.sect("Unit")
	u.set("Description", "coolix postgres restore for "+name+" (%i)")
	dbJob(u, name)
	u.set("SyslogIdentifier", "coolix-"+name+"-restore")
	// Don't wait forever for locks the running app holds.
	u.set("Environment", QuoteEnv("PGOPTIONS", "-c lock_timeout=60000"))
	u.set("ExecStart", fmt.Sprintf("%s db-restore %s %s %%S/coolix-backups/%s/%%i", o.Exec, pg.Package, DBSocketDir(name, "postgres"), name))
	return u.b.String()
}

func renderTimer(name, schedule string) string {
	u := &unit{}
	u.sect("Unit")
	u.set("Description", "coolix scheduled postgres backup for "+name)
	u.sect("Timer")
	u.set("OnCalendar", schedule)
	u.set("Persistent", "yes")
	u.set("RandomizedDelaySec", "5min")
	u.set("Unit", BackupUnit(name))
	return u.b.String()
}

// AppOfAnyUnit returns the app a coolix unit file belongs to, or "".
func AppOfAnyUnit(unit string) string {
	if app := AppOfUnit(unit); app != "" {
		return app
	}
	if rest, ok := strings.CutPrefix(unit, DBPrefix); ok {
		for _, kind := range []string{"-postgres.service", "-redis.service"} {
			if app, ok := strings.CutSuffix(rest, kind); ok {
				return app
			}
		}
	}
	if rest, ok := strings.CutPrefix(unit, BackupPrefix); ok {
		for _, suffix := range []string{".service", ".timer"} {
			if app, ok := strings.CutSuffix(rest, suffix); ok {
				return app
			}
		}
	}
	if rest, ok := strings.CutPrefix(unit, RestorePrefix); ok {
		if app, ok := strings.CutSuffix(rest, "@.service"); ok {
			return app
		}
	}
	return ""
}
