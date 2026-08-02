package crypto

import (
	"strings"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("生成主密钥: %v", err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("构造 Cipher: %v", err)
	}
	return c
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	cases := []string{
		"",
		"419765e4-97f5-457e-a609-2c74d003a83b",
		"6GwqjbluYSk9sHbN6HXwz_xMxXku-sY6ONt7RksOsmw",
		strings.Repeat("长文本", 500),
	}
	for _, plaintext := range cases {
		encrypted, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("加密 %q: %v", plaintext, err)
		}
		decrypted, err := c.Decrypt(encrypted)
		if err != nil {
			t.Fatalf("解密 %q: %v", plaintext, err)
		}
		if decrypted != plaintext {
			t.Errorf("往返不一致:得到 %q,期望 %q", decrypted, plaintext)
		}
	}
}

// 同一明文两次加密必须产生不同密文,否则可以通过比较密文判断
// 两个用户是否使用了相同的 UUID。
func TestEncryptUsesFreshNonce(t *testing.T) {
	c := newTestCipher(t)
	const plaintext = "same-value"
	first, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("两次加密产生了相同密文,nonce 未随机化")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	c := newTestCipher(t)
	encrypted, err := c.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	// 翻转末尾一个字符,破坏 GCM 认证标签。
	tampered := []byte(encrypted)
	if tampered[len(tampered)-2] == 'A' {
		tampered[len(tampered)-2] = 'B'
	} else {
		tampered[len(tampered)-2] = 'A'
	}
	if _, err := c.Decrypt(string(tampered)); err == nil {
		t.Error("被篡改的密文应当解密失败")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	encrypted, err := newTestCipher(t).Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestCipher(t).Decrypt(encrypted); err == nil {
		t.Error("换用其他主密钥应当解密失败")
	}
}

func TestNewCipherRejectsBadKeys(t *testing.T) {
	cases := map[string]string{
		"空字符串":     "",
		"非 base64": "!!!not-base64!!!",
		"长度不足 32":  "c2hvcnQ=",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCipher(key); err == nil {
				t.Errorf("主密钥 %q 应当被拒绝", key)
			}
		})
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("哈希密码: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("哈希串格式不符:%s", hash)
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("校验密码: %v", err)
	}
	if !ok {
		t.Error("正确密码校验失败")
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("校验错误密码: %v", err)
	}
	if ok {
		t.Error("错误密码通过了校验")
	}
}

// 相同密码每次哈希都应有不同的盐,否则可以从哈希值判断两个账号是否同密码。
func TestPasswordHashUsesFreshSalt(t *testing.T) {
	first, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("相同密码产生了相同哈希,盐未随机化")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
	}
	for _, hash := range cases {
		if _, err := VerifyPassword("password", hash); err == nil {
			t.Errorf("非法哈希串 %q 应当报错", hash)
		}
	}
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	a := HashToken("token-a")
	if a != HashToken("token-a") {
		t.Error("同一 Token 的哈希不稳定")
	}
	if a == HashToken("token-b") {
		t.Error("不同 Token 产生了相同哈希")
	}
	if len(a) != 64 {
		t.Errorf("SHA-256 十六进制哈希应为 64 字符,实际 %d", len(a))
	}
}

func TestGenerateTokenEnforcesMinimumLength(t *testing.T) {
	// 传入过小的长度时应被提升到安全下限,避免调用方误配出低熵 Token。
	token, err := GenerateToken(4)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 20 {
		t.Errorf("Token 过短:%q", token)
	}
	other, err := GenerateToken(4)
	if err != nil {
		t.Fatal(err)
	}
	if token == other {
		t.Error("两次生成的 Token 相同")
	}
}
