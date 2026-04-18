package oauth

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := newKey(t)
	plaintext := []byte("hello-oauth-token")

	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext == plaintext")
	}

	pt, err := Decrypt(ct, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("got %q, want %q", pt, plaintext)
	}
}

func TestEncrypt_RejectsKeyOfWrongLength(t *testing.T) {
	_, err := Encrypt([]byte("x"), make([]byte, 16))
	if err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestDecrypt_FailsOnTamperedCiphertext(t *testing.T) {
	key := newKey(t)
	ct, _ := Encrypt([]byte("tamper-me"), key)
	ct[len(ct)-1] ^= 0x01

	if _, err := Decrypt(ct, key); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestEncrypt_NonDeterministic(t *testing.T) {
	key := newKey(t)
	a, _ := Encrypt([]byte("same"), key)
	b, _ := Encrypt([]byte("same"), key)
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of same plaintext produced same ciphertext (nonce reuse)")
	}
}
