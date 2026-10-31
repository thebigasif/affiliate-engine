// Package ids generates collision-resistant identifiers.
package ids

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// NewClickID returns a 128-bit CSPRNG identifier, base64url-encoded (22 chars).
// Collision-retry is the caller's responsibility on unique-constraint violation.
func NewClickID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ids: click id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewTokenHex returns n random bytes hex-encoded (2n chars).
func NewTokenHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ids: token: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
