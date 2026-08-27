package secrets

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewCipher(testKey())
	require.NoError(t, err)

	plaintext := "sk_test_0123456789abcdef"

	ciphertext, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	assert.NotContains(t, ciphertext, plaintext, "the secret must not be readable in the stored value")

	got, err := c.Decrypt(ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

// TestEncryptIsNondeterministic checks the nonce is doing its job: encrypting
// the same secret twice must not produce the same ciphertext, or an attacker
// with the database could tell which merchants share a secret.
func TestEncryptIsNondeterministic(t *testing.T) {
	c, err := NewCipher(testKey())
	require.NoError(t, err)

	first, err := c.Encrypt("same-secret")
	require.NoError(t, err)
	second, err := c.Encrypt("same-secret")
	require.NoError(t, err)

	assert.NotEqual(t, first, second)
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c, err := NewCipher(testKey())
	require.NoError(t, err)

	ciphertext, err := c.Encrypt("secret")
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xff // flip a bit in the auth tag
	tampered := base64.StdEncoding.EncodeToString(raw)

	_, err = c.Decrypt(tampered)
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	c1, err := NewCipher(testKey())
	require.NoError(t, err)

	otherKey := testKey()
	otherKey[0] ^= 0xff
	c2, err := NewCipher(otherKey)
	require.NoError(t, err)

	ciphertext, err := c1.Encrypt("secret")
	require.NoError(t, err)

	_, err = c2.Decrypt(ciphertext)
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestDecryptRejectsGarbage(t *testing.T) {
	c, err := NewCipher(testKey())
	require.NoError(t, err)

	_, err = c.Decrypt("not base64 !!!")
	assert.ErrorIs(t, err, ErrDecrypt)

	_, err = c.Decrypt(base64.StdEncoding.EncodeToString([]byte("short")))
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestNewCipherRejectsBadKeyLength(t *testing.T) {
	_, err := NewCipher(make([]byte, 16))
	assert.Error(t, err)
	_, err = NewCipher(nil)
	assert.Error(t, err)
}

func TestGeneratedSecretsArePrefixedAndUnique(t *testing.T) {
	apiKey, err := NewAPIKey()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(apiKey, APIKeyPrefix))

	apiSecret, err := NewAPISecret()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(apiSecret, APISecretPrefix))

	webhookSecret, err := NewWebhookSecret()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(webhookSecret, WebhookPrefix))

	other, err := NewAPIKey()
	require.NoError(t, err)
	assert.NotEqual(t, apiKey, other)
}
