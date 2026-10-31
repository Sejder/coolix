// Package units is the trusted half of an app profile. The profile is built
// by the unprivileged coolix user from the apps repo, so it only carries data
// (apps.json); root validates that data against a fixed policy and renders
// the systemd units itself. Whoever controls the repo can therefore choose
// what code runs inside an app's sandbox, but not the sandbox.
package units

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
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
	Exe             string            `json:"exe"`
	PreStart        string            `json:"pre_start,omitempty"`
	PreStartTimeout string            `json:"pre_start_timeout,omitempty"`
	PortBase        int               `json:"port_base"`
	Domains         []string          `json:"domains,omitempty"`
	Health          Health            `json:"health"`
	Env             map[string]string `json:"env,omitempty"`
	Secrets         bool              `json:"secrets,omitempty"`
	Path            []string          `json:"path,omitempty"`
	Databases       []Database        `json:"databases,omitempty"`
	Hardening       map[string]any    `json:"hardening,omitempty"`
	Resources       Resources         `json:"resources"`
	Backup          Backup            `json:"backup"`
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

// ErrOldManifest marks profiles built by coolix versions that shipped unit
// files instead of data; they can't be activated anymore.
var ErrOldManifest = errors.New("profile was built by an older coolix (units weren't rendered by root yet); run `coolix deploy` to rebuild it")

// MaxManifestSize bounds apps.json.
const MaxManifestSize = 4 << 20

// readFile reads <profile>/apps.json without following a symlink, refusing
// anything but a regular file of sane size (the profile is built by the
// unprivileged side).
func readFile(profile string) ([]byte, error) {
	p := filepath.Join(profile, "apps.json")
	fd, err := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxManifestSize {
		return nil, fmt.Errorf("%s: not a regular file of at most %d bytes", p, MaxManifestSize)
	}
	return io.ReadAll(io.LimitReader(f, MaxManifestSize))
}

// ReadManifest parses <profile>/apps.json. It only checks the format; call
// Policy.Validate before acting on it.
func ReadManifest(profile string) (*Manifest, error) {
	data, err := readFile(profile)
	if err != nil {
		return nil, err
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("apps.json: %w", err)
	}
	if head.Version < ManifestVersion {
		return nil, ErrOldManifest
	}
	if head.Version != ManifestVersion {
		return nil, fmt.Errorf("apps.json: unsupported version %d (newer coolix?)", head.Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("apps.json: %w", err)
	}
	return &m, nil
}

// Summary is what status pages need from a manifest of any version.
type Summary struct {
	Version int
	Rev     string
	Apps    map[string]SummaryApp
}

type SummaryApp struct {
	Domains []string
	ports   map[string]int
}

func (a SummaryApp) Port(slot string) int { return a.ports[slot] }

// ReadSummary reads rev, apps, ports and domains from any manifest version.
func ReadSummary(profile string) (*Summary, error) {
	data, err := readFile(profile)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Version int    `json:"version"`
		Rev     string `json:"rev"`
		Apps    map[string]struct {
			PortBase int            `json:"port_base"`
			Ports    map[string]int `json:"ports"` // version 1
			Domains  []string       `json:"domains"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("apps.json: %w", err)
	}
	s := &Summary{Version: raw.Version, Rev: raw.Rev, Apps: map[string]SummaryApp{}}
	for name, a := range raw.Apps {
		ports := a.Ports
		if ports == nil {
			ports = map[string]int{"blue": a.PortBase, "green": a.PortBase + 1}
		}
		s.Apps[name] = SummaryApp{Domains: a.Domains, ports: ports}
	}
	return s, nil
}
