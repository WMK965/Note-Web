package util

import (
	"crypto/sha256"
	"encoding/hex"
)

func CacheKey(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:8])
}
