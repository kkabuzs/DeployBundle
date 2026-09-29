package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

func ReadSymmetric(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) == 32 {
		return b, nil
	}
	trimmed := strings.TrimSpace(string(b))
	if len(trimmed) == 64 {
		decoded, err := hex.DecodeString(trimmed)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("symmetric key must be 32 raw bytes or 64 hex characters, got %d bytes", len(b))
}

func WriteSymmetric(path string) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return writePrivate(path, b)
}

func ReadPrivate(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = decodeMaybeHex(b)
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(append([]byte(nil), b...)), nil
	default:
		return nil, fmt.Errorf("Ed25519 private key must be %d-byte seed or %d-byte private key, got %d", ed25519.SeedSize, ed25519.PrivateKeySize, len(b))
	}
}

func ReadPublic(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = decodeMaybeHex(b)
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 public key must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(append([]byte(nil), b...)), nil
}

func GenerateSigningFiles(privatePath, publicPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writePrivate(privatePath, priv.Seed()); err != nil {
		return err
	}
	if err := os.WriteFile(publicPath, pub, 0600); err != nil {
		return err
	}
	return nil
}

func writePrivate(path string, b []byte) error {
	if path == "" {
		return errors.New("key output path is empty")
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	return nil
}

func decodeMaybeHex(b []byte) []byte {
	trimmed := strings.TrimSpace(string(b))
	if len(trimmed)%2 == 0 {
		if decoded, err := hex.DecodeString(trimmed); err == nil {
			return decoded
		}
	}
	return b
}
