// Small shared helpers.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// --- admin helpers ---

// TokenHash returns the hex sha256 of a raw token (only this is ever persisted/logged).
func TokenHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// MarshalMetadata encodes freeform metadata to the events.metadata column.
func MarshalMetadata(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
