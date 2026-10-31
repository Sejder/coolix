// Command coolix manages apps on a NixOS host: it edits the apps repo, builds
// the app profile with Nix and activates it.
//
// Privileges: repo edits and builds run as the unprivileged coolix user (root
// invocations drop to it). Only activation runs as root, inside the
// coolix-activate@<profile>.service unit, which polkit lets the coolix user start.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"coolix/internal/engine"
)

const usage = `usage: coolix <command> [args]

commands:
  app add NAME --source REF    add an app (see app set for flags)
  app set NAME [flags]         change an app: --source, --domain (repeatable), --env K=V, --unset-env K,
                               --health-path, --health-timeout, --package, --command,
                               --db postgres[:VERSION] / --db redis / --db none
  app rm NAME                  remove an app (applied on next deploy)
  app list                     list apps
  secret set APP KEY           set a secret (value read from stdin; applied on next deploy)
  secret unset APP KEY         remove a secret
  secret list APP              list secret names (values are write-only)
  deploy [NAME]                lock, build and activate; with NAME, update that app to its latest revision
  status                       show running apps, their slots and whether the repo has undeployed changes
  generations                  list app profile generations
  rollback [GEN]               switch back to generation GEN (default: the previous one)
  serve                        run the web UI

internal (run as root by systemd):
  activate                     re-activate the current profile (at boot)
  activate-store NAME          make /nix/store/NAME the current profile and activate it
`

func logf(format string, args ...any) { fmt.Fprintf(os.Stderr, "coolix: "+format+"\n", args...) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	c, err := engine.ConfigFromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "activate", "activate-store", "help", "-h", "--help":
	default:
		if err := dropPrivileges(c); err != nil {
			return err
		}
	}
	e := &engine.Engine{Config: c, Out: os.Stderr}
	switch args[0] {
	case "app":
		return runApp(e, args[1:])
	case "secret":
		return runSecret(e, args[1:])
	case "deploy":
		if len(args) > 2 {
			return errors.New("usage: coolix deploy [NAME]")
		}
		app := ""
		if len(args) == 2 {
			app = args[1]
		}
		return e.Deploy(ctx, app)
	case "status":
		return runStatus(e)
	case "generations":
		return runGenerations(e)
	case "rollback":
		if len(args) > 2 {
			return errors.New("usage: coolix rollback [GEN]")
		}
		n := 0
		if len(args) == 2 {
			if n, err = strconv.Atoi(args[1]); err != nil || n <= 0 {
				return fmt.Errorf("invalid generation %q", args[1])
			}
		}
		return e.Rollback(ctx, n)
	case "serve":
		return runServe(ctx, e)
	case "activate":
		return e.Activate(ctx, c.Profile, nil)
	case "activate-store":
		if len(args) != 2 {
			return errors.New("usage: coolix activate-store NAME")
		}
		return e.ActivateStore(ctx, args[1])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// dropPrivileges re-executes as the coolix user when invoked as root, so the
// repo and builds are never touched by root.
func dropPrivileges(c *engine.Config) error {
	if c.User == "" || os.Geteuid() != 0 {
		return nil
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Setenv("HOME", c.StateDir); err != nil {
		return err
	}
	argv := append([]string{setpriv, "--reuid=" + c.User, "--regid=" + c.User, "--init-groups", "--", self}, os.Args[1:]...)
	return syscall.Exec(setpriv, argv, os.Environ())
}

func runStatus(e *engine.Engine) error {
	st, err := e.Status()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "APP\tSLOT\tPORT\tDOMAINS")
	for _, a := range st.Apps {
		slot, port := "stopped", "-"
		if !a.Deployed {
			slot = "not deployed"
		} else if a.Slot != "" {
			slot, port = a.Slot, strconv.Itoa(a.Port)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Name, slot, port, strings.Join(a.Domains, ","))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	switch {
	case st.RunningRev == "":
		fmt.Println("\nnothing deployed yet")
	case !st.UpToDate:
		fmt.Printf("\nrunning %s, repo is at %s: run `coolix deploy` to apply (or a deploy failed)\n",
			engine.ShortRev(st.RunningRev), engine.ShortRev(st.HeadRev))
	default:
		fmt.Printf("\nrunning %s (up to date)\n", engine.ShortRev(st.RunningRev))
	}
	return nil
}

func runGenerations(e *engine.Engine) error {
	gens, err := e.Generations()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "GEN\tDATE\tREV\tAPPS")
	for _, g := range gens {
		mark := ""
		if g.Current {
			mark = " (current)"
		}
		fmt.Fprintf(w, "%d%s\t%s\t%s\t%s\n", g.Number, mark, g.Time.Format("2006-01-02 15:04:05"), engine.ShortRev(g.Rev), strings.Join(g.Apps, ","))
	}
	return w.Flush()
}
