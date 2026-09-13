package auth

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

func generateKey(t *testing.T) (*mldsa65.PublicKey, *mldsa65.PrivateKey) {
	t.Helper()
	pub, priv, err := mldsa65.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func randomChallenge() [32]byte {
	var c [32]byte
	if _, err := rand.Read(c[:]); err != nil {
		panic(err)
	}
	return c
}

// TestRoundTrip: generate keypair, sign, verify.
func TestRoundTrip(t *testing.T) {
	pub, priv := generateKey(t)
	challenge := randomChallenge()
	connectorID := "connector-a"

	sig := Sign(priv, challenge, connectorID)

	msg := make([]byte, 32+len(connectorID))
	copy(msg[:32], challenge[:])
	copy(msg[32:], connectorID)

	if !mldsa65.Verify(pub, msg, nil, sig) {
		t.Fatal("signature verification failed")
	}
}

// TestRejectsWrongKey: sign with key A, verify with key B → should fail.
func TestRejectsWrongKey(t *testing.T) {
	_, privA := generateKey(t)
	pubB, _ := generateKey(t)

	challenge := randomChallenge()
	sig := Sign(privA, challenge, "connector-a")

	msg := make([]byte, 32+len("connector-a"))
	copy(msg[:32], challenge[:])
	copy(msg[32:], "connector-a")

	if mldsa65.Verify(pubB, msg, nil, sig) {
		t.Fatal("verification should fail with wrong key")
	}
}

// TestRejectsCrossConnectorReplay: sign for connector-a, verify as connector-b → should fail.
func TestRejectsCrossConnectorReplay(t *testing.T) {
	pub, priv := generateKey(t)
	challenge := randomChallenge()

	sig := Sign(priv, challenge, "connector-a")

	// Verify using connector-b in the message
	msg := make([]byte, 32+len("connector-b"))
	copy(msg[:32], challenge[:])
	copy(msg[32:], "connector-b")

	if mldsa65.Verify(pub, msg, nil, sig) {
		t.Fatal("cross-connector replay should fail")
	}
}

// TestUnmarshalPrivateKey: marshal/unmarshal roundtrip.
func TestUnmarshalPrivateKey(t *testing.T) {
	_, priv := generateKey(t)

	var buf [mldsa65.PrivateKeySize]byte
	priv.Pack(&buf)

	priv2, err := UnmarshalPrivateKey(buf[:])
	if err != nil {
		t.Fatalf("UnmarshalPrivateKey: %v", err)
	}

	if !priv.Equal(priv2) {
		t.Fatal("unmarshaled key does not equal original")
	}
}

// TestKeyStoreRoundTrip: Save then Load returns the same key.
func TestKeyStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "key")
	ks := NewKeyStore(path)

	// File not yet present.
	got, exists, err := ks.Load()
	if err != nil || exists || got != nil {
		t.Fatalf("expected no key, got exists=%v err=%v", exists, err)
	}

	_, priv := generateKey(t)
	if err := ks.Save(priv); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Check permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 perms, got %o", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("dir perms: got %o, want 0700", dirInfo.Mode().Perm())
	}

	loaded, exists, err := ks.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !exists {
		t.Fatal("expected key to exist")
	}
	if !priv.Equal(loaded) {
		t.Fatal("loaded key does not match saved key")
	}
}
