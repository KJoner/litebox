package node

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 苹果与谷歌系被显式排除:它们的 TLS 参数没问题,问题在于各类教程都用这几个域名,
// 一台 VPS 常年只跟它们握手本身就是特征。这条规则只写在注释里迟早会被"顺手加回来",
// 所以在这里钉死。
var excludedDestSuffixes = []string{
	"apple.com",
	"icloud.com",
	"google.com",
	"googleapis.com",
	"gstatic.com",
}

func TestDefaultDestCandidatesExcludeAppleAndGoogle(t *testing.T) {
	for _, dest := range DefaultDestCandidates {
		host := strings.ToLower(dest)
		for _, suffix := range excludedDestSuffixes {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				t.Errorf("候选握手目标 %s 属于被排除的 %s", dest, suffix)
			}
		}
	}
}

func TestDefaultDestCandidatesAreValidAndUnique(t *testing.T) {
	if len(DefaultDestCandidates) == 0 {
		// 空列表会让 CreateParams 的默认握手目标取到越界下标,建节点直接 panic。
		t.Fatal("候选握手目标不能为空")
	}

	seen := make(map[string]bool, len(DefaultDestCandidates))
	for _, dest := range DefaultDestCandidates {
		if seen[dest] {
			// 重复项会让节点上的扫描白跑一次(每次都要真的建一条 TLS 连接)。
			t.Errorf("候选握手目标 %s 重复", dest)
		}
		seen[dest] = true

		if err := singbox.ValidateHandshakeServer(dest); err != nil {
			t.Errorf("候选握手目标 %s 过不了校验:%v", dest, err)
		}
	}
}
