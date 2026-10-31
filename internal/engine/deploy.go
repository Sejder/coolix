package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"coolix/internal/activate"
	"coolix/internal/appdef"
	"coolix/internal/caddy"
	"coolix/internal/health"
	"coolix/internal/lock"
	"coolix/internal/nixcmd"
	"coolix/internal/profile"
	"coolix/internal/query"
	"coolix/internal/secrets"
)

// Two locks: the repo lock is held briefly by anything that edits the apps
// repo; the deploy lock is held for a whole build and activation. A long
// build therefore doesn't block edits in the UI.
func (e *Engine) repoLock() (*lock.Lock, error) {
	return lock.Acquire(filepath.Join(e.StateDir, "repo.lock"), "another change to the apps repo")
}

func (e *Engine) deployLock() (*lock.Lock, error) {
	return lock.Acquire(filepath.Join(e.StateDir, "deploy.lock"), "another deploy")
}

// Deploy locks inputs, builds the app profile and activates it. With app set,
// that app's input is first updated to its latest revision.
func (e *Engine) Deploy(ctx context.Context, app string) error {
	dl, err := e.deployLock()
	if err != nil {
		return err
	}
	defer dl.Release()

	rev, err := e.prepare(ctx, app)
	if err != nil {
		return err
	}
	r, err := e.Repo()
	if err != nil {
		return err
	}
	e.logf("building app profile (%s)", ShortRev(rev))
	// Build the committed revision, not the working tree, so edits made
	// meanwhile don't leak into this build. The out-link is a GC root
	// protecting the build until activation sets the profile.
	out, err := nixcmd.Build(ctx, fmt.Sprintf("git+file://%s?rev=%s#coolixProfile", r.Dir, rev),
		filepath.Join(e.StateDir, "build-result"), e.Out)
	if err != nil {
		return err
	}
	return e.requestActivation(ctx, out)
}

// prepare regenerates flake.nix, locks inputs and commits, returning the
// revision to build.
func (e *Engine) prepare(ctx context.Context, app string) (string, error) {
	rl, err := e.repoLock()
	if err != nil {
		return "", err
	}
	defer rl.Release()
	r, err := e.Repo()
	if err != nil {
		return "", err
	}
	if err := r.Init(); err != nil {
		return "", err
	}
	// Regenerate so host upgrades (new nixpkgs/coolix store paths) are picked up.
	if _, err := r.Sync(); err != nil {
		return "", err
	}
	var update []string
	msg := "Deploy"
	if app != "" {
		if _, err := os.Stat(appdef.Path(r.Dir, app)); err != nil {
			return "", fmt.Errorf("app %q does not exist", app)
		}
		update = []string{"app-" + app}
		msg = "Deploy " + app
	}
	// Nix only sees tracked files in a git flake, so commit before locking.
	if err := r.Commit("Sync flake.nix"); err != nil {
		return "", err
	}
	e.logf("locking inputs")
	if err := nixcmd.Lock(ctx, r.Dir, e.Out, update...); err != nil {
		return "", err
	}
	if err := r.Commit(msg); err != nil {
		return "", err
	}
	return r.Head()
}

// Rollback re-activates an older generation's store path (no rebuild) and
// then restores the apps repo to the revision it was built from, so the repo
// keeps describing what's running. gen 0 means the previous generation.
func (e *Engine) Rollback(ctx context.Context, gen int) error {
	dl, err := e.deployLock()
	if err != nil {
		return err
	}
	defer dl.Release()

	gens, err := profile.List(e.Profile)
	if err != nil {
		return err
	}
	g, err := profile.Find(gens, gen)
	if err != nil {
		return err
	}
	if g.Current {
		return fmt.Errorf("generation %d is already current", g.Number)
	}
	e.logf("rolling back to generation %d (%s)", g.Number, ShortRev(g.Rev))
	if err := e.requestActivation(ctx, g.Path); err != nil {
		return err
	}
	if g.Rev == "" || g.Rev == "unknown" || strings.HasSuffix(g.Rev, "-dirty") {
		e.logf("warning: generation %d has no clean repo revision; apps repo left unchanged", g.Number)
		return nil
	}
	rl, err := e.repoLock()
	if err != nil {
		return err
	}
	defer rl.Release()
	r, err := e.Repo()
	if err != nil {
		return err
	}
	if err := r.RestoreTree(g.Rev); err != nil {
		return err
	}
	return r.Commit(fmt.Sprintf("Rollback to generation %d (%s)", g.Number, ShortRev(g.Rev)))
}

// requestActivation asks root to make storePath the current profile. With a
// coolix user configured this goes through the polkit-gated systemd unit;
// otherwise (development) it runs in-process.
func (e *Engine) requestActivation(ctx context.Context, storePath string) error {
	if e.User == "" {
		return e.ActivateStore(ctx, storePath)
	}
	unit := "coolix-activate@" + filepath.Base(storePath) + ".service"
	e.logf("activating via %s", unit)
	if err := e.runUnit(ctx, unit); err != nil {
		return fmt.Errorf("activation failed (%s): %w", unit, err)
	}
	return nil
}

// runUnit starts a oneshot unit (which polkit lets the coolix user start)
// and streams its journal while it runs: that's the only output root
// produces. Reading it afterwards is unreliable; finished oneshot units are
// unloaded together with their invocation ID.
func (e *Engine) runUnit(ctx context.Context, unit string) error {
	fctx, stop := context.WithCancel(ctx)
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		if err := e.Query().Stream(fctx, query.Request{Kind: "unit", Unit: unit}, e.Out); err != nil {
			e.logf("warning: can't follow %s: %v", unit, err)
		}
	}()
	time.Sleep(200 * time.Millisecond) // let the follower attach
	err := exec.CommandContext(ctx, "systemctl", "start", "--no-ask-password", unit).Run()
	time.Sleep(500 * time.Millisecond) // let journald deliver the last lines
	stop()
	<-followed
	return err
}

// ActivateStore runs as root: point the profile at the store path, apply
// pending secrets and activate. On failure the previous generation is restored.
func (e *Engine) ActivateStore(ctx context.Context, name string) error {
	storePath, err := profile.StorePath(name)
	if err != nil {
		return err
	}
	l, err := lock.Acquire("/run/coolix/activate.lock", "another activation")
	if err != nil {
		return err
	}
	defer l.Release()
	a, err := e.activator()
	if err != nil {
		return err
	}
	// Refuse invalid profiles before they become a generation.
	if _, err := a.Load(storePath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.Profile), 0o755); err != nil {
		return err
	}
	prev, _ := filepath.EvalSymlinks(e.Profile)
	if prev != storePath {
		if err := nixcmd.SetProfile(ctx, e.Profile, storePath); err != nil {
			return err
		}
	}
	force, err := e.applyPendingSecrets(ctx, a, storePath)
	if err != nil {
		return err
	}
	if err := e.Activate(ctx, storePath, force); err != nil {
		if prev == "" || prev == storePath {
			return err
		}
		// Unhealthy apps already kept their old slot; re-activating the previous
		// generation moves any apps that did switch back, so the current
		// generation again describes what runs.
		e.logf("activation failed; restoring the previous generation")
		if rerr := nixcmd.Rollback(ctx, e.Profile); rerr != nil {
			return errors.Join(err, rerr)
		}
		if rerr := e.Activate(ctx, prev, nil); rerr != nil {
			return errors.Join(err, rerr)
		}
		return fmt.Errorf("deploy failed, previous generation restored: %w", err)
	}
	if e.KeepGenerations > 0 {
		if err := nixcmd.DeleteOldGenerations(ctx, e.Profile, e.KeepGenerations); err != nil {
			e.logf("warning: %v", err)
		}
	}
	return nil
}

func (e *Engine) activator() (*activate.Activator, error) {
	o := e.Options()
	if o.Exec == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		o.Exec = self
	}
	a := &activate.Activator{
		UnitDir:   e.UnitDir,
		RenderDir: e.RenderDir,
		Policy:    e.Policy(),
		Options:   o,
		Systemd:   activate.Systemctl{},
		Health:    health.Check,
		Drain:     e.Drain,
		Log:       e.logf,
	}
	if e.CaddyDir != "" {
		a.Router = &caddy.Router{Dir: e.CaddyDir, TLS: caddy.TLSMode(e.TLS), Reload: caddy.SystemdReload}
	}
	return a, nil
}

// Activate (root) makes profilePath live; force lists apps to restart anyway.
func (e *Engine) Activate(ctx context.Context, profilePath string, force map[string]bool) error {
	a, err := e.activator()
	if err != nil {
		return err
	}
	a.ForceRestart = force
	res, err := a.Activate(ctx, profilePath)
	if res != nil {
		e.logf("started %v, switched %v, unchanged %v, removed %v, failed %v",
			res.Started, res.Switched, res.Unchanged, res.Removed, res.Failed)
	}
	return err
}

// applyPendingSecrets runs as root: it encrypts pending secret changes into the
// credential store and reports which apps need a restart because of them.
func (e *Engine) applyPendingSecrets(ctx context.Context, a *activate.Activator, storePath string) (map[string]bool, error) {
	m, err := a.Load(storePath)
	if err != nil {
		return nil, err
	}
	var apps []string
	need := map[string]bool{}
	for name, app := range m.Apps {
		apps = append(apps, name)
		need[name] = app.Secrets
	}
	changed, err := secrets.NewSystemdStore(e.SecretsDir).Apply(ctx, e.PendingDir(), apps, need)
	for app := range changed {
		e.logf("%s: secrets updated", app)
	}
	return changed, err
}
