package sshkeys

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T) string {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestStore(t *testing.T) {
	keyA, keyB := newKey(t)+" alice@laptop", newKey(t)
	s := Open(t.TempDir())
	k, err := s.Add("", keyA)
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "alice@laptop" || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("parsed %+v", k)
	}
	if _, err := s.Add("other", keyA); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := s.Add("alice@laptop", keyB); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.Add("", keyB); err == nil {
		t.Fatal("key without a name accepted")
	}
	for _, bad := range []string{"nonsense", `command="sh" ` + keyB, "ssh-ed25519 AAAA"} {
		if _, err := s.Add("x", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := s.Add("ci", keyB); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.List()
	var b strings.Builder
	if err := WriteAuthorizedKeys(&b, keys); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "restrict ssh-ed25519 ") {
		t.Fatalf("authorized keys:\n%s", b.String())
	}

	fp, name := Identify("password\npublickey "+strings.Join(strings.Fields(keyB)[:2], " ")+"\n", keys)
	if name != "ci" || fp == "" {
		t.Fatalf("identify: %q %q", fp, name)
	}
	if err := s.Remove("ci"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(k.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.List(); len(keys) != 0 {
		t.Fatalf("left: %v", keys)
	}
}
