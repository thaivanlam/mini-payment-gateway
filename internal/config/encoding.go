package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// decodeHexOrBase64 decodes a key that may be written either way, so a
// developer can paste the output of `openssl rand -hex 32` or
// `openssl rand -base64 32` without thinking about it.
func decodeHexOrBase64(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("value is neither valid hex nor base64")
}
