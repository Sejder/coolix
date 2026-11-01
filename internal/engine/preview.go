package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"coolix/internal/appdef"
	"coolix/internal/control"
	"coolix/internal/units"
)

// Preview is a branch or pull request of an app to deploy on its own.
type Preview struct {
	Parent string
	// Number is the pull request's number; 0 for a branch preview.
	Number int
	Branch string
	// Rev pins the commit (from a webhook); "" builds the branch's tip.
	Rev string
}

// Ref is the preview's preview_ref: pr-<n> or the branch.
func (p Preview) Ref() string {
	if p.Number > 0 {
		return appdef.PRRef(p.Number)
	}
	return p.Branch
}

func (p Preview) Name() string { return appdef.PreviewName(p.Parent, p.Ref()) }

// Previews lists an app's previews, by name.
func (e *Engine) Previews(parent string) ([]*appdef.App, error) {
	apps, err := e.Apps()
	if err != nil {
		return nil, err
	}
	var out []*appdef.App
	for _, a := range apps {
		if a.PreviewOf == parent {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// previewDomain is where an app's previews are served.
func (e *Engine) previewDomain(parent *appdef.App) string {
	if parent.Preview != nil && parent.Preview.Domain != "" {
		return parent.Preview.Domain
	}
	return e.PreviewDomain
}

// DeployPreview creates or updates the preview of a branch or pull request
// and deploys it. The preview is an app like any other, made from its
// parent's settings: its own ports, domain and (empty) databases, and no
// secrets unless the parent opts in.
func (e *Engine) DeployPreview(ctx context.Context, p Preview) error {
	name, err := e.upsertPreview(p)
	if err != nil {
		return err
	}
	e.logf("deploying preview %s of %s (%s)", name, p.Parent, p.Ref())
	return e.Deploy(ctx, name)
}

func (e *Engine) upsertPreview(p Preview) (string, error) {
	l, err := e.repoLock()
	if err != nil {
		return "", err
	}
	defer l.Release()
	parent, err := e.App(p.Parent)
	if err != nil {
		return "", err
	}
	if parent.IsPreview() {
		return "", fmt.Errorf("%s is a preview itself", parent.Name)
	}
	if !parent.PreviewsEnabled() {
		return "", fmt.Errorf("previews are off for %s (turn them on in its settings, or coolix app set %s --preview on)", parent.Name, parent.Name)
	}
	domain := e.previewDomain(parent)
	if domain == "" {
		return "", errors.New("no preview domain: set services.coolix.previews.domain, or the app's preview domain")
	}
	r, err := e.Repo()
	if err != nil {
		return "", err
	}
	apps, err := appdef.LoadAll(r.Dir)
	if err != nil {
		return "", err
	}

	name := p.Name()
	var existing *appdef.App
	count := 0
	for _, a := range apps {
		if a.PreviewOf == parent.Name {
			count++
		}
		if a.Name == name {
			existing = a
		}
	}
	if existing != nil && (existing.PreviewOf != parent.Name || existing.PreviewRef != p.Ref()) {
		return "", fmt.Errorf("can't preview %s as %s: an app of that name already exists", p.Ref(), name)
	}
	if existing == nil && count >= parent.PreviewMax() {
		return "", fmt.Errorf("%s already has %d previews (its maximum); remove one first", parent.Name, count)
	}

	// A copy of the parent's settings, without what belongs to the parent.
	pv := *parent
	pv.Name, pv.PreviewOf, pv.PreviewRef = name, parent.Name, p.Ref()
	pv.Domains = []string{name + "." + domain}
	pv.Preview, pv.Backup, pv.Webhook, pv.Secrets = nil, nil, nil, nil
	pv.PreviewSecrets = parent.Preview.Secrets
	if parent.Pushed() {
		pv.Branch = p.Branch
	} else {
		pv.Branch = ""
		if pv.Source, err = appdef.PinSource(parent.Source, p.Branch, p.Rev); err != nil {
			return "", err
		}
	}
	if existing != nil {
		pv.PortBase = existing.PortBase
	} else if pv.PortBase, err = appdef.AllocatePort(apps, e.PortMin, e.PortMax); err != nil {
		return "", err
	}
	if err := pv.Validate(); err != nil {
		return "", fmt.Errorf("preview %s: %w", name, err)
	}
	msg := "Update preview " + name
	if existing == nil {
		msg = "Add preview " + name
		// A token left behind by an app of the same name.
		if err := e.DisableWebhook(name); err != nil {
			return "", err
		}
	}
	return name, e.save(&pv, msg)
}

// RemovePreview removes a preview, deploys without it and deletes its data.
func (e *Engine) RemovePreview(ctx context.Context, parent, name string) error {
	a, err := e.App(name)
	if err != nil || a.PreviewOf != parent {
		return fmt.Errorf("%s has no preview %s", parent, name)
	}
	if err := e.RemoveApp(name); err != nil {
		return err
	}
	if err := e.Deploy(ctx, ""); err != nil {
		return err
	}
	return e.privileged(ctx, control.Request{Verb: "purge", Arg: name})
}

// FindPreview returns the name of parent's preview of ref, if there is one.
func (e *Engine) FindPreview(parent, ref string) (string, bool) {
	previews, err := e.Previews(parent)
	if err != nil {
		return "", false
	}
	i := slices.IndexFunc(previews, func(a *appdef.App) bool { return a.PreviewRef == ref })
	if i < 0 {
		return "", false
	}
	return previews[i].Name, true
}

// purge (root) deletes the data of an app that is no longer deployed: its
// state directory and databases. Backups are kept.
func (e *Engine) purge(app string, w io.Writer) error {
	m, err := units.ReadManifest(e.Profile)
	if err != nil {
		return fmt.Errorf("no deployed profile: %w", err)
	}
	if _, ok := m.Apps[app]; ok {
		return fmt.Errorf("%s is deployed; remove it and deploy first", app)
	}
	var dirs []string
	for _, base := range []string{"/var/lib/private", "/var/lib"} {
		dirs = append(dirs, filepath.Join(base, "coolix-apps", app))
		for _, kind := range []string{"postgres", "redis"} {
			dirs = append(dirs, filepath.Join(base, "coolix-db", app+"-"+kind))
		}
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		fmt.Fprintf(w, "coolix: deleted %s\n", d)
	}
	return nil
}
