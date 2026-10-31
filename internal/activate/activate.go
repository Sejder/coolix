// Package activate switches the running system to an app profile: it
// validates the profile's manifest, renders the units (see package units),
// links them into a runtime unit dir, (re)starts databases and timers, and
// moves apps to their new version with blue/green slots.
package activate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"coolix/internal/units"
)

const (
	DefaultProfile   = "/nix/var/nix/profiles/coolix/apps"
	DefaultUnitDir   = "/run/systemd/system"
	DefaultRenderDir = "/run/coolix/units"
)

var Slots = units.Slots

func SlotUnit(app, slot string) string { return units.SlotUnit(app, slot) }

// Systemd is the subset of systemctl the activator needs; swapped out in tests.
type Systemd interface {
	DaemonReload() error
	Restart(unit string) error
	Start(unit string) error
	Stop(unit string) error
	IsActive(unit string) bool
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
func (s Systemctl) IsActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

// Router points app domains at a slot's port (see package caddy).
type Router interface {
	SetRoute(app string, domains []string, port int) error
	RemoveRoute(app string) error
}

type Activator struct {
	UnitDir string
	// RenderDir holds the rendered unit files (root-owned); UnitDir links to them.
	RenderDir string
	Policy    units.Policy
	Options   units.Options
	Systemd   Systemd
	Router    Router // nil disables routing
	// Health probes a slot once; nil means "always healthy".
	Health func(ctx context.Context, port int, path string) error
	// Drain is how long the old slot keeps running after traffic moved.
	Drain time.Duration
	// ForceRestart lists apps to move to a fresh slot even if their unit is
	// unchanged (e.g. their secrets changed).
	ForceRestart map[string]bool
	Log          func(format string, args ...any)
	// SkipInvalid drops apps that fail the policy instead of refusing the
	// whole profile. Used at boot, where the policy may have changed since
	// the profile was deployed (e.g. a narrower port range).
	SkipInvalid bool
	// Now/Sleep are overridable for tests.
	Sleep func(time.Duration)
	Now   func() time.Time
}

type Result struct {
	Started, Switched, Unchanged, Removed, Failed []string
	// Down lists unchanged apps that are not running and failed to start.
	// They don't fail the activation: a deploy of other apps shouldn't be
	// rolled back because one app was already broken.
	Down []string
}

type state int

const (
	unchanged state = iota
	added
	changed
)

// Load reads and validates the manifest of a profile.
func (a *Activator) Load(profileDir string) (*units.Manifest, error) {
	m, err := units.ReadManifest(profileDir)
	if err != nil {
		return nil, err
	}
	if a.SkipInvalid {
		for name, err := range a.Policy.ValidateApps(m) {
			a.logf("skipping app %s: %v", name, err)
			delete(m.Apps, name)
		}
		return m, nil
	}
	if err := a.Policy.Validate(m); err != nil {
		return nil, fmt.Errorf("refusing to activate %s: %w", profileDir, err)
	}
	return m, nil
}

// Activate makes the profile at profileDir live.
func (a *Activator) Activate(ctx context.Context, profileDir string) (*Result, error) {
	profileDir, err := filepath.EvalSymlinks(profileDir)
	if err != nil {
		return nil, err
	}
	m, err := a.Load(profileDir)
	if err != nil {
		return nil, err
	}
	r := units.Render(m, a.Options)
	if err := os.MkdirAll(a.UnitDir, 0o755); err != nil {
		return nil, err
	}
	current, err := a.linkedUnits()
	if err != nil {
		return nil, err
	}

	// Write, classify and link every rendered unit.
	states := map[string]state{}
	for _, unit := range sortedKeys(r.Files) {
		src, err := a.write(unit, r.Files[unit])
		if err != nil {
			return nil, err
		}
		st := added
		if old, ok := current[unit]; ok {
			st = changed
			if oldData, err := os.ReadFile(old); err == nil && string(oldData) == r.Files[unit] {
				st = unchanged
			}
		}
		states[unit] = st
		if err := a.link(src, unit); err != nil {
			return nil, err
		}
	}

	res := &Result{}
	// Stop and unlink whatever the profile no longer contains. Other units
	// (databases, timers) are stopped only after the apps switched, so a
	// serving slot keeps its database until its replacement is up.
	var stopLater []string
	for _, unit := range sortedKeys(current) {
		if _, ok := states[unit]; ok {
			continue
		}
		if name := units.AppOfUnit(unit); name != "" {
			a.logf("stopping removed app %s", name)
			for _, s := range Slots {
				a.warn(a.Systemd.Stop(SlotUnit(name, s)))
			}
			if a.Router != nil {
				a.warn(a.Router.RemoveRoute(name))
			}
			res.Removed = append(res.Removed, name)
			if err := os.Remove(filepath.Join(a.UnitDir, unit)); err != nil {
				return nil, err
			}
		} else if !strings.HasSuffix(unit, "@.service") {
			stopLater = append(stopLater, unit)
		} else if err := os.Remove(filepath.Join(a.UnitDir, unit)); err != nil {
			return nil, err
		}
	}

	if err := a.Systemd.DaemonReload(); err != nil {
		return nil, err
	}

	var errs []error
	// Databases first: apps depend on them.
	for _, svc := range append(r.Services, r.Timers...) {
		var err error
		switch states[svc] {
		case changed:
			a.logf("restarting %s", svc)
			err = a.Systemd.Restart(svc)
		default:
			err = a.Systemd.Start(svc)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	relink := false
	for _, name := range sortedKeys(m.Apps) {
		app := m.Apps[name]
		unit := units.AppUnit(name)
		st := states[unit]
		if st == unchanged && !a.ForceRestart[name] {
			if err := a.ensureRunning(ctx, name, app); err != nil {
				a.logf("warning: %s (unchanged) is not running: %v", name, err)
				res.Down = append(res.Down, name)
				continue
			}
			res.Unchanged = append(res.Unchanged, name)
			continue
		}
		had := a.activeSlot(name) != ""
		if err := a.switchSlot(ctx, name, app); err != nil {
			errs = append(errs, fmt.Errorf("app %s: %w", name, err))
			res.Failed = append(res.Failed, name)
			// Put the previous unit back so the old slot's restarts keep running old code.
			if old, ok := current[unit]; ok && st != unchanged {
				a.warn(a.link(old, unit))
			} else if st == added {
				a.warn(os.Remove(filepath.Join(a.UnitDir, unit)))
			}
			relink = true
			continue
		}
		if had {
			res.Switched = append(res.Switched, name)
		} else {
			res.Started = append(res.Started, name)
		}
	}
	for _, unit := range stopLater {
		// Keep what a failed app's old slot still uses (its database);
		// the previous generation gets re-activated.
		if app := units.AppOfAnyUnit(unit); app != "" && (slices.Contains(res.Failed, app) || slices.Contains(res.Down, app)) {
			a.logf("keeping %s: %s didn't switch", unit, app)
			continue
		}
		a.logf("stopping removed unit %s", unit)
		a.warn(a.Systemd.Stop(unit))
		if err := os.Remove(filepath.Join(a.UnitDir, unit)); err != nil {
			errs = append(errs, err)
		}
		relink = true
	}
	if relink {
		a.warn(a.Systemd.DaemonReload())
	}
	a.warn(a.prune())
	return res, errors.Join(errs...)
}

// write stores a rendered unit under RenderDir/<content hash>/<unit>, so
// links to older versions stay valid until they're replaced.
func (a *Activator) write(unit, content string) (string, error) {
	sum := sha256.Sum256([]byte(unit + "\x00" + content))
	dir := filepath.Join(a.RenderDir, hex.EncodeToString(sum[:12]))
	path := filepath.Join(dir, unit)
	if data, err := os.ReadFile(path); err == nil && string(data) == content {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// prune removes rendered units nothing links to anymore.
func (a *Activator) prune() error {
	linked, err := a.linkedUnits()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, target := range linked {
		keep[filepath.Dir(target)] = true
	}
	entries, err := os.ReadDir(a.RenderDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if dir := filepath.Join(a.RenderDir, e.Name()); !keep[dir] {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// activeSlot returns the running slot of app, or "".
func (a *Activator) activeSlot(app string) string {
	for _, s := range Slots {
		if a.Systemd.IsActive(SlotUnit(app, s)) {
			return s
		}
	}
	return ""
}

// switchSlot starts the idle slot, waits until it's healthy, routes traffic
// to it and stops the old one. On failure the old slot is left untouched.
func (a *Activator) switchSlot(ctx context.Context, name string, app units.App) error {
	old := a.activeSlot(name)
	next := Slots[0]
	if old == Slots[0] {
		next = Slots[1]
	}
	unit := SlotUnit(name, next)
	port := app.Port(next)
	a.logf("%s: starting %s slot on port %d", name, next, port)
	if err := a.Systemd.Restart(unit); err != nil {
		return err
	}
	if err := a.waitHealthy(ctx, unit, port, app.Health); err != nil {
		a.logf("%s: %s slot unhealthy, stopping it: %v", name, next, err)
		a.warn(a.Systemd.Stop(unit))
		if old != "" {
			a.logf("%s: %s slot keeps serving", name, old)
		}
		return err
	}
	if a.Router != nil {
		if err := a.Router.SetRoute(name, app.Domains, port); err != nil {
			a.warn(a.Systemd.Stop(unit))
			return err
		}
	}
	if old != "" {
		a.sleep(a.Drain)
		a.logf("%s: stopping %s slot", name, old)
		a.warn(a.Systemd.Stop(SlotUnit(name, old)))
	}
	return nil
}

// ensureRunning brings an unchanged app up if nothing is running (e.g. at
// boot) and makes sure its route exists.
func (a *Activator) ensureRunning(ctx context.Context, name string, app units.App) error {
	slot := a.activeSlot(name)
	if slot == "" {
		return a.switchSlot(ctx, name, app)
	}
	if a.Router != nil {
		return a.Router.SetRoute(name, app.Domains, app.Port(slot))
	}
	return nil
}

func (a *Activator) waitHealthy(ctx context.Context, unit string, port int, h units.Health) error {
	timeout := 30 * time.Second
	if h.Timeout != "" {
		if d, err := time.ParseDuration(h.Timeout); err == nil {
			timeout = d
		}
	}
	if a.Health == nil {
		return nil
	}
	deadline := a.now().Add(timeout)
	var last error
	for {
		if last = a.Health(ctx, port, h.Path); last == nil {
			return nil
		}
		if a.now().After(deadline) {
			if !a.Systemd.IsActive(unit) {
				return fmt.Errorf("health check timed out after %s (%s is not running; see journalctl -u %s): %w", timeout, unit, unit, last)
			}
			return fmt.Errorf("health check timed out after %s: %w", timeout, last)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		a.sleep(500 * time.Millisecond)
	}
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
		if !units.Owned(n) {
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

func (a *Activator) sleep(d time.Duration) {
	if a.Sleep != nil {
		a.Sleep(d)
	} else {
		time.Sleep(d)
	}
}

func (a *Activator) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

func (a *Activator) warn(err error) {
	if err != nil {
		a.logf("warning: %v", err)
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

func (a *Activator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
