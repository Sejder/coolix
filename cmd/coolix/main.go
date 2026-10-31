// Command coolix manages apps on a NixOS host: it edits the apps repo, builds
// the app profile with Nix and activates it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"

	"coolix/internal/activate"
	"coolix/internal/appdef"
	"coolix/internal/nixcmd"
	"coolix/internal/repo"
)

const usage = `usage: coolix <command> [args]

commands:
  init                         create the apps repo
  app add NAME --source REF    add an app (flags: --domain, --package, --command, --env K=V)
  app rm NAME                  remove an app (applied on next deploy)
  app list                     list apps
  deploy [NAME]                lock, build and activate; with NAME, update that app to its latest revision
  activate                     re-activate the current profile (used at boot)
`

type config struct {
	stateDir         string
	nixpkgs, lib     string
	system           string
	portMin, portMax int
	profile, unitDir string
}

func loadConfig() (*config, error) {
	c := &config{
		stateDir: envOr("COOLIX_STATE_DIR", "/var/lib/coolix"),
		nixpkgs:  os.Getenv("COOLIX_NIXPKGS"),
		lib:      os.Getenv("COOLIX_LIB"),
		system:   os.Getenv("COOLIX_SYSTEM"),
		profile:  envOr("COOLIX_PROFILE", activate.DefaultProfile),
		unitDir:  envOr("COOLIX_UNIT_DIR", activate.DefaultUnitDir),
	}
	var err error
	if c.portMin, err = strconv.Atoi(envOr("COOLIX_PORT_MIN", "20000")); err != nil {
		return nil, fmt.Errorf("COOLIX_PORT_MIN: %w", err)
	}
	if c.portMax, err = strconv.Atoi(envOr("COOLIX_PORT_MAX", "29999")); err != nil {
		return nil, fmt.Errorf("COOLIX_PORT_MAX: %w", err)
	}
	return c, nil
}

func (c *config) repo() (*repo.Repo, error) {
	if c.nixpkgs == "" || c.lib == "" || c.system == "" {
		return nil, errors.New("COOLIX_NIXPKGS, COOLIX_LIB and COOLIX_SYSTEM must be set (the NixOS module's wrapper does this)")
	}
	return &repo.Repo{Dir: c.stateDir + "/repo", Nixpkgs: c.nixpkgs, Lib: c.lib, System: c.system}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logf(format string, args ...any) { fmt.Fprintf(os.Stderr, "coolix: "+format+"\n", args...) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "coolix:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "init":
		r, err := c.repo()
		if err != nil {
			return err
		}
		return r.Init()
	case "app":
		return runApp(c, args[1:])
	case "deploy":
		return runDeploy(ctx, c, args[1:])
	case "activate":
		return runActivate(c, c.profile)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runApp(c *config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coolix app add|rm|list")
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if err := r.Init(); err != nil {
		return err
	}
	switch args[0] {
	case "list":
		apps, err := appdef.LoadAll(r.Dir)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSOURCE\tPORTS\tDOMAINS")
		for _, a := range apps {
			fmt.Fprintf(w, "%s\t%s\t%d,%d\t%s\n", a.Name, a.Source, a.PortBase, a.PortBase+1, strings.Join(a.Domains, ","))
		}
		return w.Flush()

	case "add":
		fs := flag.NewFlagSet("app add", flag.ContinueOnError)
		source := fs.String("source", "", "flake reference of the app")
		pkg := fs.String("package", "", "package attribute (default: default)")
		command := fs.String("command", "", "executable path inside the package (default: mainProgram)")
		var domains, envs multiFlag
		fs.Var(&domains, "domain", "domain to route to the app (repeatable)")
		fs.Var(&envs, "env", "KEY=VALUE environment variable (repeatable)")
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: coolix app add NAME --source REF [flags]")
		}
		name := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if _, err := os.Stat(appdef.Path(r.Dir, name)); err == nil {
			return fmt.Errorf("app %q already exists", name)
		}
		existing, err := appdef.LoadAll(r.Dir)
		if err != nil {
			return err
		}
		port, err := appdef.AllocatePort(existing, c.portMin, c.portMax)
		if err != nil {
			return err
		}
		app := &appdef.App{Name: name, Source: *source, Package: *pkg, Command: *command, Domains: domains, PortBase: port}
		for _, kv := range envs {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--env %q: want KEY=VALUE", kv)
			}
			if app.Env == nil {
				app.Env = map[string]string{}
			}
			app.Env[k] = v
		}
		if err := appdef.Save(r.Dir, app); err != nil {
			return err
		}
		if _, err := r.Sync(); err != nil {
			return err
		}
		if err := r.Commit("Add app " + name); err != nil {
			return err
		}
		logf("added %s on ports %d/%d; run `coolix deploy %s` to start it", name, port, port+1, name)
		return nil

	case "rm":
		if len(args) != 2 {
			return errors.New("usage: coolix app rm NAME")
		}
		if err := r.RemoveApp(args[1]); err != nil {
			return err
		}
		if _, err := r.Sync(); err != nil {
			return err
		}
		if err := r.Commit("Remove app " + args[1]); err != nil {
			return err
		}
		logf("removed %s; run `coolix deploy` to stop it", args[1])
		return nil
	}
	return fmt.Errorf("unknown app command %q", args[0])
}

func runDeploy(ctx context.Context, c *config, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: coolix deploy [NAME]")
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if err := r.Init(); err != nil {
		return err
	}
	// Regenerate so host upgrades (new nixpkgs/coolix store paths) are picked up.
	if _, err := r.Sync(); err != nil {
		return err
	}
	var update []string
	msg := "Deploy"
	if len(args) == 1 {
		if _, err := os.Stat(appdef.Path(r.Dir, args[0])); err != nil {
			return fmt.Errorf("app %q does not exist", args[0])
		}
		update = []string{"app-" + args[0]}
		msg = "Deploy " + args[0]
	}
	// Nix only sees tracked files in a git flake, so commit before locking.
	if err := r.Commit("Sync flake.nix"); err != nil {
		return err
	}
	logf("locking inputs")
	if err := nixcmd.Lock(ctx, r.Dir, os.Stderr, update...); err != nil {
		return err
	}
	if err := r.Commit(msg); err != nil {
		return err
	}
	logf("building app profile")
	out, err := nixcmd.Build(ctx, r.Dir+"#coolixProfile", os.Stderr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(c.profile), 0o755); err != nil {
		return err
	}
	if err := nixcmd.SetProfile(ctx, c.profile, out); err != nil {
		return err
	}
	return runActivate(c, out)
}

func runActivate(c *config, profile string) error {
	a := &activate.Activator{UnitDir: c.unitDir, Systemd: activate.Systemctl{}, Log: logf}
	res, err := a.Activate(profile)
	if res != nil {
		logf("started %v, restarted %v, unchanged %v, removed %v", res.Started, res.Restarted, res.Unchanged, res.Removed)
	}
	return err
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
