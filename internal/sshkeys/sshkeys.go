// Package sshkeys keeps the SSH keys that may `git push` to coolix, added in
// the UI or with `coolix ssh-key add`. sshd asks for them through
// AuthorizedKeysCommand (`coolix ssh-authorized-keys`), as the coolix user.
// Keys declared in services.coolix.git.authorizedKeys come from NixOS
// instead.
package sshkeys

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Key struct {
	Name        string    `json:"name"`
	Key         string    `json:"key"` // "<type> <base64>"
	Fingerprint string    `json:"fingerprint"`
	Added       time.Time `json:"added"`
}

type Store struct {
	Path string
	mu   sync.Mutex
}

// FileName is the store's file in the state dir.
const FileName = "ssh-keys.json"

func Open(stateDir string) *Store { return &Store{Path: filepath.Join(stateDir, FileName)} }

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}$`)

func (s *Store) load() ([]Key, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var keys []Key
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return keys, nil
}

func (s *Store) save(keys []Key) error {
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.Path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(s.Path+".tmp", s.Path)
}

func (s *Store) List() ([]Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Parse reads one authorized_keys style line; name defaults to its comment.
func Parse(name, line string) (Key, error) {
	pub, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return Key{}, errors.New("not an SSH public key (paste the contents of e.g. ~/.ssh/id_ed25519.pub)")
	}
	if len(options) > 0 {
		return Key{}, errors.New("paste the key without options")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = strings.TrimSpace(comment)
	}
	if !nameRe.MatchString(name) {
		return Key{}, fmt.Errorf("key name %q: use up to 64 letters, digits and ._@+-", name)
	}
	return Key{
		Name:        name,
		Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
		Added:       time.Now().UTC(),
	}, nil
}

// Add stores a key; names and keys must be unique.
func (s *Store) Add(name, line string) (Key, error) {
	k, err := Parse(name, line)
	if err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.load()
	if err != nil {
		return Key{}, err
	}
	for _, o := range keys {
		switch {
		case o.Fingerprint == k.Fingerprint:
			return Key{}, fmt.Errorf("this key is already added as %s", o.Name)
		case o.Name == k.Name:
			return Key{}, fmt.Errorf("a key named %s already exists", k.Name)
		}
	}
	return k, s.save(append(keys, k))
}

// Remove deletes the key with this name or fingerprint.
func (s *Store) Remove(nameOrFingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.load()
	if err != nil {
		return err
	}
	for i, k := range keys {
		if k.Name == nameOrFingerprint || k.Fingerprint == nameOrFingerprint {
			return s.save(append(keys[:i], keys[i+1:]...))
		}
	}
	return fmt.Errorf("no SSH key %q", nameOrFingerprint)
}

// WriteAuthorizedKeys prints the keys for sshd, without any rights beyond
// running the forced command.
func WriteAuthorizedKeys(w io.Writer, keys []Key) error {
	var b bytes.Buffer
	for _, k := range keys {
		if strings.ContainsAny(k.Key, "\n\r") {
			continue
		}
		fmt.Fprintf(&b, "restrict %s %s\n", k.Key, k.Name)
	}
	_, err := w.Write(b.Bytes())
	return err
}

// Identify finds the key a session authenticated with, from the file sshd
// names in SSH_USER_AUTH (ExposeAuthInfo): lines like
// "publickey ssh-ed25519 AAAA...". It returns the key's fingerprint and its
// name ("" for keys declared in NixOS).
func Identify(authInfo string, keys []Key) (fingerprint, name string) {
	for _, line := range strings.Split(authInfo, "\n") {
		rest, ok := strings.CutPrefix(line, "publickey ")
		if !ok {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			continue
		}
		fingerprint = ssh.FingerprintSHA256(pub)
		for _, k := range keys {
			if k.Fingerprint == fingerprint {
				return fingerprint, k.Name
			}
		}
	}
	return fingerprint, ""
}
