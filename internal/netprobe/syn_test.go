package netprobe

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// fakeSyn 按顺序回答每一次握手尝试。
type fakeSyn struct {
	answers []SynAttempt
	calls   int
}

func (f *fakeSyn) Attempt(context.Context, netip.Addr, int, time.Duration) SynAttempt {
	a := f.answers[f.calls%len(f.answers)]
	f.calls++
	return a
}

func (f *fakeSyn) Impl() string { return "fake" }

func synTarget(count int) Target {
	return Target{ID: 1, Protocol: ProtoTCP, TCPMode: ModeSyn, Host: "198.51.100.1", Port: 443, Family: FamilyV4,
		Count: count, IntervalMs: MinIntervalMs, TimeoutMs: 300, AllowRestricted: true}
}

func runSyn(t *testing.T, answers []SynAttempt, count int) Result {
	t.Helper()
	r := &Runner{Syn: &fakeSyn{answers: answers}}
	resp := r.RunBatch(context.Background(), Request{Version: Version, Targets: []Target{synTarget(count)}})
	if resp.SynImpl != "fake" {
		t.Errorf("响应里应写明握手近似的实现: %q", resp.SynImpl)
	}
	return resp.Results[0]
}

func TestSynRoundAccounting(t *testing.T) {
	ack := SynAttempt{Outcome: SynAck, RTT: 30 * time.Millisecond}
	none := SynAttempt{Outcome: SynNone}
	rst := SynAttempt{Outcome: SynRST, RTT: 31 * time.Millisecond}
	cases := []struct {
		name                      string
		answers                   []SynAttempt
		status                    string
		sent, recv, refused, rttN int
		retrans                   int
		wantRTT                   bool
	}{
		{"全部应答", []SynAttempt{ack}, StatusOK, 4, 4, 0, 0, 0, true},
		{"一半无应答", []SynAttempt{ack, none}, StatusLoss, 4, 2, 0, 0, 0, true},
		{"全部无应答", []SynAttempt{none}, StatusTimeout, 4, 0, 0, 0, 0, false},
		// RST 是应答(不进无应答率),但全是 RST 时这一轮是「端口拒绝」。
		{"全部 RST", []SynAttempt{rst}, StatusRefused, 4, 4, 4, 0, 0, true},
		{"RST 与 SYN-ACK 混合", []SynAttempt{ack, rst}, StatusOK, 4, 4, 2, 0, 0, true},
		// 内核重发了一次:发出的 SYN 多一个、那一次的 RTT 不取。
		{"内核重发", []SynAttempt{{Outcome: SynAck, RTT: 1100 * time.Millisecond, Retrans: 1}, ack, ack, ack},
			StatusLoss, 5, 4, 0, 3, 1, true},
		{"每次都重发", []SynAttempt{{Outcome: SynAck, RTT: 1100 * time.Millisecond, Retrans: 1}},
			StatusLoss, 8, 4, 0, 0, 4, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runSyn(t, c.answers, 4)
			if res.Status != c.status || res.Sent != c.sent || res.Received != c.recv || res.Refused != c.refused ||
				res.Retrans != c.retrans {
				t.Errorf("得到 %+v", res)
			}
			if (res.RTTUs > 0) != c.wantRTT {
				t.Errorf("RTT 样本: %+v", res)
			}
			if res.RTTN != c.rttN {
				t.Errorf("RTT 样本数 %d,期望 %d(0 = 等于收到数)", res.RTTN, c.rttN)
			}
		})
	}
}

// 发不出去(没有路由):这一轮剩下的不再尝试,按「打算发的全没有应答」记,与 ICMP 发送失败同一口径。
func TestSynRoundUnreachable(t *testing.T) {
	f := &fakeSyn{answers: []SynAttempt{{Outcome: SynUnreach}}}
	r := &Runner{Syn: f}
	resp := r.RunBatch(context.Background(), Request{Version: Version, Targets: []Target{synTarget(4)}})
	res := resp.Results[0]
	if res.Status != StatusUnreachable || res.Sent != 4 || res.Received != 0 || f.calls != 1 {
		t.Errorf("得到 %+v,尝试了 %d 次", res, f.calls)
	}
}

// 面板回滚到 v1 时,v1 的请求在新辅助程序上照样能跑:按 v1 回话,TCP 方式一律当建连。
func TestHelperAnswersV1Requests(t *testing.T) {
	f := &fakeSyn{answers: []SynAttempt{{Outcome: SynAck, RTT: time.Millisecond}}}
	r := &Runner{Syn: f, Dialer: fakeDialer{"198.51.100.1:443": refused()}}
	tg := synTarget(4)
	resp := r.RunBatch(context.Background(), Request{Version: 1, Targets: []Target{tg}})
	if resp.Version != 1 {
		t.Errorf("v1 请求应按 v1 回话: %d", resp.Version)
	}
	if f.calls != 0 || resp.Results[0].Sent != 1 {
		t.Errorf("v1 请求里不该出现握手近似: calls=%d %+v", f.calls, resp.Results[0])
	}
	if got := r.RunBatch(context.Background(), Request{Version: 2, Targets: []Target{tg}}); got.Version != 2 || f.calls == 0 {
		t.Errorf("v2 请求: %+v", got)
	}
}

func TestSynNormalizeAndBudget(t *testing.T) {
	tg := Target{Protocol: ProtoTCP, TCPMode: ModeSyn, Host: "example.com", Port: 80, Family: FamilyV4, TimeoutMs: 3000, Count: 50}
	if err := Normalize(&tg); err != nil {
		t.Fatal(err)
	}
	if tg.TimeoutMs != SynCutoffMs || tg.Count != MaxCount || tg.IntervalMs == 0 {
		t.Errorf("握手近似的参数应被夹住: %+v", tg)
	}
	// DNS 900 + 10 次 × max(900, 500) = 9.9 秒。
	if got := Budget([]Target{tg}); got != 9900*time.Millisecond {
		t.Errorf("预算: %v", got)
	}
	bad := Target{Protocol: ProtoTCP, TCPMode: "XMAS", Host: "example.com", Port: 80, Family: FamilyV4}
	if err := Normalize(&bad); err == nil {
		t.Error("不认识的 TCP 方式应报错")
	}
}
