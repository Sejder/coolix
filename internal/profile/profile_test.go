package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListAndFind(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"1", "2", "10"} {
		target := filepath.Join(dir, "store-"+n)
		os.MkdirAll(target, 0o755)
		os.WriteFile(filepath.Join(target, "apps.json"),
			[]byte(`{"version":1,"rev":"rev`+n+`","apps":{"b":{},"a":{}}}`), 0o644)
		if err := os.Symlink(target, filepath.Join(dir, "apps-"+n+"-link")); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("apps-10-link", filepath.Join(dir, "apps"))
	os.WriteFile(filepath.Join(dir, "apps-x-link"), nil, 0o644) // ignored

	gens, err := List(filepath.Join(dir, "apps"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 3 || gens[2].Number != 10 || !gens[2].Current || gens[0].Rev != "rev1" {
		t.Fatalf("unexpected generations: %+v", gens)
	}
	if gens[0].Apps[0] != "a" {
		t.Fatalf("apps not sorted: %v", gens[0].Apps)
	}

	prev, err := Find(gens, 0)
	if err != nil || prev.Number != 2 {
		t.Fatalf("previous: %+v %v", prev, err)
	}
	if _, err := Find(gens, 7); err == nil {
		t.Fatal("expected error for missing generation")
	}
	if _, err := Find(gens[:1], 0); err == nil {
		t.Fatal("expected error when current is the first generation")
	}
}

func TestStorePathRejects(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd",
		"abc-coolix-app-profile",
		"0000000000000000000000000000000e-coolix-app-profile", // 'e' is not in nix base32
		"00000000000000000000000000000000-something-else",
		"00000000000000000000000000000000-coolix-app-profile/../x",
	} {
		if _, err := StorePath(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
