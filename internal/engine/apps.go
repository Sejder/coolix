package engine

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"coolix/internal/appdef"
	"coolix/internal/secrets"
	"coolix/internal/units"
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
	if err := e.checkHostPolicy(app); err != nil {
		return err
	}
	if app.Backup != nil && app.Backup.Schedule != "" {
		if err := checkCalendar(app.Backup.Schedule); err != nil {
			return fmt.Errorf("backup schedule %q: %w", app.Backup.Schedule, err)
		}
	}
	r, err := e.Repo()
	if err != nil {
		return err
	}
	if app.Pushed() {
		if err := e.initPushRepo(app.PushRepo()); err != nil {
			return err
		}
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
	// Its previews go with it.
	apps, err := appdef.LoadAll(r.Dir)
	if err != nil {
		return err
	}
	var previews []string
	for _, a := range apps {
		if a.PreviewOf == name {
			if err := r.RemoveApp(a.Name); err != nil {
				return err
			}
			previews = append(previews, a.Name)
		}
	}
	if _, err := r.Sync(); err != nil {
		return err
	}
	if err := r.Commit("Remove app " + name); err != nil {
		return err
	}
	if err := e.removePushRepo(name); err != nil {
		return err
	}
	for _, p := range previews {
		if err := e.DisableWebhook(p); err != nil {
			return err
		}
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

// checkHostPolicy catches, when saving, what root would refuse at the next
// deploy (and so block every deploy): domains that are reserved or used by
// another app, and [hardening] keys this host doesn't allow.
func (e *Engine) checkHostPolicy(app *appdef.App) error {
	p := e.Policy()
	m := &units.Manifest{Apps: map[string]units.App{}}
	r, err := e.Repo()
	if err != nil {
		return err
	}
	others, err := appdef.LoadAll(r.Dir)
	if err != nil {
		return err
	}
	m.Apps[app.Name] = units.App{Domains: app.Domains, Hardening: app.Hardening}
	var errs []error
	for _, o := range others {
		if o.Name == app.Name {
			continue
		}
		for _, d := range app.Domains {
			if slices.ContainsFunc(o.Domains, func(x string) bool { return strings.EqualFold(x, d) }) {
				errs = append(errs, fmt.Errorf("domain %s is already used by %s", d, o.Name))
			}
		}
	}
	for _, err := range p.ValidateApps(m) {
		for _, line := range strings.Split(err.Error(), "\n") {
			if strings.HasPrefix(line, "app "+app.Name+": domain") || strings.HasPrefix(line, "app "+app.Name+": hardening") {
				errs = append(errs, errors.New(strings.TrimPrefix(line, "app "+app.Name+": ")))
			}
		}
	}
	return errors.Join(errs...)
}
