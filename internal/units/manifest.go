// Package units is the trusted half of an app profile. The profile is built
// by the unprivileged coolix user from the apps repo, so it only carries data
// (apps.json); root validates that data against a fixed policy and renders
// the systemd units itself. Whoever controls the repo can therefore choose
// what code runs inside an app's sandbox, but not the sandbox.
package units

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const ManifestVersion = 2

type Health struct {
	Path    string `json:"path,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

type Database struct {
	Kind    string `json:"kind"`    // postgres | redis
	Package string `json:"package"` // store path of the server package
}

type Resources struct {
	MemoryMax string `json:"memory_max,omitempty"`
	CPUQuota  string `json:"cpu_quota,omitempty"`
	TasksMax  int    `json:"tasks_max,omitempty"`
}

type Backup struct {
	Schedule string `json:"schedule,omitempty"` // systemd OnCalendar; empty: on demand only
	Keep     int    `json:"keep,omitempty"`
}

// App is one app as described by the profile (untrusted until Validate).
type App struct {
	Exe       string            `json:"exe"`
	PreStart  string            `json:"pre_start,omitempty"`
	PortBase  int               `json:"port_base"`
	Domains   []string          `json:"domains,omitempty"`
	Health    Health            `json:"health"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   bool              `json:"secrets,omitempty"`
	Path      []string          `json:"path,omitempty"`
	Databases []Database        `json:"databases,omitempty"`
	Hardening map[string]any    `json:"hardening,omitempty"`
	Resources Resources         `json:"resources"`
	Backup    Backup            `json:"backup"`
}

// Port returns the port of a slot (blue = PortBase, green = PortBase+1).
func (a App) Port(slot string) int {
	if slot == "green" {
		return a.PortBase + 1
	}
	return a.PortBase
}

func (a App) Database(kind string) (Database, bool) {
	for _, d := range a.Databases {
		if d.Kind == kind {
			return d, true
		}
	}
	return Database{}, false
}

type Manifest struct {
	Version int            `json:"version"`
	Rev     string         `json:"rev"`
	Apps    map[string]App `json:"apps"`
}

// ReadManifest parses <profile>/apps.json. It only checks the format; call
// Policy.Validate before acting on it.
func ReadManifest(profile string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(profile, "apps.json"))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("apps.json: %w", err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("apps.json: unsupported version %d (profiles built before coolix rendered units itself can't be activated; deploy again)", m.Version)
	}
	return &m, nil
}
