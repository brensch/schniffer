// Package secrets wraps AES-256-GCM for encrypting rec.gov passwords at rest
// in sqlite. The master key is loaded from the SCHNIFFER_ENC_KEY env var
// (base64-std, 32 raw bytes). Ciphertext layout: [nonce(12) || ciphertext+tag].
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

const EnvKey = "SCHNIFFER_ENC_KEY"

type Key [32]byte

// LoadKeyFromEnv reads SCHNIFFER_ENC_KEY, base64-decodes it, and returns the
// 32-byte key. Errors if the env var is missing or the wrong size.
func LoadKeyFromEnv() (Key, error) {
	raw := os.Getenv(EnvKey)
	if raw == "" {
		return Key{}, fmt.Errorf("%s not set", EnvKey)
	}
	return DecodeKey(raw)
}

func DecodeKey(b64 string) (Key, error) {
	var k Key
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return k, fmt.Errorf("decode key: %w", err)
	}
	if len(decoded) != len(k) {
		return k, fmt.Errorf("key must be %d bytes, got %d", len(k), len(decoded))
	}
	copy(k[:], decoded)
	return k, nil
}

func GenerateKey() (string, error) {
	var k Key
	if _, err := io.ReadFull(rand.Reader, k[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

type Box struct {
	gcm cipher.AEAD
}

func New(k Key) (*Box, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	n := b.gcm.NonceSize()
	if len(ciphertext) < n {
		return nil, errors.New("ciphertext too short")
	}
	return b.gcm.Open(nil, ciphertext[:n], ciphertext[n:], nil)
}
