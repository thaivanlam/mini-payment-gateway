package idempotency

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyIsScopedToMerchant(t *testing.T) {
	a := Key("merchant-a", "key-1")
	b := Key("merchant-b", "key-1")

	assert.Equal(t, "idem:merchant-a:key-1", a)
	assert.NotEqual(t, a, b, "the same key from two merchants must not collide")
}

func TestFingerprintIsStableAcrossFormatting(t *testing.T) {
	// Same request, different serialisation: key order and whitespace vary
	// between HTTP clients, and must not be read as a different request.
	compact := []byte(`{"amount":150000,"currency":"VND","reference":"ORDER-1"}`)
	reordered := []byte(`{"reference":"ORDER-1","currency":"VND","amount":150000}`)
	spaced := []byte("{\n  \"amount\": 150000,\n  \"currency\": \"VND\",\n  \"reference\": \"ORDER-1\"\n}")

	want := Fingerprint(compact)
	assert.Equal(t, want, Fingerprint(reordered))
	assert.Equal(t, want, Fingerprint(spaced))
}

func TestFingerprintDetectsRealDifferences(t *testing.T) {
	base := []byte(`{"amount":150000,"currency":"VND","reference":"ORDER-1"}`)

	assert.NotEqual(t, Fingerprint(base), Fingerprint([]byte(`{"amount":150001,"currency":"VND","reference":"ORDER-1"}`)),
		"a different amount is a different request")
	assert.NotEqual(t, Fingerprint(base), Fingerprint([]byte(`{"amount":150000,"currency":"USD","reference":"ORDER-1"}`)),
		"a different currency is a different request")
	assert.NotEqual(t, Fingerprint(base), Fingerprint([]byte(`{"amount":150000,"currency":"VND","reference":"ORDER-2"}`)))
}

func TestFingerprintHandlesNestedAndArrayValues(t *testing.T) {
	a := []byte(`{"card":{"number":"4242","exp_month":12},"items":[{"b":2,"a":1}]}`)
	b := []byte(`{"items":[{"a":1,"b":2}],"card":{"exp_month":12,"number":"4242"}}`)
	assert.Equal(t, Fingerprint(a), Fingerprint(b))

	// Array order carries meaning and must not be normalised away.
	c := []byte(`{"items":[{"a":1},{"a":2}]}`)
	d := []byte(`{"items":[{"a":2},{"a":1}]}`)
	assert.NotEqual(t, Fingerprint(c), Fingerprint(d))
}

func TestFingerprintHandlesNonJSON(t *testing.T) {
	assert.Equal(t, Fingerprint([]byte("not json")), Fingerprint([]byte("not json")))
	assert.NotEqual(t, Fingerprint([]byte("not json")), Fingerprint([]byte("also not json")))
	assert.NotEmpty(t, Fingerprint(nil), "an empty body still has a fingerprint")
}
