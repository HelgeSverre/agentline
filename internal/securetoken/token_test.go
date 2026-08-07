package securetoken

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestNewReturnsUniqueRawURLTokens(t *testing.T) {
	a, err := New(32)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(32)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated tokens are equal")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("token is not raw URL-safe base64: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded token has length %d, want 32", len(decoded))
	}
}

func TestNewRejectsNonPositiveSize(t *testing.T) {
	if _, err := New(0); err == nil {
		t.Fatal("New(0) returned no error")
	}
}

func TestHashIsStableAndInputSensitive(t *testing.T) {
	a := Hash("secret")
	if a != Hash("secret") {
		t.Fatal("same input produced different hashes")
	}
	if a == Hash("different") {
		t.Fatal("different inputs produced equal hashes")
	}
}

func TestHashMatchesKnownSHA256Vector(t *testing.T) {
	hash := Hash("abc")
	got := hex.EncodeToString(hash[:])
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("Hash(abc) = %s, want %s", got, want)
	}
}
