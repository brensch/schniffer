package secrets

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	k64, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := DecodeKey(k64)
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("hunter2-correct-horse-battery-staple")
	ct, err := box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	out, err := box.Open(ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("got %q want %q", out, plain)
	}
}

func TestOpenRejectsTamper(t *testing.T) {
	k64, _ := GenerateKey()
	k, _ := DecodeKey(k64)
	box, _ := New(k)
	ct, _ := box.Seal([]byte("secret"))
	ct[len(ct)-1] ^= 0x01
	if _, err := box.Open(ct); err == nil {
		t.Fatal("expected open to fail on tampered ciphertext")
	}
}

func TestDecodeKeyWrongSize(t *testing.T) {
	if _, err := DecodeKey("dGlueQ=="); err == nil {
		t.Fatal("expected error for short key")
	}
}
