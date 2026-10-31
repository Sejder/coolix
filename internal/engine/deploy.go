package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"coolix/internal/activate"
	"coolix/internal/appdef"
	"coolix/internal/caddy"
	"coolix/internal/control"
	"coolix/internal/health"
	"coolix/internal/lock"
	"coolix/internal/nixcmd"
	"coolix/internal/profile"
	"coolix/internal/query"
	"coolix/internal/secrets"
	"coolix/internal/units"
)

const activateLock = "/run/coolix/activate.lock"

// Two locks: the repo lock is held briefly by anything that edits the apps
// repo; the deploy lock is held for a whole deploy. Locking inputs and
// building happen in a private clone, so a long deploy doesn't block edits.
func (e *Engine) repoLock() (*lock.Lock, error) {
	return lock.Acquire(filepath.Join(e.StateDir, "repo.lock"), "another change to the apps repo", e.Out)
}

func (e *Engine) deployLock() (*lock.Lock, error) {
	return lock.Acquire(filepath.Join(e.StateDir, "deploy.lock"), "another deploy", e.Out)
}

// Deploy locks inputs, builds the app profile and activates it. With app set,
// that app's input is first updated to its latest revision.
func (e *Engine) Deploy(ctx context.Context, app string) error {
	dl, err := e.deployLock()
	if err != nil {
		return err
	}
	defer dl.Release()
	if e.OnDeployStart != nil {
		e.OnDeployStart()
	}

	rev, err := e.prepare(ctx, app)
	if err != nil {
		return err
	}
	r, err := e.Repo()
	if err != nil {
		return err
	}
	e.logf("building app profile (%s)", ShortRev(rev))
	// Build that exact commit: edits made meanwhile don't leak into it. The
	// out-link is a GC root protecting the build until activation sets the
	// profile.
	out, err := nixcmd.Build(ctx, fmt.Sprintf("git+file://%s?rev=%s#coolixProfile", r.Dir, rev),
		filepath.Join(e.StateDir, "build-result"), e.Out)
	if err != nil {
		return err
	}
	return e.requestActivation(ctx, out)
}

// prepare regenerates flake.nix and commits (briefly holding the repo lock),
// then locks inputs in a private clone (no lock held: this fetches from the
// network) and brings the new flake.lock back. It returns the revision to
// build.
func (e *Engine) prepare(ctx context.Context, app string) (string, error) {
	r, err := e.Repo()
	if err != nil {
		return "", err
	}
	msg := "Deploy"
	if app != "" {
		msg = "Deploy " + app
	}
	base, err := func() (string, error) {
		rl, err := e.repoLock()
		if err != nil {
			return "", err
		}
		defer rl.Release()
		if err := r.Init(); err != nil {
			return "", err
		}
		if app != "" {
			if _, err := os.Stat(appdef.Path(r.Dir, app)); err != nil {
				return "", fmt.Errorf("app %q does not exist", app)
			}
		}
		// Regenerate so host upgrades (new nixpkgs/coolix store paths) are picked up.
		if _, err := r.Sync(); err != nil {
			return "", err
		}
		// Nix only sees tracked files in a git flake, so commit before locking.
		if err := r.Commit("Sync flake.nix"); err != nil {
			return "", err
		}
		return r.Head()
	}()
	if err != nil {
		return "", err
	}

	clone, err := r.Clone(filepath.Join(e.StateDir, "deploy-tree"), base)
	if err != nil {
		return "", err
	}
	var update []string
	if app != "" {
		update = []string{"app-" + app}
	}
	e.logf("locking inputs")
	if err := nixcmd.Lock(ctx, clone.Dir, e.Out, update...); err != nil {
		return "", err
	}
	if err := clone.Commit(msg); err != nil {
		return "", err
	}

	rl, err := e.repoLock()
	if err != nil {
		return "", err
	}
	defer rl.Release()
	return r.Adopt(clone, base, msg)
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

// requestActivation asks root to make storePath the current profile, through
// coolix-control. Without a coolix user (development) it runs in-process.
func (e *Engine) requestActivation(ctx context.Context, storePath string) error {
	return e.privileged(ctx, control.Request{Verb: "activate", Arg: filepath.Base(storePath)})
}

// privileged runs a control request: over the control socket when there is
// an unprivileged coolix user, in-process otherwise.
func (e *Engine) privileged(ctx context.Context, r control.Request) error {
	if e.User == "" {
		return e.HandleControl(ctx, r, e.Out)
	}
	if e.ControlSocket == "" {
		return errors.New("COOLIX_CONTROL_SOCKET is not set (the NixOS module's wrapper sets it)")
	}
	return control.Do(ctx, e.ControlSocket, r, e.Out)
}

// HandleControl performs a control request as root (coolix-control@).
func (e *Engine) HandleControl(ctx context.Context, r control.Request, w io.Writer) error {
	root := &Engine{Config: e.Config, Out: w}
	switch r.Verb {
	case "activate":
		return root.ActivateStore(ctx, r.Arg)
	case "backup":
		return root.runDBJob(ctx, r.Arg, units.BackupUnit(r.Arg))
	case "restore":
		return root.runDBJob(ctx, r.Arg, units.RestoreInstance(r.Arg, r.File))
	}
	return fmt.Errorf("unknown request %q", r.Verb)
}

// runDBJob (root) starts a rendered backup or restore unit of a deployed app
// with postgres, streaming its journal.
func (e *Engine) runDBJob(ctx context.Context, app, unit string) error {
	m, err := units.ReadManifest(e.Profile)
	if err != nil {
		return fmt.Errorf("no deployed profile: %w", err)
	}
	a, ok := m.Apps[app]
	if !ok {
		return fmt.Errorf("app %s is not deployed", app)
	}
	if _, ok := a.Database("postgres"); !ok {
		return fmt.Errorf("app %s has no deployed postgres database", app)
	}
	e.logf("starting %s", unit)
	return e.runUnit(ctx, unit)
}

// runUnit (root) starts a oneshot unit and streams its journal while it
// runs. The follower starts from a timestamp taken before the unit starts,
// and keeps going after it finished until the journal is quiet, so neither
// the first nor the last lines (usually the error) are lost.
func (e *Engine) runUnit(ctx context.Context, unit string) error {
	since := time.Now().Add(-time.Second).Unix()
	fctx, stop := context.WithCancel(ctx)
	defer stop()
	qw := &quietWriter{w: e.Out, last: time.Now()}
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		query.Journal(fctx, qw, "--no-pager", "--follow", "--output=cat",
			fmt.Sprintf("--since=@%d", since), "--unit="+unit)
	}()
	out, err := exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput()
	finished := time.Now()
	// Wait for journald to deliver the rest: until 1s without output since
	// the unit finished (at least 1s), at most 10s.
	deadline := finished.Add(10 * time.Second)
	quietSince := func() time.Time {
		if t := qw.lastWrite(); t.After(finished) {
			return t
		}
		return finished
	}
	for time.Now().Before(deadline) && time.Since(quietSince()) < time.Second {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-followed
	if err != nil {
		return fmt.Errorf("%s failed: %w %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type quietWriter struct {
	mu   sync.Mutex
	w    io.Writer
	last time.Time
}

func (q *quietWriter) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.last = time.Now()
	return q.w.Write(p)
}

func (q *quietWriter) lastWrite() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.last
}

// ActivateStore runs as root: apply pending secrets, make the store path the
// current profile generation and activate it. If activation fails, the
// previous generation is restored and the failed one deleted.
func (e *Engine) ActivateStore(ctx context.Context, name string) error {
	storePath, err := profile.StorePath(name)
	if err != nil {
		return err
	}
	l, err := lock.Acquire(activateLock, "another activation", e.Out)
	if err != nil {
		return err
	}
	defer l.Release()
	a, err := e.activator()
	if err != nil {
		return err
	}
	// Refuse invalid profiles before they become a generation.
	m, err := a.Load(storePath)
	if err != nil {
		return err
	}
	// Secrets first: if this fails, nothing has changed yet.
	force, err := e.applyPendingSecrets(ctx, m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.Profile), 0o755); err != nil {
		return err
	}
	prev, _ := filepath.EvalSymlinks(e.Profile)
	prevGen := currentGeneration(e.Profile)
	if prev != storePath {
		if err := nixcmd.SetProfile(ctx, e.Profile, storePath); err != nil {
			return err
		}
	}
	res, err := e.activateResult(ctx, a, storePath, force)
	if err == nil {
		store := secrets.NewSystemdStore(e.SecretsDir)
		for app := range force {
			e.warnf(store.ClearRestart(app))
		}
		// A new app of the same name must not inherit old secrets.
		for _, app := range res.Removed {
			e.warnf(os.Remove(store.CredPath(app)))
			e.warnf(store.ClearRestart(app))
		}
		if e.KeepGenerations > 0 {
			if err := nixcmd.DeleteOldGenerations(ctx, e.Profile, e.KeepGenerations); err != nil {
				e.logf("warning: %v", err)
			}
		}
		return nil
	}
	if prev == "" || prev == storePath || prevGen == 0 {
		return err
	}
	// Unhealthy apps already kept their old slot; re-activating the previous
	// generation moves any apps that did switch back, so the current
	// generation again describes what runs. Only if it can still be
	// activated: a profile from an older coolix can't, and then the new
	// generation (whose healthy apps did switch) stays.
	if _, lerr := a.Load(prev); lerr != nil {
		e.logf("warning: the previous generation can't be re-activated (%v); keeping the new one", lerr)
		return fmt.Errorf("deploy partly failed: %w", err)
	}
	e.logf("activation failed; restoring generation %d", prevGen)
	failedGen := currentGeneration(e.Profile)
	if rerr := nixcmd.SwitchGeneration(ctx, e.Profile, prevGen); rerr != nil {
		return errors.Join(err, rerr)
	}
	if failedGen != 0 && failedGen != prevGen {
		if derr := nixcmd.DeleteGeneration(ctx, e.Profile, failedGen); derr != nil {
			e.logf("warning: %v", derr)
		}
	}
	if rerr := e.activate(ctx, a, prev, nil); rerr != nil {
		return errors.Join(err, rerr)
	}
	return fmt.Errorf("deploy failed, previous generation restored: %w", err)
}

func currentGeneration(profilePath string) int {
	gens, err := profile.List(profilePath)
	if err != nil {
		return 0
	}
	for _, g := range gens {
		if g.Current {
			return g.Number
		}
	}
	return 0
}

// ActivateBoot (root, at boot) re-renders and starts the current profile.
// Apps that no longer pass the policy are skipped, and a profile from an
// older coolix is left alone with a warning, so boot never fails because
// of coolix.
func (e *Engine) ActivateBoot(ctx context.Context) error {
	l, err := lock.Acquire(activateLock, "another activation", e.Out)
	if err != nil {
		return err
	}
	defer l.Release()
	a, err := e.activator()
	if err != nil {
		return err
	}
	a.SkipInvalid = true
	err = e.activate(ctx, a, e.Profile, nil)
	if errors.Is(err, units.ErrOldManifest) {
		e.logf("warning: not starting apps: %v", err)
		return nil
	}
	if err != nil {
		// Apps that failed kept their old slot (if any); that's for the UI and
		// `coolix status` to show, not a reason to fail the boot.
		e.logf("warning: %v", err)
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

func (e *Engine) activate(ctx context.Context, a *activate.Activator, profilePath string, force map[string]bool) error {
	_, err := e.activateResult(ctx, a, profilePath, force)
	return err
}

func (e *Engine) activateResult(ctx context.Context, a *activate.Activator, profilePath string, force map[string]bool) (*activate.Result, error) {
	a.ForceRestart = force
	res, err := a.Activate(ctx, profilePath)
	if res == nil {
		return &activate.Result{}, err
	}
	e.logf("started %v, switched %v, unchanged %v, removed %v, failed %v",
		res.Started, res.Switched, res.Unchanged, res.Removed, res.Failed)
	if len(res.Down) > 0 {
		e.logf("warning: not running (unchanged, failed to start): %v", res.Down)
	}
	return res, err
}

func (e *Engine) warnf(err error) {
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("warning: %v", err)
	}
}

// applyPendingSecrets runs as root: it encrypts pending secret changes into the
// credential store and reports which apps need a restart because of them.
func (e *Engine) applyPendingSecrets(ctx context.Context, m *units.Manifest) (map[string]bool, error) {
	var apps []string
	need := map[string]bool{}
	for name, app := range m.Apps {
		apps = append(apps, name)
		need[name] = app.Secrets
	}
	store := secrets.NewSystemdStore(e.SecretsDir)
	changed, err := store.Apply(ctx, e.PendingDir(), apps, need)
	for app := range changed {
		e.logf("%s: secrets updated", app)
	}
	// Also apps whose new secrets an earlier, failed activation applied.
	for app := range store.Restarts(apps) {
		changed[app] = true
	}
	return changed, err
}
