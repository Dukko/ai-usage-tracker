// Package secrets encrypts credentials at rest with AES-256-GCM.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Box struct{ aead cipher.AEAD }

// Load derives the key from TRACKER_SECRET_KEY if set, otherwise it reads
// (or creates) <dataDir>/secret.key.
func Load(dataDir string) (*Box, error) {
	var key []byte
	if pass := os.Getenv("TRACKER_SECRET_KEY"); pass != "" {
		sum := sha256.Sum256([]byte(pass))
		key = sum[:]
	} else {
		path := filepath.Join(dataDir, "secret.key")
		b, err := os.ReadFile(path)
		switch {
		case err == nil && len(b) == 32:
			key = b
		case err == nil:
			return nil, fmt.Errorf("%s: expected 32 bytes, got %d", path, len(b))
		case errors.Is(err, os.ErrNotExist):
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, key, 0o600); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, errors.New("cannot decrypt credentials (wrong TRACKER_SECRET_KEY or secret.key?)")
	}
	return plain, nil
}
