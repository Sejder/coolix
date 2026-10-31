package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"coolix/internal/activate"
	"coolix/internal/appdef"
	"coolix/internal/lock"
	"coolix/internal/nixcmd"
	"coolix/internal/profile"
)

func runDeploy(ctx context.Context, c *config, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: coolix deploy [NAME]")
	}
	l, err := lock.Acquire(filepath.Join(c.stateDir, "deploy.lock"))
	if err != nil {
		return err
	}
	defer l.Release()

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
	// The out-link is a GC root protecting the build until activation sets the profile.
	out, err := nixcmd.Build(ctx, r.Dir+"#coolixProfile", filepath.Join(c.stateDir, "build-result"), os.Stderr)
	if err != nil {
		return err
	}
	return requestActivation(ctx, c, out)
}

func runGenerations(c *config) error {
	gens, err := profile.List(c.profile)
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
		fmt.Fprintf(w, "%d%s\t%s\t%s\t%s\n", g.Number, mark, g.Time.Format("2006-01-02 15:04:05"), shortRev(g.Rev), strings.Join(g.Apps, ","))
	}
	return w.Flush()
}

// runRollback re-activates an older generation's store path (no rebuild) and
// then restores the apps repo to the revision it was built from, so the repo
// keeps describing what's running.
func runRollback(ctx context.Context, c *config, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: coolix rollback [GEN]")
	}
	n := 0
	if len(args) == 1 {
		var err error
		if n, err = strconv.Atoi(args[0]); err != nil || n <= 0 {
			return fmt.Errorf("invalid generation %q", args[0])
		}
	}
	l, err := lock.Acquire(filepath.Join(c.stateDir, "deploy.lock"))
	if err != nil {
		return err
	}
	defer l.Release()

	gens, err := profile.List(c.profile)
	if err != nil {
		return err
	}
	g, err := profile.Find(gens, n)
	if err != nil {
		return err
	}
	if g.Current {
		return fmt.Errorf("generation %d is already current", g.Number)
	}
	logf("rolling back to generation %d (%s)", g.Number, shortRev(g.Rev))
	if err := requestActivation(ctx, c, g.Path); err != nil {
		return err
	}
	if g.Rev == "" || g.Rev == "unknown" || strings.HasSuffix(g.Rev, "-dirty") {
		logf("warning: generation %d has no clean repo revision; apps repo left unchanged", g.Number)
		return nil
	}
	r, err := c.repo()
	if err != nil {
		return err
	}
	if err := r.RestoreTree(g.Rev); err != nil {
		return err
	}
	return r.Commit(fmt.Sprintf("Rollback to generation %d (%s)", g.Number, shortRev(g.Rev)))
}

// requestActivation asks root to make storePath the current profile. With a
// coolix user configured this goes through the polkit-gated systemd unit;
// otherwise (development) it runs in-process.
func requestActivation(ctx context.Context, c *config, storePath string) error {
	if c.user == "" {
		return activateStore(ctx, c, storePath)
	}
	unit := "coolix-activate@" + filepath.Base(storePath) + ".service"
	logf("activating via %s", unit)
	startErr := exec.CommandContext(ctx, "systemctl", "start", "--no-ask-password", unit).Run()

	// Show the activation log either way; it's the only output root produced.
	if id, err := exec.Command("systemctl", "show", "-p", "InvocationID", "--value", unit).Output(); err == nil && len(strings.TrimSpace(string(id))) > 0 {
		j := exec.Command("journalctl", "--no-pager", "-o", "cat", "_SYSTEMD_INVOCATION_ID="+strings.TrimSpace(string(id)))
		j.Stdout, j.Stderr = os.Stderr, os.Stderr
		_ = j.Run()
	}
	if startErr != nil {
		return fmt.Errorf("activation failed (%s): %w", unit, startErr)
	}
	return nil
}

// activateStore runs as root: point the profile at the store path and activate it.
func activateStore(ctx context.Context, c *config, name string) error {
	storePath, err := profile.StorePath(name)
	if err != nil {
		return err
	}
	l, err := lock.Acquire("/run/coolix/activate.lock")
	if err != nil {
		return err
	}
	defer l.Release()
	if err := os.MkdirAll(filepath.Dir(c.profile), 0o755); err != nil {
		return err
	}
	if err := nixcmd.SetProfile(ctx, c.profile, storePath); err != nil {
		return err
	}
	return activateProfile(c, storePath)
}

func activateProfile(c *config, profilePath string) error {
	a := &activate.Activator{UnitDir: c.unitDir, Systemd: activate.Systemctl{}, Log: logf}
	res, err := a.Activate(profilePath)
	if res != nil {
		logf("started %v, restarted %v, unchanged %v, removed %v", res.Started, res.Restarted, res.Unchanged, res.Removed)
	}
	return err
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
