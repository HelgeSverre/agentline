package securetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

func New(bytes int) (string, error) {
	if bytes <= 0 {
		return "", fmt.Errorf("token size must be positive")
	}
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func Hash(raw string) [32]byte {
	return sha256.Sum256([]byte(raw))
}
