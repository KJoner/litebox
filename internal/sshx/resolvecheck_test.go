package sshx

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 只在显式指定 LITEBOX_RESOLVE_CHECK 时运行:它打真实 DNS,
// 结果取决于跑测试的这台机器,不能当作断言。
func TestResolveCheckManual(t *testing.T) {
	hosts := os.Getenv("LITEBOX_RESOLVE_CHECK")
	if hosts == "" {
		t.Skip("设 LITEBOX_RESOLVE_CHECK=host1,host2 才运行")
	}
	for _, h := range strings.Split(hosts, ",") {
		ips, err := ResolveHost(t.Context(), h, 5*time.Second)
		if err != nil {
			t.Logf("%-34s 失败: %v", h, err)
			continue
		}
		t.Logf("%-34s → %v", h, ips)
	}
}
