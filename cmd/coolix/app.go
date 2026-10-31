package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"coolix/internal/appdef"
	"coolix/internal/engine"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// appFlags are shared by `app add` and `app set`; only flags that were given
// change the app.
type appFlags struct {
	fs                        *flag.FlagSet
	source, pkg, command      *string
	healthPath, healthTimeout *string
	domains, envs, unsetEnvs  multiFlag
	dbs                       multiFlag
}

func newAppFlags(name string) *appFlags {
	f := &appFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	f.source = f.fs.String("source", "", "flake reference of the app")
	f.pkg = f.fs.String("package", "", "package attribute (default: default)")
	f.command = f.fs.String("command", "", "executable path inside the package (default: mainProgram)")
	f.healthPath = f.fs.String("health-path", "", "HTTP path that returns < 400 when the app is ready (default: TCP check)")
	f.healthTimeout = f.fs.String("health-timeout", "", "how long a new slot may take to become healthy (default 30s)")
	f.fs.Var(&f.domains, "domain", "domain to route to the app (repeatable; replaces existing domains)")
	f.fs.Var(&f.envs, "env", "KEY=VALUE environment variable (repeatable)")
	f.fs.Var(&f.unsetEnvs, "unset-env", "remove an environment variable (repeatable)")
	f.fs.Var(&f.dbs, "db", "database: postgres[:VERSION] or redis (repeatable; replaces existing; \"none\" removes all)")
	return f
}

func (f *appFlags) apply(app *appdef.App) error {
	set := map[string]bool{}
	f.fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if set["source"] {
		app.Source = *f.source
	}
	if set["package"] {
		app.Package = *f.pkg
	}
	if set["command"] {
		app.Command = *f.command
	}
	if set["domain"] {
		app.Domains = f.domains
	}
	if set["health-path"] || set["health-timeout"] {
		if app.Health == nil {
			app.Health = &appdef.Health{}
		}
		if set["health-path"] {
			app.Health.Path = *f.healthPath
		}
		if set["health-timeout"] {
			app.Health.Timeout = *f.healthTimeout
		}
	}
	for _, kv := range f.envs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--env %q: want KEY=VALUE", kv)
		}
		if app.Env == nil {
			app.Env = map[string]string{}
		}
		app.Env[k] = v
	}
	for _, k := range f.unsetEnvs {
		delete(app.Env, k)
	}
	if set["db"] {
		app.Databases = nil
		for _, d := range f.dbs {
			if d == "none" {
				continue
			}
			kind, version, _ := strings.Cut(d, ":")
			app.Databases = append(app.Databases, appdef.Database{Kind: kind, Version: version})
		}
	}
	return nil
}

func runApp(e *engine.Engine, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: coolix app add|set|rm|list")
	}
	switch args[0] {
	case "list":
		apps, err := e.Apps()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSOURCE\tPORTS\tDOMAINS")
		for _, a := range apps {
			fmt.Fprintf(w, "%s\t%s\t%d,%d\t%s\n", a.Name, a.Source, a.PortBase, a.PortBase+1, strings.Join(a.Domains, ","))
		}
		return w.Flush()

	case "add", "set":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("usage: coolix app %s NAME [flags]", args[0])
		}
		name := args[1]
		f := newAppFlags("app " + args[0])
		if err := f.fs.Parse(args[2:]); err != nil {
			return err
		}
		if args[0] == "add" {
			app := &appdef.App{Name: name}
			if err := f.apply(app); err != nil {
				return err
			}
			app.PortBase = 1 // placeholder so validation passes; CreateApp allocates the real one
			if err := app.Validate(); err != nil {
				return err
			}
			if err := e.CreateApp(app); err != nil {
				return err
			}
			logf("added %s on ports %d/%d; run `coolix deploy %s` to start it", name, app.PortBase, app.PortBase+1, name)
			return nil
		}
		app, err := e.App(name)
		if err != nil {
			return err
		}
		if err := f.apply(app); err != nil {
			return err
		}
		if err := e.UpdateApp(app); err != nil {
			return err
		}
		logf("updated %s; run `coolix deploy %s` to apply", name, name)
		return nil

	case "rm":
		if len(args) != 2 {
			return errors.New("usage: coolix app rm NAME")
		}
		if err := e.RemoveApp(args[1]); err != nil {
			return err
		}
		logf("removed %s; run `coolix deploy` to stop it", args[1])
		return nil
	}
	return fmt.Errorf("unknown app command %q", args[0])
}
