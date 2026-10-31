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
	"syscall"

	"coolix/internal/activate"
	"coolix/internal/repo"
)

const usage = `usage: coolix <command> [args]

commands:
  init                         create the apps repo
  app add NAME --source REF    add an app (flags: --domain, --package, --command, --env K=V)
  app rm NAME                  remove an app (applied on next deploy)
  app list                     list apps
  deploy [NAME]                lock, build and activate; with NAME, update that app to its latest revision
  generations                  list app profile generations
  rollback [GEN]               switch back to generation GEN (default: the previous one)

internal (run as root by systemd):
  activate                     re-activate the current profile (at boot)
  activate-store NAME          make /nix/store/NAME the current profile and activate it
`

type config struct {
	stateDir         string
	user             string // unprivileged user owning stateDir; empty = run everything in-process
	nixpkgs, lib     string
	system           string
	portMin, portMax int
	profile, unitDir string
}

func loadConfig() (*config, error) {
	c := &config{
		stateDir: envOr("COOLIX_STATE_DIR", "/var/lib/coolix"),
		user:     os.Getenv("COOLIX_USER"),
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
	case "activate", "activate-store", "help", "-h", "--help":
	default:
		if err := c.dropPrivileges(); err != nil {
			return err
		}
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
	case "generations":
		return runGenerations(c)
	case "rollback":
		return runRollback(ctx, c, args[1:])
	case "activate":
		return activateProfile(c, c.profile)
	case "activate-store":
		if len(args) != 2 {
			return errors.New("usage: coolix activate-store NAME")
		}
		return activateStore(ctx, c, args[1])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// dropPrivileges re-executes as the coolix user when invoked as root, so the
// repo and builds are never touched by root.
func (c *config) dropPrivileges() error {
	if c.user == "" || os.Geteuid() != 0 {
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
	if err := os.Setenv("HOME", c.stateDir); err != nil {
		return err
	}
	argv := append([]string{setpriv, "--reuid=" + c.user, "--regid=" + c.user, "--init-groups", "--", self}, os.Args[1:]...)
	return syscall.Exec(setpriv, argv, os.Environ())
}
