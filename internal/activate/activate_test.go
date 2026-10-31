package activate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeSystemd struct {
	calls  []string
	active map[string]bool
}

func newFake() *fakeSystemd { return &fakeSystemd{active: map[string]bool{}} }

func (f *fakeSystemd) DaemonReload() error { f.calls = append(f.calls, "reload"); return nil }
func (f *fakeSystemd) Restart(u string) error {
	f.calls = append(f.calls, "restart "+u)
	f.active[u] = true
	return nil
}
func (f *fakeSystemd) Start(u string) error {
	f.calls = append(f.calls, "start "+u)
	f.active[u] = true
	return nil
}
func (f *fakeSystemd) Stop(u string) error {
	f.calls = append(f.calls, "stop "+u)
	f.active[u] = false
	return nil
}
func (f *fakeSystemd) IsActive(u string) bool { return f.active[u] }

type fakeRouter struct{ routes map[string]int }

func (r *fakeRouter) SetRoute(app string, _ []string, port int) error {
	r.routes[app] = port
	return nil
}
func (r *fakeRouter) RemoveRoute(app string) error { delete(r.routes, app); return nil }

// mkProfile writes a fake profile: app -> unit file contents; db units are plain services.
func mkProfile(t *testing.T, apps map[string]string, dbs ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "units"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Apps: map[string]AppManifest{}, Services: dbs}
	i := 0
	for name, content := range apps {
		unit := "coolix-app-" + name + "@.service"
		m.Apps[name] = AppManifest{Unit: unit, Domains: []string{name + ".test"},
			Ports: map[string]int{"blue": 20000 + 2*i, "green": 20001 + 2*i}}
		i++
		os.WriteFile(filepath.Join(dir, "units", unit), []byte(content), 0o644)
	}
	for _, db := range dbs {
		os.WriteFile(filepath.Join(dir, "units", db), []byte("db"), 0o644)
	}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "apps.json"), data, 0o644)
	return dir
}

func newActivator(t *testing.T) (*Activator, *fakeSystemd, *fakeRouter) {
	sd, r := newFake(), &fakeRouter{routes: map[string]int{}}
	clock := time.Unix(0, 0)
	return &Activator{UnitDir: t.TempDir(), Systemd: sd, Router: r,
		Sleep: func(d time.Duration) { clock = clock.Add(d) },
		Now:   func() time.Time { return clock }}, sd, r
}

func TestBlueGreenLifecycle(t *testing.T) {
	ctx := context.Background()
	a, sd, r := newActivator(t)

	res, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Started, []string{"blog"}) || !sd.active["coolix-app-blog@blue.service"] || r.routes["blog"] != 20000 {
		t.Fatalf("first deploy: %+v routes=%v", res, r.routes)
	}

	res, err = a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v2"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Switched, []string{"blog"}) || r.routes["blog"] != 20001 ||
		!sd.active["coolix-app-blog@green.service"] || sd.active["coolix-app-blog@blue.service"] {
		t.Fatalf("second deploy should switch to green: %+v routes=%v active=%v", res, r.routes, sd.active)
	}

	res, err = a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v3"}))
	if err != nil || r.routes["blog"] != 20000 || !sd.active["coolix-app-blog@blue.service"] {
		t.Fatalf("third deploy should switch back to blue: %v %+v", err, res)
	}

	res, err = a.Activate(ctx, mkProfile(t, map[string]string{}))
	if err != nil || !reflect.DeepEqual(res.Removed, []string{"blog"}) {
		t.Fatalf("removal: %v %+v", err, res)
	}
	if _, ok := r.routes["blog"]; ok || sd.active["coolix-app-blog@blue.service"] {
		t.Fatal("removed app still routed or running")
	}
}

func TestUnhealthyKeepsOldSlot(t *testing.T) {
	ctx := context.Background()
	a, sd, r := newActivator(t)
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"})); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(a.UnitDir, "coolix-app-blog@.service")
	before, _ := os.Readlink(link)

	a.Health = func(context.Context, int, string) error { return errors.New("connection refused") }
	start := time.Now()
	res, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "broken"}))
	if err == nil || !reflect.DeepEqual(res.Failed, []string{"blog"}) {
		t.Fatalf("expected failure: %v %+v", err, res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("sleep was not faked")
	}
	if !sd.active["coolix-app-blog@blue.service"] || sd.active["coolix-app-blog@green.service"] || r.routes["blog"] != 20000 {
		t.Fatalf("old slot must keep serving: active=%v routes=%v", sd.active, r.routes)
	}
	if after, _ := os.Readlink(link); after != before {
		t.Fatalf("unit link not restored: %s != %s", after, before)
	}
}

func TestUnchangedAppStartsAfterBoot(t *testing.T) {
	ctx := context.Background()
	a, sd, _ := newActivator(t)
	p := mkProfile(t, map[string]string{"blog": "v1"})
	if _, err := a.Activate(ctx, p); err != nil {
		t.Fatal(err)
	}
	sd.active = map[string]bool{} // reboot: nothing running, links still there (re-created)
	res, err := a.Activate(ctx, p)
	if err != nil || !sd.active["coolix-app-blog@blue.service"] || !reflect.DeepEqual(res.Unchanged, []string{"blog"}) {
		t.Fatalf("unchanged app not started: %v %+v", err, res)
	}
}

func TestForceRestartAndServices(t *testing.T) {
	ctx := context.Background()
	a, sd, r := newActivator(t)
	p := mkProfile(t, map[string]string{"blog": "v1"}, "coolix-db-blog-postgres.service")
	if _, err := a.Activate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if !sd.active["coolix-db-blog-postgres.service"] {
		t.Fatal("db service not started")
	}
	if i, j := indexOf(sd.calls, "start coolix-db-blog-postgres.service"), indexOf(sd.calls, "restart coolix-app-blog@blue.service"); i < 0 || i > j {
		t.Fatalf("db must start before the app: %v", sd.calls)
	}
	a.ForceRestart = map[string]bool{"blog": true}
	res, err := a.Activate(ctx, p)
	if err != nil || !reflect.DeepEqual(res.Switched, []string{"blog"}) || r.routes["blog"] != 20001 {
		t.Fatalf("forced restart should switch slots: %v %+v", err, res)
	}
	a.ForceRestart = nil
	res, err = a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	if sd.active["coolix-db-blog-postgres.service"] {
		t.Fatal("removed db still running")
	}
	_ = res
}

func TestIgnoresForeignUnits(t *testing.T) {
	a, _, _ := newActivator(t)
	foreign := filepath.Join(a.UnitDir, "coolix-app-x@.service")
	os.WriteFile(foreign, []byte("not a link"), 0o644)
	if _, err := a.Activate(context.Background(), mkProfile(t, map[string]string{})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("regular file that coolix didn't create was removed")
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if strings.TrimSpace(x) == v {
			return i
		}
	}
	return -1
}
