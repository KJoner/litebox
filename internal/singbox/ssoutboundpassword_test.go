package singbox

import (
	"encoding/base64"
	"strings"
	"testing"
)

// CheckOutboundSSPassword 替 sing-box 提前做它启动时才做的那一项检查
// (bad key length)。这里的 32 字节密钥恰好是 GenerateSSKey 产出的形状,
// 16 字节的则是按 aes-128 截过的 —— 两种在机场链接里都常见。
func TestCheckOutboundSSPassword(t *testing.T) {
	key16 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.StdEncoding.EncodeToString(make([]byte, 32))

	good := []struct {
		method   SSMethod
		password string
	}{
		{SSMethodAES128GCM, key16},
		{SSMethodAES128GCM, key16 + ":" + key16},
		{SSMethodAES256GCM, key32},
		{SSMethodChaCha20, key32 + ":" + key32},
		// 传统 AEAD 不查:password 是任意字符串,连空的都由 sing-box 自己决定。
		{"aes-256-gcm", "anything"},
		{"chacha20-ietf-poly1305", ""},
	}
	for _, c := range good {
		if err := CheckOutboundSSPassword(c.method, c.password); err != nil {
			t.Errorf("%s / %q 应通过:%v", c.method, c.password, err)
		}
	}

	bad := []struct {
		method   SSMethod
		password string
		want     string
	}{
		{SSMethodAES256GCM, key16, "需要 32 字节"},
		{SSMethodAES128GCM, key32, "需要 16 字节"},
		{SSMethodAES128GCM, key16 + ":" + key32, "第 2 把"},
		{SSMethodAES128GCM, "not base64!", "不是标准 base64"},
		{SSMethodAES128GCM, "", "为空"},
	}
	for _, c := range bad {
		err := CheckOutboundSSPassword(c.method, c.password)
		if err == nil {
			t.Errorf("%s / %q 应被拒绝", c.method, c.password)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s / %q 的错误应含 %q,得到 %v", c.method, c.password, c.want, err)
		}
	}
}
