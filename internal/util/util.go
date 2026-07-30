// 通用工具包
package util

import (
	"crypto/sha256"
	"encoding/hex"
)

// CacheKey 根据PDF文件的绝对路径生成缓存键
// 使用 SHA256 哈希的前8字节（16个十六进制字符）
// 同一文件路径始终生成相同的键，即使数据库重建也不会冲突
func CacheKey(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:8])
}
