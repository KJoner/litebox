// Package crypto 提供 LiteBox 的密码学基础设施:
//
//   - 主密钥与可恢复字段的对称加密(用户 UUID、节点 REALITY 私钥);
//   - 订阅 Token 的生成与单向哈希(只存哈希,不存明文);
//   - 管理员密码的 argon2id 哈希与校验。
//
// 设计原则:UUID 与节点私钥必须能够还原(要下发给客户端、要写进节点配置),
// 因此使用对称加密而非哈希;订阅 Token 只需验证,因此只存哈希。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MasterKeySize 是主密钥的字节长度,对应 AES-256。
const MasterKeySize = 32

var (
	ErrInvalidMasterKey = errors.New("主密钥非法:必须是 32 字节的 base64 编码")
	ErrDecryptFailed    = errors.New("解密失败:密文被篡改或主密钥不匹配")
	ErrInvalidHash      = errors.New("密码哈希格式非法")
)

// GenerateMasterKey 生成一个新的主密钥,返回 base64 标准编码。
func GenerateMasterKey() (string, error) {
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Cipher 使用主密钥对可恢复字段做 AES-256-GCM 加密。
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher 从 base64 编码的主密钥构造 Cipher。
func NewCipher(masterKeyB64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(masterKeyB64))
	if err != nil {
		return nil, ErrInvalidMasterKey
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("%w(实际 %d 字节)", ErrInvalidMasterKey, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密明文,返回 base64(nonce || 密文 || 认证标签)。
// 每次调用使用新的随机 nonce,因此同一明文的密文不同。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 还原 Encrypt 产生的密文。
func (c *Cipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrDecryptFailed
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", ErrDecryptFailed
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrDecryptFailed
	}
	return string(plaintext), nil
}

// GenerateToken 生成 URL 安全的随机 Token,用于订阅地址。
// byteLen 为随机字节数,建议不少于 24(约 192 位熵)。
func GenerateToken(byteLen int) (string, error) {
	if byteLen < 16 {
		byteLen = 16
	}
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 计算 Token 的 SHA-256 十六进制哈希。
// 订阅 Token 在数据库中只保存该哈希,泄库时无法反推出可用的订阅地址。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// argon2id 参数。按 OWASP 2024 建议取值,在 2C2G 主控上单次校验约数十毫秒。
// 参数写进哈希串,因此日后调参不影响已有密码的校验。
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// HashPassword 用 argon2id 哈希密码,返回自描述的哈希串:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword 校验密码是否与哈希串匹配。使用常数时间比较。
func VerifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, fmt.Errorf("%w:argon2 版本不匹配", ErrInvalidHash)
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
