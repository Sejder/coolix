// Package activate switches the running system to an app profile: it links
// the profile's units into a runtime unit dir and (re)starts what changed.
package activate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	DefaultProfile = "/nix/var/nix/profiles/coolix/apps"
	DefaultUnitDir = "/run/systemd/system"
	unitPrefix     = "coolix-app-"
)

type AppManifest struct {
	Unit    string         `json:"unit"`
	Ports   map[string]int `json:"ports"`
	Domains []string       `json:"domains"`
	Health  map[string]any `json:"health"`
}

type Manifest struct {
	Version int                    `json:"version"`
	Apps    map[string]AppManifest `json:"apps"`
}

func ReadManifest(profile string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(profile, "apps.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("apps.json: %w", err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("apps.json: unsupported version %d", m.Version)
	}
	return &m, nil
}

// Systemd is the subset of systemctl the activator needs; swapped out in tests.
type Systemd interface {
	DaemonReload() error
	Restart(unit string) error
	Start(unit string) error
	Stop(unit string) error
}

type Systemctl struct{}

func (Systemctl) run(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}
func (s Systemctl) DaemonReload() error       { return s.run("daemon-reload") }
func (s Systemctl) Restart(unit string) error { return s.run("restart", unit) }
func (s Systemctl) Start(unit string) error   { return s.run("start", unit) }
func (s Systemctl) Stop(unit string) error    { return s.run("stop", unit) }

type Activator struct {
	UnitDir string
	Systemd Systemd
	Log     func(format string, args ...any)
}

// Result reports what an activation did, per app name.
type Result struct {
	Started, Restarted, Unchanged, Removed []string
}

// slotUnit is the running instance for an app. M1 always uses the blue slot;
// blue/green switching replaces this.
func slotUnit(app string) string { return unitPrefix + app + "@blue.service" }

func appFromUnit(unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, unitPrefix), "@.service")
}

// Activate makes the profile at profileDir live. profileDir should be a
// resolved store path (or the profile symlink).
func (a *Activator) Activate(profileDir string) (*Result, error) {
	profileDir, err := filepath.EvalSymlinks(profileDir)
	if err != nil {
		return nil, err
	}
	m, err := ReadManifest(profileDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.UnitDir, 0o755); err != nil {
		return nil, err
	}

	current, err := a.linkedUnits()
	if err != nil {
		return nil, err
	}

	res := &Result{}
	wanted := map[string]bool{}
	changed := map[string]bool{}
	for name, app := range m.Apps {
		wanted[app.Unit] = true
		src := filepath.Join(profileDir, "units", app.Unit)
		newData, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("app %s: %w", name, err)
		}
		oldTarget, linked := current[app.Unit]
		if linked {
			// Compare contents, not paths: every profile has a new store path.
			if oldData, err := os.ReadFile(oldTarget); err == nil && bytes.Equal(oldData, newData) {
				res.Unchanged = append(res.Unchanged, name)
				if err := a.link(src, app.Unit); err != nil {
					return nil, err
				}
				continue
			}
		}
		changed[name] = linked
		if err := a.link(src, app.Unit); err != nil {
			return nil, err
		}
	}

	// Stop removed apps before their unit files disappear.
	for unit := range current {
		if wanted[unit] {
			continue
		}
		name := appFromUnit(unit)
		a.logf("stopping removed app %s", name)
		if err := a.Systemd.Stop(slotUnit(name)); err != nil {
			a.logf("warning: %v", err)
		}
		if err := os.Remove(filepath.Join(a.UnitDir, unit)); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, name)
	}

	if err := a.Systemd.DaemonReload(); err != nil {
		return nil, err
	}

	var errs []string
	for _, name := range sortedKeys(changed) {
		if changed[name] {
			a.logf("restarting %s", name)
			if err := a.Systemd.Restart(slotUnit(name)); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			res.Restarted = append(res.Restarted, name)
		} else {
			a.logf("starting %s", name)
			if err := a.Systemd.Start(slotUnit(name)); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			res.Started = append(res.Started, name)
		}
	}
	// Unchanged apps may not be running (e.g. after a crash); start is idempotent.
	for _, name := range res.Unchanged {
		if err := a.Systemd.Start(slotUnit(name)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	sort.Strings(res.Unchanged)
	sort.Strings(res.Removed)
	if len(errs) > 0 {
		return res, fmt.Errorf("activation finished with errors:\n%s", strings.Join(errs, "\n"))
	}
	return res, nil
}

// linkedUnits returns coolix unit links currently in UnitDir, name -> target.
func (a *Activator) linkedUnits() (map[string]string, error) {
	entries, err := os.ReadDir(a.UnitDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, unitPrefix) || !strings.HasSuffix(n, "@.service") {
			continue
		}
		target, err := os.Readlink(filepath.Join(a.UnitDir, n))
		if err != nil {
			continue // not a symlink: not ours
		}
		out[n] = target
	}
	return out, nil
}

// link atomically points UnitDir/unit at src.
func (a *Activator) link(src, unit string) error {
	dst := filepath.Join(a.UnitDir, unit)
	tmp := dst + ".coolix-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(src, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (a *Activator) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
