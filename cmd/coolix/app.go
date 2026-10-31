package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"coolix/internal/appdef"
)

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
