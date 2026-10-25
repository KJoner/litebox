package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// fullScanParts 是优化之前的算法:读满 24 小时明细,点阵、1h、24h、最近一条全从明细里算。
// Summaries 改成「整点读小时汇总」之后,这几样必须与它一字不差。
func fullScanParts(t *testing.T, s *Store, targets []Target, sc SummaryContext, now time.Time) map[int64]string {
	t.Helper()
	raw, err := s.rawSince(context.Background(), targets, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]string{}
	for _, tg := range targets {
		rows := raw[tg.ID]
		dot := dotWidth(sc.interval(tg))
		dotTo := now.Truncate(dot).Add(dot)
		part := map[string]any{
			"dots": bucketize(rows, dotTo.Add(-DotCount*dot), dotTo, dot),
			"h1":   sumRows(rows, now.Add(-time.Hour), now),
			"h24":  sumRows(rows, now.Add(-24*time.Hour), now),
		}
		if n := len(rows); n > 0 {
			r := rows[n-1]
			var rtt time.Duration
			if r.res.Status.Reachable() {
				rtt = r.res.RTT
			}
			sent, recv := r.res.counts()
			part["last"] = []any{r.at.Format(time.RFC3339), r.res.Status, rtt, sent, recv, r.res.HTTPStatus, r.res.Error}
		}
		b, _ := json.Marshal(part)
		out[tg.ID] = string(b)
	}
	return out
}

func summaryParts(sums []Summary) map[int64]string {
	out := map[int64]string{}
	for _, sm := range sums {
		part := map[string]any{"dots": sm.Dots, "h1": sm.H1, "h24": sm.H24}
		if l := sm.Last; l != nil {
			var rtt time.Duration
			if l.RTTMs != nil {
				rtt = time.Duration(*l.RTTMs*1000) * time.Microsecond
			}
			part["last"] = []any{l.At, l.Status, rtt, l.Sent, l.Received, l.HTTPStatus, l.Error}
		}
		b, _ := json.Marshal(part)
		out[sm.ID] = string(b)
	}
	return out
}

func TestSummariesMatchFullScan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	nid := e.addNode(t, "hk", "192.0.2.10")
	for i := 0; i < 8; i++ {
		e.exec(t, `INSERT INTO probe_targets (node_id, kind, ref, family, label, generation_since, created_at, updated_at)
			VALUES (?, 'SERVICE', ?, 'V4', 'x', 't', 't', 't')`, nid, fmt.Sprintf("ENTRY:SINGBOX:%d", i))
	}
	targets, err := e.store.ListAll(ctx, DirIngress)
	if err != nil {
		t.Fatal(err)
	}
	// 「现在」不在整点上:24 小时窗口开头有一截不满一小时的零头要读明细。
	now := time.Date(2026, 10, 18, 3, 17, 42, 0, time.UTC)
	statuses := []Status{StatusOK, StatusOK, StatusOK, StatusLoss, StatusTimeout, StatusRefused, StatusDNSError,
		StatusSkipped, StatusSSHError, StatusToolError}
	var samples []Sample
	add := func(tg Target, gen int, at time.Time) {
		st := statuses[rng.Intn(len(statuses))]
		r := Result{Status: st}
		if st.Reachable() {
			r.RTT = time.Duration(10000+rng.Intn(90000)) * time.Microsecond
			r.RTTMin = r.RTT - time.Duration(rng.Intn(5000))*time.Microsecond
			r.RTTMax = r.RTT + time.Duration(rng.Intn(5000))*time.Microsecond
			if st == StatusLoss {
				r.Sent, r.Received = 5, 1+rng.Intn(4)
			} else if rng.Intn(2) == 0 {
				r.Sent, r.Received = 5, 5
			}
			if rng.Intn(3) == 0 {
				r.HTTPStatus = []int{200, 302, 404, 503}[rng.Intn(4)]
			}
		} else if st == StatusTimeout {
			r.Error = "连接超时"
		}
		samples = append(samples, Sample{TargetID: tg.ID, Generation: gen, Source: "MASTER", At: at, Result: r})
	}
	for i, tg := range targets {
		switch i {
		case 0: // 正常:每分钟一条,整整 30 小时
			for m := 0; m < 30*60; m++ {
				add(tg, 1, now.Add(-time.Duration(m)*time.Minute-time.Duration(rng.Intn(50))*time.Second))
			}
		case 1: // 5 小时前停了:最近一条落在用小时汇总代替的整点里
			for m := 5 * 60; m < 26*60; m++ {
				add(tg, 1, now.Add(-time.Duration(m)*time.Minute))
			}
		case 2: // 只有 24 小时窗口开头那一截零头(以及更早的)
			for m := 23*60 + 50; m < 25*60; m++ {
				add(tg, 1, now.Add(-time.Duration(m)*time.Minute))
			}
		case 3: // 换过代:旧口径的行无论在明细还是小时汇总里都不能算进来
			for m := 0; m < 24*60; m++ {
				gen := 2
				if m > 10*60 {
					gen = 1
				}
				add(tg, gen, now.Add(-time.Duration(m)*time.Minute))
			}
		case 4: // 稀疏:几小时一条
			for h := 0; h < 30; h += 3 {
				add(tg, 1, now.Add(-time.Duration(h)*time.Hour-17*time.Minute))
			}
		case 5: // 恰好落在边界上:24 小时窗口起点、整点、now、now 之后一秒
			for _, at := range []time.Time{now.Add(-24 * time.Hour), now.Add(-24 * time.Hour).Truncate(time.Hour).Add(time.Hour),
				now.Truncate(time.Hour), now, now.Add(time.Second), now.Add(-time.Hour)} {
				add(tg, 1, at)
			}
		case 6: // 同一秒好几条
			for m := 0; m < 3*60; m++ {
				at := now.Add(-time.Duration(m) * time.Minute)
				add(tg, 1, at)
				add(tg, 1, at)
			}
		case 7: // 一条都没有
		}
	}
	if err := e.store.SaveRound(ctx, samples, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE probe_targets SET generation = 2 WHERE id = ?`, targets[3].ID); err != nil {
		t.Fatal(err)
	}
	targets, _ = e.store.ListAll(ctx, DirIngress)

	for _, tc := range []struct {
		name     string
		interval time.Duration
		now      time.Time
	}{
		{"间隔一分钟", time.Minute, now},
		{"间隔五分钟", 5 * time.Minute, now},
		{"间隔一小时(点阵窗口超过一天,全部读明细)", time.Hour, now},
		{"现在恰好是整点", time.Minute, now.Truncate(time.Hour)},
		{"现在在整点后一秒", time.Minute, now.Truncate(time.Hour).Add(time.Second)},
		{"一天以后(全都过了 24 小时)", time.Minute, now.Add(26 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := e.cfg
			cfg.Interval = tc.interval
			sc := SummaryContext{Config: cfg}
			want := fullScanParts(t, e.store, targets, sc, tc.now)
			sums, err := e.store.Summaries(ctx, targets, sc, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			got := summaryParts(sums)
			for _, tg := range targets {
				if got[tg.ID] != want[tg.ID] {
					t.Errorf("目标 %d:\n得到 %s\n应为 %s", tg.ID, got[tg.ID], want[tg.ID])
				}
			}
		})
	}
}

func TestSummaryCutReadsLessRaw(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 17, 42, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	sc := SummaryContext{Config: Config{Interval: time.Minute}}
	edge, cut := summaryCut([]Target{{ID: 1}}, sc, since, now)
	if want := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC); !edge.Equal(want) {
		t.Fatalf("edge = %v,应为 %v", edge, want)
	}
	// 间隔一分钟:点阵窗口 30 × 2 分钟 = 1 小时,与近 1 小时一起,明细只从前一个整点读起。
	if want := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC); !cut.Equal(want) {
		t.Fatalf("cut = %v,应为 %v", cut, want)
	}
	sc.Config.Interval = time.Hour
	if edge, cut = summaryCut([]Target{{ID: 1}}, sc, since, now); !edge.Equal(since) || !cut.Equal(since) {
		t.Fatalf("间隔一小时时应全部读明细,得到 edge=%v cut=%v", edge, cut)
	}
}
