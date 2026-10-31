package secrets

import (
	"context"
	"encoding/base64"
	"os"
	"reflect"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	in := map[string]string{
		"PLAIN":  "hello",
		"QUOTES": `it's "quoted" $HOME ` + "`x`",
		"MULTI":  "line1\nline2'\n",
		"EMPTY":  "",
	}
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n%q\n%q", in, out)
	}
	for _, bad := range []string{"NOPE", "1X='a'", "X='unterminated"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeStore "encrypts" with base64 so tests can check plaintext never lands on disk.
func fakeStore(t *testing.T) *Store {
	return &Store{
		Dir: t.TempDir(),
		Encrypt: func(_ context.Context, plain []byte, out string) error {
			return os.WriteFile(out, []byte(base64.StdEncoding.EncodeToString(plain)), 0o600)
		},
		Decrypt: func(_ context.Context, in string) ([]byte, error) {
			data, err := os.ReadFile(in)
			if err != nil {
				return nil, err
			}
			return base64.StdEncoding.DecodeString(string(data))
		},
	}
}

func TestPendingAndApply(t *testing.T) {
	ctx := context.Background()
	pending := t.TempDir()
	s := fakeStore(t)

	if err := AddPending(pending, "blog", map[string]string{"A": "1", "B": "2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := AddPending(pending, "blog", nil, []string{"B"}); err != nil {
		t.Fatal(err)
	}
	if err := AddPending(pending, "blog", map[string]string{"bad-key": "x"}, nil); err == nil {
		t.Fatal("accepted invalid key")
	}
	p, _ := ReadPending(pending, "blog")
	if !reflect.DeepEqual(p.Set, map[string]string{"A": "1"}) || !reflect.DeepEqual(p.Unset, []string{"B"}) {
		t.Fatalf("merged pending: %+v", p)
	}
	if info, _ := os.Stat(pendingPath(pending, "blog")); info.Mode().Perm() != 0o600 {
		t.Fatalf("pending file mode %v", info.Mode())
	}

	changed, err := s.Apply(ctx, pending, []string{"blog", "api"}, map[string]bool{"api": true})
	if err != nil {
		t.Fatal(err)
	}
	if !changed["blog"] || !changed["api"] {
		t.Fatalf("changed = %v", changed)
	}
	if _, err := os.Stat(pendingPath(pending, "blog")); !os.IsNotExist(err) {
		t.Fatal("pending file not removed")
	}
	vars, _ := s.read(ctx, "blog")
	if !reflect.DeepEqual(vars, map[string]string{"A": "1"}) {
		t.Fatalf("stored = %v", vars)
	}
	if _, err := os.Stat(s.CredPath("api")); err != nil {
		t.Fatal("app needing a credential got none")
	}

	// Nothing pending: nothing changes.
	changed, err = s.Apply(ctx, pending, []string{"blog", "api"}, map[string]bool{"api": true})
	if err != nil || len(changed) != 0 {
		t.Fatalf("idempotent apply changed %v (%v)", changed, err)
	}

	AddPending(pending, "blog", map[string]string{"C": "3"}, []string{"A"})
	s.Apply(ctx, pending, []string{"blog"}, nil)
	vars, _ = s.read(ctx, "blog")
	if !reflect.DeepEqual(vars, map[string]string{"C": "3"}) {
		t.Fatalf("second apply = %v", vars)
	}
}
