package activate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"coolix/internal/units"
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

const testPG = "/nix/store/1jzvzdyhmwjm2xdw2iak5p5x8qj2pkgr-postgresql-18.1"

// mkProfile writes a fake profile: app -> version (changes the rendered unit);
// apps listed in withDB get a postgres database.
func mkProfile(t *testing.T, apps map[string]string, withDB ...string) string {
	t.Helper()
	dir := t.TempDir()
	m := units.Manifest{Version: units.ManifestVersion, Apps: map[string]units.App{}}
	names := make([]string, 0, len(apps))
	for name := range apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		app := units.App{
			Exe:      "/nix/store/0c6sg6wp6c6k3mhwkzv8jk4xkkr5mpyh-" + name + "/bin/" + name,
			PortBase: 20000 + 2*i,
			Domains:  []string{name + ".test"},
			Env:      map[string]string{"VERSION": apps[name]},
		}
		if slices.Contains(withDB, name) {
			app.Databases = []units.Database{{Kind: "postgres", Package: testPG}}
		}
		m.Apps[name] = app
	}
	data, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "apps.json"), data, 0o644)
	return dir
}

func newActivator(t *testing.T) (*Activator, *fakeSystemd, *fakeRouter) {
	sd, r := newFake(), &fakeRouter{routes: map[string]int{}}
	clock := time.Unix(0, 0)
	return &Activator{UnitDir: t.TempDir(), RenderDir: t.TempDir(), Systemd: sd, Router: r,
		Policy:  units.Policy{PortMin: 20000, PortMax: 29999},
		Options: units.Options{Exec: "/etc/coolix/coolix", SecretsDir: "/var/lib/coolix-secrets"},
		Sleep:   func(d time.Duration) { clock = clock.Add(d) },
		Now:     func() time.Time { return clock }}, sd, r
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
	p := mkProfile(t, map[string]string{"blog": "v1"}, "blog")
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

func TestRefusesInvalidProfile(t *testing.T) {
	a, sd, _ := newActivator(t)
	p := mkProfile(t, map[string]string{"blog": "v1"})
	data, _ := os.ReadFile(filepath.Join(p, "apps.json"))
	evil := strings.Replace(string(data), `"env":{`, `"hardening":{"DynamicUser":false},"env":{`, 1)
	os.WriteFile(filepath.Join(p, "apps.json"), []byte(evil), 0o644)
	if _, err := a.Activate(context.Background(), p); err == nil || !strings.Contains(err.Error(), "DynamicUser") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if len(sd.calls) != 0 {
		t.Fatalf("systemd touched: %v", sd.calls)
	}
	if entries, _ := os.ReadDir(a.UnitDir); len(entries) != 0 {
		t.Fatal("units linked for a refused profile")
	}
}

func TestPrunesOldRenders(t *testing.T) {
	ctx := context.Background()
	a, _, _ := newActivator(t)
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": v})); err != nil {
			t.Fatal(err)
		}
	}
	if entries, _ := os.ReadDir(a.RenderDir); len(entries) != 1 {
		t.Fatalf("want 1 rendered dir, got %d", len(entries))
	}
}

func TestBrokenUnchangedAppDoesNotFailDeploy(t *testing.T) {
	ctx := context.Background()
	a, sd, _ := newActivator(t)
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"api": "v1", "blog": "v1"})); err != nil {
		t.Fatal(err)
	}
	sd.active["coolix-app-api@blue.service"] = false // crashed
	a.Health = func(_ context.Context, port int, _ string) error {
		if port == 20000 || port == 20001 { // api
			return errors.New("connection refused")
		}
		return nil
	}
	res, err := a.Activate(ctx, mkProfile(t, map[string]string{"api": "v1", "blog": "v2"}))
	if err != nil {
		t.Fatalf("deploy of blog failed because api was down: %v", err)
	}
	if !reflect.DeepEqual(res.Down, []string{"api"}) || !reflect.DeepEqual(res.Switched, []string{"blog"}) {
		t.Fatalf("%+v", res)
	}
}

func TestRemovedDatabaseStopsAfterSwitch(t *testing.T) {
	ctx := context.Background()
	a, sd, _ := newActivator(t)
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"}, "blog")); err != nil {
		t.Fatal(err)
	}
	sd.calls = nil
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"})); err != nil {
		t.Fatal(err)
	}
	stop, start := indexOf(sd.calls, "stop coolix-db-blog-postgres.service"), indexOf(sd.calls, "restart coolix-app-blog@green.service")
	if stop < 0 || start < 0 || stop < start {
		t.Fatalf("database must stop after the new slot started: %v", sd.calls)
	}
}

func TestFailedSwitchKeepsRemovedDatabase(t *testing.T) {
	ctx := context.Background()
	a, sd, _ := newActivator(t)
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v1"}, "blog")); err != nil {
		t.Fatal(err)
	}
	a.Health = func(context.Context, int, string) error { return errors.New("down") }
	if _, err := a.Activate(ctx, mkProfile(t, map[string]string{"blog": "v2"})); err == nil {
		t.Fatal("expected failure")
	}
	if !sd.active["coolix-db-blog-postgres.service"] {
		t.Fatal("database of the still-serving old slot was stopped")
	}
}

func TestAppOfAnyUnit(t *testing.T) {
	for unit, want := range map[string]string{
		"coolix-app-a-b@.service":        "a-b",
		"coolix-db-a-b-postgres.service": "a-b",
		"coolix-db-x-redis.service":      "x",
		"coolix-backup-blog.timer":       "blog",
		"coolix-restore-blog@.service":   "blog",
		"sshd.service":                   "",
	} {
		if got := units.AppOfAnyUnit(unit); got != want {
			t.Errorf("%s: %q", unit, got)
		}
	}
}
