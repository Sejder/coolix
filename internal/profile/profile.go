// Package profile reads and validates app profile generations without
// touching the nix profile lock, so it works for unprivileged users.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"coolix/internal/activate"
)

type Generation struct {
	Number  int
	Path    string // store path
	Time    time.Time
	Current bool
	Rev     string // apps repo commit the profile was built from
	Apps    []string
}

// List returns the generations of the profile at path (e.g.
// /nix/var/nix/profiles/coolix/apps), oldest first.
func List(path string) ([]Generation, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `-(\d+)-link$`)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	current, _ := os.Readlink(path)
	var gens []Generation
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		link := filepath.Join(dir, e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		info, err := os.Lstat(link)
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		g := Generation{Number: n, Path: target, Time: info.ModTime(), Current: e.Name() == current}
		if man, err := activate.ReadManifest(target); err == nil {
			g.Rev = man.Rev
			for name := range man.Apps {
				g.Apps = append(g.Apps, name)
			}
			sort.Strings(g.Apps)
		}
		gens = append(gens, g)
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].Number < gens[j].Number })
	return gens, nil
}

// Find returns generation n, or the one before the current if n == 0.
func Find(gens []Generation, n int) (*Generation, error) {
	if n == 0 {
		for i, g := range gens {
			if g.Current {
				if i == 0 {
					return nil, errors.New("no generation before the current one")
				}
				return &gens[i-1], nil
			}
		}
		return nil, errors.New("no current generation")
	}
	for i := range gens {
		if gens[i].Number == n {
			return &gens[i], nil
		}
	}
	return nil, fmt.Errorf("generation %d does not exist", n)
}

var storeNameRe = regexp.MustCompile(`^[0-9a-df-np-sv-z]{32}-coolix-app-profile$`)

// StorePath validates a profile store-path basename (as passed to the
// privileged coolix-activate@ unit) and returns the full store path.
func StorePath(name string) (string, error) {
	name = strings.TrimPrefix(name, "/nix/store/")
	if !storeNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a coolix app profile store path", name)
	}
	p := "/nix/store/" + name
	if _, err := os.Stat(filepath.Join(p, "apps.json")); err != nil {
		return "", fmt.Errorf("%s: not a valid app profile: %w", p, err)
	}
	return p, nil
}
