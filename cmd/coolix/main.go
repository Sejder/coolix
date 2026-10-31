// Command coolix manages apps on a NixOS host: it edits the apps repo, builds
// the app profile with Nix and activates it.
//
// Privileges: repo edits and builds run as the unprivileged coolix user (root
// invocations drop to it). Activation and database backups/restores run as
// root in coolix-control@ instances, which the coolix user reaches through
// /run/coolix/control.sock; root validates every request and profile.
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

	"coolix/internal/control"
	"coolix/internal/engine"
)

const usage = `usage: coolix <command> [args]

commands:
  app add NAME --source REF    add an app (see app set for flags)
  app set NAME [flags]         change an app: --source, --domain (repeatable), --env K=V, --unset-env K,
                               --health-path, --health-timeout, --package, --command, --pre-start,
                               --path ATTR (repeatable), --memory-max, --cpu-quota, --tasks-max,
                               --db postgres[:VERSION] / --db redis / --db none,
                               --backup-schedule, --backup-keep, --webhook-branch
  app rm NAME                  remove an app (applied on next deploy)
  app list                     list apps
  secret set APP KEY           set a secret (value read from stdin; applied on next deploy)
  secret unset APP KEY         remove a secret
  secret list APP              list secret names (values are write-only)
  deploy [NAME]                lock, build and activate; with NAME, update that app to its latest revision
  status                       show running apps, their slots and whether the repo has undeployed changes
  logs APP [-f] [-n LINES]     show (or follow) an app's logs, including its databases
  generations                  list app profile generations
  rollback [GEN]               switch back to generation GEN (default: the previous one)
  db backup APP                dump the app's postgres database now
  db backups APP               list the app's database backups
  db restore APP BACKUP        replace the app's database with a backup
  webhook enable APP           create (or rotate) the app's deploy webhook token and print it
  webhook show APP             print the webhook URL path and token
  webhook disable APP          turn the app's webhook off
  serve                        run the web UI
  setup-code                   print a one-time code to create the UI's admin account
  account reset                remove the admin's password and passkeys (then use setup-code)

internal (run by systemd):
  activate                     re-activate the current profile (root, at boot)
  control-serve                answer one coolix-control connection (root): activate a
                               profile, back up or restore a database
  exec-app, db-init, db-createdb, db-backup, db-restore
                               helpers inside app and database units (app user)
  query-serve                  answer one coolix-query connection
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
	if f, ok := sandboxCommands[args[0]]; ok {
		return f(ctx, args[1:])
	}
	c, err := engine.ConfigFromEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "activate", "control-serve", "help", "-h", "--help":
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
	case "logs":
		return runLogs(ctx, e, args[1:])
	case "db":
		return runDB(ctx, e, args[1:])
	case "webhook":
		return runWebhook(e, args[1:])
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
	case "setup-code":
		return runSetupCode(e)
	case "account":
		return runAccount(e, args[1:])
	case "serve":
		return runServe(ctx, e)
	case "activate":
		return e.ActivateBoot(ctx)
	case "control-serve":
		if os.Geteuid() != 0 {
			return errors.New("control-serve must run as root")
		}
		// One connection on stdin/stdout (coolix-control@); the journal
		// (stderr) keeps an audit trail. Not cancelled when the client
		// goes away: a half-done activation is worse than an unwatched one.
		return control.Serve(context.WithoutCancel(ctx), os.Stdin, os.Stdout, os.Stderr, e.HandleControl)
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
	case st.OldProfile:
		fmt.Println("\nthe running profile was built by an older coolix and won't start at boot: run `coolix deploy`")
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
