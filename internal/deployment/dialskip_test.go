package deployment

import (
	"context"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 管理员在确认框里关掉拨测:优先于「没有用户」「缺参数」两种跳过,说明里要写明是管理员关的,
// 而且不能被读成验过了 —— 服务状态与端口监听那两步不受影响(它们在这个判断之前)。
func TestDialSkipReasonHonoursAdminChoice(t *testing.T) {
	in := singbox.InboundParams{Tag: "in-1", Users: []singbox.User{{Code: "user_000001"}}}
	req := Request{Probes: []ProbeTarget{{Tag: "in-1"}}}

	if _, reason := dialSkipReason(req, in); reason != "" {
		t.Fatalf("默认要拨测,却跳过了:%s", reason)
	}

	req.SkipDial = true
	_, reason := dialSkipReason(req, in)
	if reason != DialSkippedByAdmin {
		t.Fatalf("管理员关掉拨测时说明 = %q", reason)
	}
	if !strings.Contains(reason, "没有验证") {
		t.Fatalf("说明要讲清面板没有验证过入口:%q", reason)
	}

	// 没有用户、缺参数时同样是管理员那一句 —— 他关掉的是整步拨测,不该再提示去补用户。
	if _, r := dialSkipReason(req, singbox.InboundParams{Tag: "in-2"}); r != DialSkippedByAdmin {
		t.Fatalf("没有用户时 = %q", r)
	}

	// 没关拨测时原来那两种跳过不变。
	req.SkipDial = false
	if _, r := dialSkipReason(req, singbox.InboundParams{Tag: "in-1"}); !strings.Contains(r, "没有用户") {
		t.Fatalf("没有用户 = %q", r)
	}
	if _, r := dialSkipReason(req, singbox.InboundParams{Tag: "in-9", Users: in.Users}); !strings.Contains(r, "拨测参数") {
		t.Fatalf("缺参数 = %q", r)
	}
}

// Mieru:出口那一跳也是一次拨测,关掉时两步都记 SKIPPED;没关时一步都不记(由后面真的去拨)。
func TestSkipMieruDialsRecordsBothHops(t *testing.T) {
	rec := &stepRecorder{ctx: context.Background()}
	if skipMieruDials(rec, MieruRequest{EgressSocksPort: 30001}) || len(rec.steps) != 0 {
		t.Fatalf("没关拨测却跳过了:%+v", rec.steps)
	}

	if !skipMieruDials(rec, MieruRequest{EgressSocksPort: 30001, SkipDial: true}) {
		t.Fatal("关掉拨测时应当跳过")
	}
	if len(rec.steps) != 2 || rec.steps[0].Name != mieruEgressStep || rec.steps[1].Name != "真实拨测" {
		t.Fatalf("步骤 = %+v", rec.steps)
	}
	for _, s := range rec.steps {
		if s.Status != StepSkipped || s.Detail != DialSkippedByAdmin {
			t.Fatalf("应当记 SKIPPED 并写明是管理员关的:%+v", s)
		}
	}

	// 直连入口没有出口那一跳。
	rec = &stepRecorder{ctx: context.Background()}
	skipMieruDials(rec, MieruRequest{SkipDial: true})
	if len(rec.steps) != 1 || rec.steps[0].Name != "真实拨测" {
		t.Fatalf("直连入口的步骤 = %+v", rec.steps)
	}
}
