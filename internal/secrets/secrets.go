// Package secrets keeps app secrets out of git and the Nix store.
//
// The unprivileged side (CLI/UI, coolix user) only records *pending* changes
// in <stateDir>/secrets-pending/<app>.json (mode 0600). At activation, root
// merges them into /var/lib/coolix-secrets/<app>.cred, encrypted with
// systemd-creds (TPM2 and/or the host key), and deletes the pending file.
// Apps receive the credential through LoadCredentialEncrypted=, decrypted by
// systemd into a private tmpfs, and source it as environment variables.
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// CredName is the credential ID; it must match LoadCredentialEncrypted=env:...
const CredName = "env"

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Encode renders vars as KEY='value' lines that both POSIX sh and systemd's
// EnvironmentFile parser read literally.
func Encode(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(vars[k], "'", `'\''`))
	}
	return b.String()
}

// Decode parses the output of Encode.
func Decode(s string) (map[string]string, error) {
	vars := map[string]string{}
	for len(s) > 0 {
		if s[0] == '\n' {
			s = s[1:]
			continue
		}
		eq := strings.Index(s, "='")
		if eq < 0 || !keyRe.MatchString(s[:eq]) {
			return nil, errors.New("malformed secrets file")
		}
		key := s[:eq]
		s = s[eq+2:]
		var val strings.Builder
		for {
			i := strings.IndexByte(s, '\'')
			if i < 0 {
				return nil, fmt.Errorf("unterminated value for %s", key)
			}
			val.WriteString(s[:i])
			s = s[i+1:]
			if strings.HasPrefix(s, `\''`) {
				val.WriteByte('\'')
				s = s[3:]
				continue
			}
			break
		}
		vars[key] = val.String()
	}
	return vars, nil
}

type Pending struct {
	Set   map[string]string `json:"set,omitempty"`
	Unset []string          `json:"unset,omitempty"`
}

func pendingPath(dir, app string) string { return filepath.Join(dir, app+".json") }

// AddPending records secret changes for app, merging with earlier pending ones.
func AddPending(dir, app string, set map[string]string, unset []string) error {
	for k := range set {
		if !keyRe.MatchString(k) {
			return fmt.Errorf("invalid secret name %q", k)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p, err := ReadPending(dir, app)
	if err != nil {
		return err
	}
	if p.Set == nil {
		p.Set = map[string]string{}
	}
	for k, v := range set {
		p.Set[k] = v
		p.Unset = remove(p.Unset, k)
	}
	for _, k := range unset {
		delete(p.Set, k)
		if !contains(p.Unset, k) {
			p.Unset = append(p.Unset, k)
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := pendingPath(dir, app) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pendingPath(dir, app))
}

func ReadPending(dir, app string) (*Pending, error) {
	data, err := os.ReadFile(pendingPath(dir, app))
	if errors.Is(err, os.ErrNotExist) {
		return &Pending{}, nil
	} else if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", pendingPath(dir, app), err)
	}
	return &p, nil
}

// Store is the root-owned encrypted side.
type Store struct {
	Dir     string // /var/lib/coolix-secrets
	Encrypt func(ctx context.Context, plain []byte, out string) error
	Decrypt func(ctx context.Context, in string) ([]byte, error)
}

func NewSystemdStore(dir string) *Store {
	return &Store{
		Dir: dir,
		Encrypt: func(ctx context.Context, plain []byte, out string) error {
			cmd := exec.CommandContext(ctx, "systemd-creds", "encrypt", "--name="+CredName, "-", out)
			cmd.Stdin = bytes.NewReader(plain)
			if o, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("systemd-creds encrypt: %w\n%s", err, o)
			}
			return nil
		},
		Decrypt: func(ctx context.Context, in string) ([]byte, error) {
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, "systemd-creds", "decrypt", "--name="+CredName, in, "-")
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("systemd-creds decrypt %s: %w\n%s", in, err, stderr.Bytes())
			}
			return out, nil
		},
	}
}

func (s *Store) CredPath(app string) string { return filepath.Join(s.Dir, app+".cred") }

func (s *Store) read(ctx context.Context, app string) (map[string]string, error) {
	if _, err := os.Stat(s.CredPath(app)); errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	plain, err := s.Decrypt(ctx, s.CredPath(app))
	if err != nil {
		return nil, err
	}
	return Decode(string(plain))
}

func (s *Store) write(ctx context.Context, app string, vars map[string]string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp := s.CredPath(app) + ".tmp"
	if err := s.Encrypt(ctx, []byte(Encode(vars)), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, s.CredPath(app))
}

// Apply merges pending changes for the given apps into the store, and makes
// sure every app in needCred has a credential (systemd refuses to start a unit
// whose LoadCredentialEncrypted file is missing). It returns the apps whose
// secrets changed and therefore need a restart.
func (s *Store) Apply(ctx context.Context, pendingDir string, apps []string, needCred map[string]bool) (map[string]bool, error) {
	changed := map[string]bool{}
	dir, err := openPendingDir(pendingDir)
	if err != nil {
		return changed, err
	}
	defer dir.close()
	for _, app := range apps {
		p, err := dir.read(app)
		if err != nil {
			return changed, err
		}
		_, statErr := os.Stat(s.CredPath(app))
		if len(p.Set) == 0 && len(p.Unset) == 0 && !(needCred[app] && statErr != nil) {
			continue
		}
		vars, err := s.read(ctx, app)
		if err != nil {
			return changed, err
		}
		for k, v := range p.Set {
			vars[k] = v
		}
		for _, k := range p.Unset {
			delete(vars, k)
		}
		if err := s.write(ctx, app, vars); err != nil {
			return changed, err
		}
		if err := dir.remove(app); err != nil {
			return changed, err
		}
		changed[app] = true
	}
	return changed, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func remove(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// pendingDir is the coolix-writable pending directory as seen by root: it
// is opened without following symlinks, and files in it are read with
// O_NOFOLLOW, so the unprivileged side can't point root at other files.
type pendingDir struct{ fd int }

func openPendingDir(dir string) (*pendingDir, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}
	return &pendingDir{fd}, nil
}

func (d *pendingDir) close() {
	if d != nil {
		syscall.Close(d.fd)
	}
}

func (d *pendingDir) read(app string) (*Pending, error) {
	if d == nil {
		return &Pending{}, nil
	}
	fd, err := syscall.Openat(d.fd, app+".json", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return &Pending{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("pending secrets of %s: %w", app, err)
	}
	f := os.NewFile(uintptr(fd), app+".json")
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil, fmt.Errorf("pending secrets of %s: not a regular file", app)
	}
	var p Pending
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&p); err != nil {
		return nil, fmt.Errorf("pending secrets of %s: %w", app, err)
	}
	for k := range p.Set {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("pending secrets of %s: invalid name %q", app, k)
		}
	}
	for _, k := range p.Unset {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("pending secrets of %s: invalid name %q", app, k)
		}
	}
	return &p, nil
}

func (d *pendingDir) remove(app string) error {
	if d == nil {
		return nil
	}
	if err := syscall.Unlinkat(d.fd, app+".json"); err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return nil
}
