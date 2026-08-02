package user

import (
	"crypto/rand"
	"fmt"
)

// GenerateUUID 生成 RFC 4122 版本 4 的 UUID,小写带连字符。
//
// 格式必须与 singbox.ValidateUUID 完全对齐:sing-box 会把任何字符串
// 哈希成可用的 UUID 而不报错,校验只能靠面板自己,
// 因此生成器与校验器必须来自同一套约定。
func GenerateUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // 版本 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // RFC 4122 变体
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
