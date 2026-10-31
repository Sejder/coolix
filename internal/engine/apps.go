package engine

import (
	"fmt"
	"os"
	"slices"
	"sort"

	"coolix/internal/appdef"
	"coolix/internal/secrets"
)

func (e *Engine) Apps() ([]*appdef.App, error) {
	r, err := e.Repo()
	if err != nil {
		return nil, err
	}
	if err := r.Init(); err != nil {
		return nil, err
	}
	return appdef.LoadAll(r.Dir)
}

func (e *Engine) App(name string) (*appdef.App, error) {
	r, err := e.Repo()
	if err != nil {
		return nil, err
	}
	return appdef.Load(appdef.Path(r.Dir, name))
}

// CreateApp allocates ports for a new app, saves and commits it.
func (e *Engine) CreateApp(app *appdef.App) error {
	l, err := e.repoLock()
	if err != nil {
		return err
	}
	defer l.Release()
	r, err := e.Repo()
	if err != nil {
		return err
	}
	if err := r.Init(); err != nil {
		return err
	}
	if _, err := os.Stat(appdef.Path(r.Dir, app.Name)); err == nil {
		return fmt.Errorf("app %q already exists", app.Name)
	}
	existing, err := appdef.LoadAll(r.Dir)
	if err != nil {
		return err
	}
	if app.PortBase, err = appdef.AllocatePort(existing, e.PortMin, e.PortMax); err != nil {
		return err
	}
	// A token left behind by an app of the same name removed by hand.
	if err := e.DisableWebhook(app.Name); err != nil {
		return err
	}
	app.Normalize()
	return e.save(app, "Add app "+app.Name)
}

func (e *Engine) save(app *appdef.App, msg string) error {
	if app.Backup != nil && app.Backup.Schedule != "" {
		if err := checkCalendar(app.Backup.Schedule); err != nil {
			return fmt.Errorf("backup schedule %q: %w", app.Backup.Schedule, err)
		}
	}
	r, err := e.Repo()
	if err != nil {
		return err
	}
	if err := appdef.Save(r.Dir, app); err != nil {
		return err
	}
	if _, err := r.Sync(); err != nil {
		return err
	}
	return r.Commit(msg)
}

// RemoveApp deletes an app from the repo; the next deploy stops it.
func (e *Engine) RemoveApp(name string) error {
	l, err := e.repoLock()
	if err != nil {
		return err
	}
	defer l.Release()
	r, err := e.Repo()
	if err != nil {
		return err
	}
	if err := r.RemoveApp(name); err != nil {
		return err
	}
	if _, err := r.Sync(); err != nil {
		return err
	}
	if err := r.Commit("Remove app " + name); err != nil {
		return err
	}
	return e.DisableWebhook(name)
}

type SecretInfo struct {
	Name    string
	Pending bool // changed but not deployed yet
}

func (e *Engine) Secrets(app string) ([]SecretInfo, error) {
	a, err := e.App(app)
	if err != nil {
		return nil, err
	}
	p, err := secrets.ReadPending(e.PendingDir(), app)
	if err != nil {
		return nil, err
	}
	keys := slices.Clone(a.Secrets)
	sort.Strings(keys)
	out := make([]SecretInfo, len(keys))
	for i, k := range keys {
		_, pending := p.Set[k]
		out[i] = SecretInfo{Name: k, Pending: pending}
	}
	return out, nil
}

// SetSecret records a secret value (applied at the next deploy) and commits
// only its name to the repo.
func (e *Engine) SetSecret(app, key, value string) error {
	l, err := e.repoLock()
	if err != nil {
		return err
	}
	defer l.Release()
	a, err := e.App(app)
	if err != nil {
		return err
	}
	if !slices.Contains(a.Secrets, key) {
		a.Secrets = append(a.Secrets, key)
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if err := secrets.AddPending(e.PendingDir(), app, map[string]string{key: value}, nil); err != nil {
		return err
	}
	return e.save(a, fmt.Sprintf("Set secret %s for %s", key, app))
}

func (e *Engine) UnsetSecret(app, key string) error {
	l, err := e.repoLock()
	if err != nil {
		return err
	}
	defer l.Release()
	a, err := e.App(app)
	if err != nil {
		return err
	}
	if !slices.Contains(a.Secrets, key) {
		return fmt.Errorf("%s has no secret %s", app, key)
	}
	a.Secrets = slices.DeleteFunc(a.Secrets, func(s string) bool { return s == key })
	if err := secrets.AddPending(e.PendingDir(), app, nil, []string{key}); err != nil {
		return err
	}
	return e.save(a, fmt.Sprintf("Unset secret %s for %s", key, app))
}

// ModifyApp loads, changes and saves an app under the repo lock, so
// concurrent edits (e.g. a secret set while the settings form is saved)
// don't overwrite each other. It returns the changed app, also on error.
func (e *Engine) ModifyApp(name string, change func(*appdef.App) error) (*appdef.App, error) {
	l, err := e.repoLock()
	if err != nil {
		return nil, err
	}
	defer l.Release()
	app, err := e.App(name)
	if err != nil {
		return nil, err
	}
	if err := change(app); err != nil {
		return app, err
	}
	app.Normalize()
	return app, e.save(app, "Update app "+name)
}
