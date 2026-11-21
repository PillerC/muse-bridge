package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex 返回 n 字节随机数的十六进制表示。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b)
}
