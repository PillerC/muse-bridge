// Package secret 负责三件事：加密机器人 token、派生并校验桥令牌、生成随机串。
//
// 这里刻意不引入密码学之外的依赖：机器人 token 用 AES-256-GCM 做认证加密，
// 桥令牌只存 SHA-256 摘要（令牌本身是高熵随机串，不需要慢哈希），
// 主密钥只从环境变量或 0600 的文件读取。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Box 持有 32 字节主密钥，用于加解密机器人 token。
type Box struct {
	aead cipher.AEAD
}

// NewBox 用 32 字节密钥构造。
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥必须 32 字节，当前 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal 加密明文，输出 base64(nonce||ciphertext)。
func (b *Box) Seal(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open 解密 Seal 的输出。
func (b *Box) Open(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文长度不足")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（主密钥可能已更换）: %w", err)
	}
	return string(plain), nil
}

// NewToken 生成形如 "mb_<24字节hex>" 的桥令牌。
// 前缀让运维一眼能从日志里认出这是什么，也方便用正则扫描泄露。
func NewToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return "mb_" + hex.EncodeToString(buf)
}

// HashToken 返回令牌的存储形态。令牌本身不落库。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// TokenPrefix 返回用于在后台列表里辨认令牌的前 10 个字符。
func TokenPrefix(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:10]
}

// RandomHex 生成 n 字节随机数的十六进制串。
func RandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// ---------- 管理员密码 ----------

const pbkdf2Iter = 210000

// HashPassword 用 PBKDF2-HMAC-SHA256 派生密码摘要。
// 格式：pbkdf2_sha256$<iter>$<salt-hex>$<hash-hex>，自带参数便于以后提高强度。
func HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	sum := pbkdf2([]byte(password), salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", pbkdf2Iter, hex.EncodeToString(salt), hex.EncodeToString(sum))
}

// VerifyPassword 校验密码，用常数时间比较。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	h := sha256.New
	hashLen := h().Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		mac := hmac.New(h, password)
		mac.Write(salt)
		mac.Write(buf)
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac = hmac.New(h, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
