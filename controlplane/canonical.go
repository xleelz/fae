package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/xleelz/fae/domain"
)

// CanonicalJSON uses the same pure canonicalization as workflow identities.
func CanonicalJSON(raw []byte) ([]byte, error) { return domain.CanonicalJSON(raw) }
func Digest(content []byte) string             { sum := sha256.Sum256(content); return hex.EncodeToString(sum[:]) }
func ContentHash(raw []byte) (string, []byte, error) {
	b, e := CanonicalJSON(raw)
	if e != nil {
		return "", nil, e
	}
	return Digest(b), b, nil
}
