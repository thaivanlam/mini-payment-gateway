// Package secrets encrypts merchant secrets at rest.
//
// Why encryption and not a hash: request signing (HMAC-SHA256) and webhook
// signing are symmetric schemes. To verify a merchant signature the server must
// recompute it, which requires the secret itself -- a bcrypt hash cannot do
// that. So the secret is stored as AES-256-GCM ciphertext, keyed by
// SECRET_ENC_KEY, which lives outside the database. A database dump alone is
// therefore not enough to forge a request.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrDecrypt is returned when a ciphertext cannot be authenticated.
var ErrDecrypt = errors.New("secret could not be decrypted")

// Cipher encrypts and decrypts short secrets.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext || tag).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secrets: read nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A tampered ciphertext fails the GCM tag check.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	n := c.aead.NonceSize()
	if len(raw) < n {
		return "", ErrDecrypt
	}
	plaintext, err := c.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return string(plaintext), nil
}
