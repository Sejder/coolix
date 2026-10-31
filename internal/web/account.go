package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/argon2"
)

// Account is the single admin account, stored in <stateDir>/account.json
// (0600, coolix user). It's created through the setup page with a one-time
// setup code, or implied by services.coolix.ui.adminPasswordFile.
type Account struct {
	UserID       []byte    `json:"user_id"`
	PasswordHash string    `json:"password_hash,omitempty"`
	Passkeys     []Passkey `json:"passkeys,omitempty"`
	// Epoch is bumped whenever a credential is removed or the password
	// changes; sessions signed under an older epoch are void.
	Epoch int `json:"epoch"`
}

type Passkey struct {
	Name       string              `json:"name"`
	Created    time.Time           `json:"created"`
	LastUsed   time.Time           `json:"last_used,omitzero"`
	Credential webauthn.Credential `json:"credential"`
}

// ID is a stable, URL-safe identifier of the passkey.
func (p Passkey) ID() string { return base64.RawURLEncoding.EncodeToString(p.Credential.ID) }

// HasCredential reports whether anyone can sign in to the account.
func (a *Account) HasCredential() bool { return a.PasswordHash != "" || len(a.Passkeys) > 0 }

// webauthn.User for the admin.
type accountUser struct{ a *Account }

func (u accountUser) WebAuthnID() []byte          { return u.a.UserID }
func (u accountUser) WebAuthnName() string        { return "admin" }
func (u accountUser) WebAuthnDisplayName() string { return "coolix admin" }
func (u accountUser) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, len(u.a.Passkeys))
	for i, p := range u.a.Passkeys {
		out[i] = p.Credential
	}
	return out
}

// AccountStore loads and saves the account; all access goes through it.
type AccountStore struct {
	Path string
	mu   sync.Mutex
}

func (s *AccountStore) load() (*Account, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &Account{}, nil
	} else if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return &a, nil
}

// Get returns a copy of the account.
func (s *AccountStore) Get() (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Update changes the account atomically.
func (s *AccountStore) Update(change func(*Account) error) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.load()
	if err != nil {
		return nil, err
	}
	if len(a.UserID) == 0 {
		a.UserID = make([]byte, 64)
		if _, err := rand.Read(a.UserID); err != nil {
			return nil, err
		}
	}
	if err := change(a); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, err
	}
	return a, os.Rename(tmp, s.Path)
}

// Passwords ----------------------------------------------------------------

const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 1
)

// HashPassword returns an argon2id PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a HashPassword string.
func VerifyPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var mem, iter uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iter, &threads); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, iter, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func checkNewPassword(pw, confirm string) error {
	if len(pw) < MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", MinPasswordLength)
	}
	if pw != confirm {
		return errors.New("the passwords don't match")
	}
	return nil
}

// Setup codes --------------------------------------------------------------

// SetupCodeTTL is how long a setup code works.
const SetupCodeTTL = time.Hour

type setupCode struct {
	Hash    []byte    `json:"hash"`
	Expires time.Time `json:"expires"`
}

func setupCodePath(stateDir string) string { return filepath.Join(stateDir, "setup-code.json") }

func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

// NewSetupCode creates (replacing any earlier one) a one-time code that
// claims an account without credentials. Only someone with a shell on the
// host can run `coolix setup-code`, so a stranger who finds a fresh install
// first can't take it over.
func NewSetupCode(stateDir string) (string, error) {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	code := base32.StdEncoding.EncodeToString(raw) // 16 chars, 80 bits
	sum := sha256.Sum256([]byte(code))
	data, _ := json.Marshal(setupCode{Hash: sum[:], Expires: time.Now().Add(SetupCodeTTL)})
	p := setupCodePath(stateDir)
	if err := os.WriteFile(p+".tmp", data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return "", err
	}
	return code[:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:], nil
}

func checkSetupCode(stateDir, code string) bool {
	data, err := os.ReadFile(setupCodePath(stateDir))
	if err != nil {
		return false
	}
	var c setupCode
	if json.Unmarshal(data, &c) != nil || time.Now().After(c.Expires) {
		return false
	}
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return subtle.ConstantTimeCompare(sum[:], c.Hash) == 1
}

func clearSetupCode(stateDir string) { os.Remove(setupCodePath(stateDir)) }

// ResetAccount removes every credential (recovery: then claim it again with
// a setup code).
func ResetAccount(stateDir string) error {
	s := &AccountStore{Path: filepath.Join(stateDir, "account.json")}
	_, err := s.Update(func(a *Account) error {
		a.PasswordHash, a.Passkeys = "", nil
		a.Epoch++
		return nil
	})
	return err
}
