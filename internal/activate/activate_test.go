package activate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeSystemd struct{ calls []string }

func (f *fakeSystemd) DaemonReload() error    { f.calls = append(f.calls, "reload"); return nil }
func (f *fakeSystemd) Restart(u string) error { f.calls = append(f.calls, "restart "+u); return nil }
func (f *fakeSystemd) Start(u string) error   { f.calls = append(f.calls, "start "+u); return nil }
func (f *fakeSystemd) Stop(u string) error    { f.calls = append(f.calls, "stop "+u); return nil }

// mkProfile writes a fake profile with the given app -> unit file contents.
func mkProfile(t *testing.T, apps map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "units"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Apps: map[string]AppManifest{}}
	for name, content := range apps {
		unit := "coolix-app-" + name + "@.service"
		m.Apps[name] = AppManifest{Unit: unit}
		if err := os.WriteFile(filepath.Join(dir, "units", unit), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestActivateLifecycle(t *testing.T) {
	sd := &fakeSystemd{}
	a := &Activator{UnitDir: t.TempDir(), Systemd: sd}

	res, err := a.Activate(mkProfile(t, map[string]string{"blog": "v1", "api": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Started, []string{"api", "blog"}) {
		t.Fatalf("first activation started %v", res.Started)
	}

	sd.calls = nil
	res, err = a.Activate(mkProfile(t, map[string]string{"blog": "v2", "api": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Restarted, []string{"blog"}) || !reflect.DeepEqual(res.Unchanged, []string{"api"}) {
		t.Fatalf("second activation: %+v", res)
	}

	sd.calls = nil
	res, err = a.Activate(mkProfile(t, map[string]string{"api": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Removed, []string{"blog"}) {
		t.Fatalf("third activation: %+v", res)
	}
	if sd.calls[0] != "stop coolix-app-blog@blue.service" {
		t.Fatalf("removed app must be stopped before reload, calls: %v", sd.calls)
	}
	if _, err := os.Lstat(filepath.Join(a.UnitDir, "coolix-app-blog@.service")); !os.IsNotExist(err) {
		t.Fatal("removed app's unit link still present")
	}
}

func TestActivateIgnoresForeignUnits(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "coolix-app-x@.service")
	if err := os.WriteFile(foreign, []byte("not a link"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Activator{UnitDir: dir, Systemd: &fakeSystemd{}}
	if _, err := a.Activate(mkProfile(t, map[string]string{})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("regular file that coolix didn't create was removed")
	}
}
