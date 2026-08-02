package node

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// RealityKeyPair 是一对 REALITY X25519 密钥,均为 base64url 无填充编码。
type RealityKeyPair struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// GenerateRealityKeyPair 在主控本地生成 REALITY 密钥对。
//
// 刻意不调用节点上的 `sing-box generate reality-keypair`:私钥若在节点上生成,
// 就会出现在该节点的进程参数与命令输出里,并可能被 shell 历史或日志留存。
// 主控生成后加密入库,只在写配置时下发。
func GenerateRealityKeyPair() (RealityKeyPair, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return RealityKeyPair{}, fmt.Errorf("生成 X25519 密钥: %w", err)
	}
	return RealityKeyPair{
		PrivateKey: base64.RawURLEncoding.EncodeToString(private.Bytes()),
		PublicKey:  base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
	}, nil
}

// DerivePublicKey 从 base64url 私钥推导公钥,用于校验库中两者是否配套。
func DerivePublicKey(privateKeyB64 string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(privateKeyB64)
	if err != nil {
		return "", fmt.Errorf("解码 REALITY 私钥: %w", err)
	}
	private, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", fmt.Errorf("解析 REALITY 私钥: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()), nil
}

// GenerateShortID 生成 REALITY short_id。
// byteLen 为字节数,编码成十六进制后长度翻倍,上限 8 字节(16 个十六进制字符)。
func GenerateShortID(byteLen int) (string, error) {
	if byteLen < 1 || byteLen > 8 {
		byteLen = 8
	}
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// GenerateUUID 生成 RFC 4122 版本 4 的 UUID,小写带连字符。
// 不引入第三方 UUID 库:格式固定且必须与 singbox.ValidateUUID 完全对齐。
func GenerateUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // 版本 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // RFC 4122 变体
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
