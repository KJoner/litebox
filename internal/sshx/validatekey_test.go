package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestValidatePrivateKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	good := string(pem.EncodeToMemory(block))

	// 带口令的私钥:面板无人值守,输不了口令,必须拒。
	encBlock, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	withPass := string(pem.EncodeToMemory(encBlock))

	pub := string(ssh.MarshalAuthorizedKey(func() ssh.PublicKey {
		s, _ := ssh.ParsePrivateKey([]byte(good))
		return s.PublicKey()
	}()))

	cases := []struct {
		name    string
		in      string
		wantErr bool
		wantSub string
	}{
		{"合法私钥", good, false, ""},
		{"空串表示用面板密钥", "", false, ""},
		{"只有空白", "   \n  ", false, ""},
		{"误贴公钥", pub, true, "合法的 SSH 私钥"},
		{"贴成一行", strings.ReplaceAll(strings.TrimSpace(good), "\n", " "), true, "合法的 SSH 私钥"},
		{"被截断", strings.TrimSuffix(good, "-----END OPENSSH PRIVATE KEY-----\n"), true, "合法的 SSH 私钥"},
		{"带口令的私钥", withPass, true, "带口令"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePrivateKey(c.in)
			if c.wantErr && err == nil {
				t.Fatalf("期望被拒绝,却通过了")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望通过,却被拒绝:%v", err)
			}
			if c.wantSub != "" && (err == nil || !strings.Contains(err.Error(), c.wantSub)) {
				t.Errorf("错误信息里没有 %q:%v", c.wantSub, err)
			}
		})
	}
}
