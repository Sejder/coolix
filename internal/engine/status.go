package engine

import (
	"os/exec"
	"sort"

	"coolix/internal/activate"
	"coolix/internal/appdef"
	"coolix/internal/profile"
)

type AppStatus struct {
	Name    string
	Slot    string // "blue", "green" or "" when stopped
	Port    int
	Domains []string
	// Deployed is false for apps in the repo that aren't in the running profile.
	Deployed bool
	Def      *appdef.App // nil if removed from the repo but still running
}

type Status struct {
	Apps []AppStatus
	// RunningRev is the repo revision of the current profile ("" if none).
	RunningRev string
	HeadRev    string
	// UpToDate is true when the running profile matches the repo.
	UpToDate bool
}

func slotOf(app string) string {
	for _, s := range activate.Slots {
		if exec.Command("systemctl", "is-active", "--quiet", activate.SlotUnit(app, s)).Run() == nil {
			return s
		}
	}
	return ""
}

func (e *Engine) Status() (*Status, error) {
	st := &Status{}
	m, _ := activate.ReadManifest(e.Profile)
	if m == nil {
		m = &activate.Manifest{}
	}
	st.RunningRev = m.Rev

	apps, err := e.Apps()
	if err != nil {
		return nil, err
	}
	byName := map[string]*AppStatus{}
	for _, a := range apps {
		byName[a.Name] = &AppStatus{Name: a.Name, Domains: a.Domains, Def: a}
	}
	for name, app := range m.Apps {
		s, ok := byName[name]
		if !ok {
			s = &AppStatus{Name: name}
			byName[name] = s
		}
		s.Deployed = true
		s.Domains = app.Domains
		if s.Slot = slotOf(name); s.Slot != "" {
			s.Port = app.Ports[s.Slot]
		}
	}
	for _, s := range byName {
		st.Apps = append(st.Apps, *s)
	}
	sort.Slice(st.Apps, func(i, j int) bool { return st.Apps[i].Name < st.Apps[j].Name })

	if r, err := e.Repo(); err == nil {
		st.HeadRev, _ = r.Head()
		st.UpToDate = m.Rev != "" && r.SameTree(m.Rev)
	}
	return st, nil
}

func (e *Engine) Generations() ([]profile.Generation, error) {
	return profile.List(e.Profile)
}
