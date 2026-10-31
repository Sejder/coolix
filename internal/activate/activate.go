// Package activate switches the running system to an app profile: it links
// the profile's units into a runtime unit dir, (re)starts plain services such
// as databases, and moves apps to their new version with blue/green slots.
package activate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultProfile = "/nix/var/nix/profiles/coolix/apps"
	DefaultUnitDir = "/run/systemd/system"
	appPrefix      = "coolix-app-"
	dbPrefix       = "coolix-db-"
)

var Slots = [2]string{"blue", "green"}

type Health struct {
	Path    string `json:"path"`
	Timeout string `json:"timeout"`
}

type AppManifest struct {
	Unit    string         `json:"unit"`
	Ports   map[string]int `json:"ports"`
	Domains []string       `json:"domains"`
	Health  Health         `json:"health"`
	// Secrets is true when the unit loads /var/lib/coolix-secrets/<app>.cred.
	Secrets bool `json:"secrets"`
}

type Manifest struct {
	Version int                    `json:"version"`
	Rev     string                 `json:"rev"`
	Apps    map[string]AppManifest `json:"apps"`
	// Services are plain (non-slotted) units such as per-app databases.
	Services []string `json:"services"`
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
	Systemd Systemd
	Router  Router // nil disables routing
	// Health probes a slot once; nil means "always healthy".
	Health func(ctx context.Context, port int, path string) error
	// Drain is how long the old slot keeps running after traffic moved.
	Drain time.Duration
	// ForceRestart lists apps to move to a fresh slot even if their unit is
	// unchanged (e.g. their secrets changed).
	ForceRestart map[string]bool
	Log          func(format string, args ...any)
	// Now/Sleep are overridable for tests.
	Sleep func(time.Duration)
	Now   func() time.Time
}

type Result struct {
	Started, Switched, Unchanged, Removed, Failed []string
}

func SlotUnit(app, slot string) string { return appPrefix + app + "@" + slot + ".service" }

func appFromUnit(unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, appPrefix), "@.service")
}

func isAppUnit(n string) bool {
	return strings.HasPrefix(n, appPrefix) && strings.HasSuffix(n, "@.service")
}
func isDBUnit(n string) bool {
	return strings.HasPrefix(n, dbPrefix) && strings.HasSuffix(n, ".service")
}

type state int

const (
	unchanged state = iota
	added
	changed
)

// Activate makes the profile at profileDir live.
func (a *Activator) Activate(ctx context.Context, profileDir string) (*Result, error) {
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

	// Classify and link every unit in the profile.
	units, err := os.ReadDir(filepath.Join(profileDir, "units"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	states := map[string]state{}
	for _, e := range units {
		unit := e.Name()
		src := filepath.Join(profileDir, "units", unit)
		newData, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		st := added
		if old, ok := current[unit]; ok {
			st = changed
			// Compare contents, not paths: every profile has a new store path.
			if oldData, err := os.ReadFile(old); err == nil && bytes.Equal(oldData, newData) {
				st = unchanged
			}
		}
		states[unit] = st
		if err := a.link(src, unit); err != nil {
			return nil, err
		}
	}

	res := &Result{}
	// Stop and unlink whatever the profile no longer contains.
	for _, unit := range sortedKeys(current) {
		if _, ok := states[unit]; ok {
			continue
		}
		if isAppUnit(unit) {
			name := appFromUnit(unit)
			a.logf("stopping removed app %s", name)
			for _, s := range Slots {
				a.warn(a.Systemd.Stop(SlotUnit(name, s)))
			}
			if a.Router != nil {
				a.warn(a.Router.RemoveRoute(name))
			}
			res.Removed = append(res.Removed, name)
		} else {
			a.logf("stopping removed service %s", unit)
			a.warn(a.Systemd.Stop(unit))
		}
		if err := os.Remove(filepath.Join(a.UnitDir, unit)); err != nil {
			return nil, err
		}
	}

	if err := a.Systemd.DaemonReload(); err != nil {
		return nil, err
	}

	var errs []error
	// Databases first: apps depend on them.
	for _, svc := range m.Services {
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
		st := states[app.Unit]
		if st == unchanged && !a.ForceRestart[name] {
			if err := a.ensureRunning(ctx, name, app); err != nil {
				errs = append(errs, fmt.Errorf("app %s: %w", name, err))
				res.Failed = append(res.Failed, name)
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
			if old, ok := current[app.Unit]; ok && st != unchanged {
				a.warn(a.link(old, app.Unit))
			} else if st == added {
				a.warn(os.Remove(filepath.Join(a.UnitDir, app.Unit)))
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
	if relink {
		a.warn(a.Systemd.DaemonReload())
	}
	return res, errors.Join(errs...)
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
func (a *Activator) switchSlot(ctx context.Context, name string, app AppManifest) error {
	old := a.activeSlot(name)
	next := Slots[0]
	if old == Slots[0] {
		next = Slots[1]
	}
	unit := SlotUnit(name, next)
	port := app.Ports[next]
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
func (a *Activator) ensureRunning(ctx context.Context, name string, app AppManifest) error {
	slot := a.activeSlot(name)
	if slot == "" {
		return a.switchSlot(ctx, name, app)
	}
	if a.Router != nil {
		return a.Router.SetRoute(name, app.Domains, app.Ports[slot])
	}
	return nil
}

func (a *Activator) waitHealthy(ctx context.Context, unit string, port int, h Health) error {
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
		if !isAppUnit(n) && !isDBUnit(n) {
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
