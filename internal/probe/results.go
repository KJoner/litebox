package probe

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 保留期。明细 7 天够看一周的点阵与 24 小时曲线;更长的趋势读小时汇总。
const (
	RawRetention    = 7 * 24 * time.Hour
	HourlyRetention = 90 * 24 * time.Hour
)

// Sample 是一条落库的结果。
type Sample struct {
	TargetID   int64
	Generation int
	Source     string
	At         time.Time
	Result
}

// TargetUpdate 是一轮结束后要写回目标行的状态。
type TargetUpdate struct {
	ID                  int64
	ResolvedHost        string
	ResolvedPort        int
	Generation          int
	GenerationSince     string
	RefState            string
	RefError            string
	ConsecutiveFailures int
	AlertState          string
	AlertSince          string
	AlertNotified       bool
	RecoveryPending     bool
	LastStatus          string
	LastAt              string
	LastSuccessAt       string
}

// SaveRound 把一轮的结果、小时汇总与目标状态写进同一个事务。
//
// 一轮一个事务:数据库只有一条连接,逐条提交会让每分钟几十次小事务去和部署、
// 流量同步抢那条连接。
func (s *Store) SaveRound(ctx context.Context, samples []Sample, updates []TargetUpdate) error {
	if len(samples) == 0 && len(updates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sm := range samples {
		at := sm.At.UTC().Format(time.RFC3339)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, dns_us, error)
			VALUES (?,?,?,?,?,?,?,?)`,
			sm.TargetID, sm.Generation, sm.Source, at, string(sm.Status),
			sm.RTT.Microseconds(), sm.DNS.Microseconds(), sm.Error); err != nil {
			return err
		}
		var attempts, ok, dns, skipped int
		var rtt int64
		switch {
		case sm.Status == StatusSkipped:
			skipped = 1
		case sm.Status == StatusDNSError:
			dns = 1
		case sm.Status.Attempted():
			attempts = 1
			if sm.Status == StatusOK {
				ok, rtt = 1, sm.RTT.Microseconds()
			}
		}
		hour := sm.At.UTC().Truncate(time.Hour).Format(time.RFC3339)
		// UPSERT 里 SET 右侧引用的是【旧行】的值,所以 rtt_min 要先判断旧行有没有成功样本。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, dns_errors, skipped, rtt_sum_us, rtt_min_us, rtt_max_us)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(target_id, generation, hour) DO UPDATE SET
			  attempts = attempts + excluded.attempts,
			  ok = ok + excluded.ok,
			  dns_errors = dns_errors + excluded.dns_errors,
			  skipped = skipped + excluded.skipped,
			  rtt_sum_us = rtt_sum_us + excluded.rtt_sum_us,
			  rtt_min_us = CASE WHEN excluded.ok = 0 THEN rtt_min_us
			                    WHEN ok = 0 THEN excluded.rtt_min_us
			                    ELSE MIN(rtt_min_us, excluded.rtt_min_us) END,
			  rtt_max_us = MAX(rtt_max_us, excluded.rtt_max_us)`,
			sm.TargetID, sm.Generation, hour, attempts, ok, dns, skipped, rtt, rtt, rtt); err != nil {
			return err
		}
	}
	now := s.now().UTC().Format(time.RFC3339)
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, `
			UPDATE probe_targets SET
			  resolved_host = ?, resolved_port = ?, generation = ?, generation_since = ?,
			  ref_state = ?, ref_error = ?, consecutive_failures = ?,
			  alert_state = ?, alert_since = ?, alert_notified = ?, recovery_pending = ?,
			  last_status = ?, last_at = ?, last_success_at = ?, updated_at = ?
			 WHERE id = ?`,
			u.ResolvedHost, u.ResolvedPort, u.Generation, u.GenerationSince,
			u.RefState, u.RefError, u.ConsecutiveFailures,
			u.AlertState, u.AlertSince, b2i(u.AlertNotified), b2i(u.RecoveryPending),
			u.LastStatus, u.LastAt, u.LastSuccessAt, now, u.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Prune 删掉超出保留期的明细与小时汇总。
func (s *Store) Prune(ctx context.Context, now time.Time) (int64, error) {
	rawCut := now.UTC().Add(-RawRetention).Format(time.RFC3339)
	hourCut := now.UTC().Add(-HourlyRetention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM probe_results WHERE at < ?`, rawCut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `DELETE FROM probe_hourly WHERE hour < ?`, hourCut)
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// ---------- 统计 ----------

// Bucket 是一个时间窗口里的计数。点阵的每一个点、曲线的每一个点都是一个 Bucket,
// 卡片上的失败率由同一批 Bucket 相加得到 —— 分母、样本数与点阵永远对得上。
type Bucket struct {
	At        string   `json:"at"`
	Attempts  int      `json:"attempts"`
	OK        int      `json:"ok"`
	Failures  int      `json:"failures"`
	DNSErrors int      `json:"dns_errors"`
	Skipped   int      `json:"skipped"`
	RTTAvgMs  *float64 `json:"rtt_avg_ms"`
	RTTMinMs  *float64 `json:"rtt_min_ms"`
	RTTMaxMs  *float64 `json:"rtt_max_ms"`
	// State 是点阵的颜色:OK / PARTIAL / FAIL / DNS / SKIP / NONE。
	// NONE = 这个窗口一个样本都没有 —— **不按正常画**。
	State string `json:"state"`

	rttSum, rttMin, rttMax int64
}

// Stats 是一段时间的合计。
type Stats struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Attempts 是实际发起的 TCP 连接数,失败率的分母;DNSErrors 与 Skipped 不在分母里。
	Attempts  int `json:"attempts"`
	OK        int `json:"ok"`
	Failures  int `json:"failures"`
	DNSErrors int `json:"dns_errors"`
	Skipped   int `json:"skipped"`
	// FailureRate 是 Failures / Attempts(0~1);一次都没发起过时为 nil,不是 0。
	FailureRate *float64 `json:"failure_rate"`
	RTTAvgMs    *float64 `json:"rtt_avg_ms"`
	RTTMinMs    *float64 `json:"rtt_min_ms"`
	RTTMaxMs    *float64 `json:"rtt_max_ms"`
}

func (b *Bucket) add(status Status, rttUS int64) {
	switch {
	case status == StatusSkipped:
		b.Skipped++
	case status == StatusDNSError:
		b.DNSErrors++
	case status.Attempted():
		b.Attempts++
		if status == StatusOK {
			b.addOK(1, rttUS, rttUS, rttUS)
		} else {
			b.Failures++
		}
	}
}

func (b *Bucket) addOK(n int, sum, min, max int64) {
	if b.OK == 0 || min < b.rttMin {
		b.rttMin = min
	}
	if max > b.rttMax {
		b.rttMax = max
	}
	b.OK += n
	b.rttSum += sum
}

func ms(us int64) *float64 {
	v := float64(us) / 1000
	return &v
}

func (b *Bucket) finish() {
	if b.OK > 0 {
		b.RTTAvgMs = ms(b.rttSum / int64(b.OK))
		b.RTTMinMs = ms(b.rttMin)
		b.RTTMaxMs = ms(b.rttMax)
	}
	switch {
	case b.Attempts > 0 && b.Failures == 0:
		b.State = "OK"
	case b.Attempts > 0 && b.OK == 0:
		b.State = "FAIL"
	case b.Attempts > 0:
		b.State = "PARTIAL"
	case b.DNSErrors > 0:
		b.State = "DNS"
	case b.Skipped > 0:
		b.State = "SKIP"
	default:
		b.State = "NONE"
	}
}

// Sum 把一批窗口合成一段时间的合计。
func Sum(buckets []Bucket, from, to time.Time) Stats {
	st := Stats{From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339)}
	var acc Bucket
	for _, b := range buckets {
		st.Attempts += b.Attempts
		st.Failures += b.Failures
		st.DNSErrors += b.DNSErrors
		st.Skipped += b.Skipped
		if b.OK > 0 {
			acc.addOK(b.OK, b.rttSum, b.rttMin, b.rttMax)
		}
	}
	st.OK = acc.OK
	if st.Attempts > 0 {
		r := float64(st.Failures) / float64(st.Attempts)
		st.FailureRate = &r
	}
	acc.finish()
	st.RTTAvgMs, st.RTTMinMs, st.RTTMaxMs = acc.RTTAvgMs, acc.RTTMinMs, acc.RTTMaxMs
	return st
}

// rawRow 是从明细表读出来的一行。
type rawRow struct {
	targetID int64
	gen      int
	at       time.Time
	status   Status
	rttUS    int64
	err      string
}

// rawSince 读一批目标从 since 起的明细,只取各自当前口径(generation)的行。
func (s *Store) rawSince(ctx context.Context, targets []Target, since time.Time) (map[int64][]rawRow, error) {
	out := map[int64][]rawRow{}
	if len(targets) == 0 {
		return out, nil
	}
	gen := map[int64]int{}
	ph := make([]string, len(targets))
	args := []any{since.UTC().Format(time.RFC3339)}
	for i, t := range targets {
		gen[t.ID] = t.Generation
		ph[i] = "?"
		args = append(args, t.ID)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, generation, at, status, rtt_us, error
		  FROM probe_results
		 WHERE at >= ? AND target_id IN (`+strings.Join(ph, ",")+`)
		 ORDER BY target_id, at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r rawRow
		var at string
		if err := rows.Scan(&r.targetID, &r.gen, &at, &r.status, &r.rttUS, &r.err); err != nil {
			return nil, err
		}
		if r.gen != gen[r.targetID] {
			continue
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			continue
		}
		r.at = t
		out[r.targetID] = append(out[r.targetID], r)
	}
	return out, rows.Err()
}

// bucketize 把 [from, to) 切成 size 宽的窗口并填入明细。最后一个窗口可以不满。
func bucketize(rows []rawRow, from, to time.Time, size time.Duration) []Bucket {
	n := int(to.Sub(from) / size)
	if to.Sub(from)%size != 0 {
		n++
	}
	if n <= 0 {
		return []Bucket{}
	}
	out := make([]Bucket, n)
	for i := range out {
		out[i].At = from.Add(time.Duration(i) * size).UTC().Format(time.RFC3339)
	}
	for _, r := range rows {
		if r.at.Before(from) || !r.at.Before(to) {
			continue
		}
		out[int(r.at.Sub(from)/size)].add(r.status, r.rttUS)
	}
	for i := range out {
		out[i].finish()
	}
	return out
}

// HourlyBuckets 从小时汇总读 [from, to) 的窗口(7 天视图用)。
func (s *Store) HourlyBuckets(ctx context.Context, t Target, from, to time.Time) ([]Bucket, error) {
	from = from.UTC().Truncate(time.Hour)
	rows, err := s.db.QueryContext(ctx, `
		SELECT hour, attempts, ok, dns_errors, skipped, rtt_sum_us, rtt_min_us, rtt_max_us
		  FROM probe_hourly
		 WHERE target_id = ? AND generation = ? AND hour >= ? AND hour < ?`,
		t.ID, t.Generation, from.Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byHour := map[string]Bucket{}
	for rows.Next() {
		var b Bucket
		var ok int
		var sum, min, max int64
		if err := rows.Scan(&b.At, &b.Attempts, &ok, &b.DNSErrors, &b.Skipped, &sum, &min, &max); err != nil {
			return nil, err
		}
		b.Failures = b.Attempts - ok
		if ok > 0 {
			b.addOK(ok, sum, min, max)
		}
		byHour[b.At] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Bucket
	for h := from; h.Before(to); h = h.Add(time.Hour) {
		key := h.Format(time.RFC3339)
		b, ok := byHour[key]
		if !ok {
			b = Bucket{At: key}
		}
		b.finish()
		out = append(out, b)
	}
	if out == nil {
		out = []Bucket{}
	}
	return out, nil
}

// Range 是历史视图的时间范围。
type Range string

const (
	Range1h  Range = "1h"
	Range24h Range = "24h"
	Range7d  Range = "7d"
)

// ParseRange 校验范围。
func ParseRange(raw string) (Range, error) {
	switch r := Range(raw); r {
	case Range1h, Range24h, Range7d:
		return r, nil
	case "":
		return Range24h, nil
	}
	return "", fmt.Errorf("不认识的时间范围 %q(1h / 24h / 7d)", raw)
}

// History 是一个目标在一段时间里的窗口与合计。
type History struct {
	Target  Target   `json:"target"`
	Range   Range    `json:"range"`
	Bucket  int      `json:"bucket_seconds"`
	Source  string   `json:"source"`
	Buckets []Bucket `json:"buckets"`
	Stats   Stats    `json:"stats"`
	// Failures 是这段时间里最近的若干次失败(失败点),最新的在前。
	Failures []FailurePoint `json:"failures"`
}

// FailurePoint 是一次失败的时间与类别。
type FailurePoint struct {
	At     string `json:"at"`
	Status Status `json:"status"`
	Label  string `json:"label"`
	Error  string `json:"error"`
}

// History 读一个目标的历史。1h / 24h 从明细按 1 / 10 分钟切窗;7d 读小时汇总。
func (s *Store) History(ctx context.Context, t Target, r Range, now time.Time) (*History, error) {
	h := &History{Target: t, Range: r, Source: SourceLabel(t.Source), Failures: []FailurePoint{}}
	var size, span time.Duration
	switch r {
	case Range1h:
		size, span = time.Minute, time.Hour
	case Range24h:
		size, span = 10*time.Minute, 24*time.Hour
	default:
		size, span = time.Hour, 7*24*time.Hour
	}
	to := now.UTC().Truncate(size).Add(size)
	from := to.Add(-span)
	h.Bucket = int(size.Seconds())
	if r == Range7d {
		b, err := s.HourlyBuckets(ctx, t, from, to)
		if err != nil {
			return nil, err
		}
		h.Buckets = b
	} else {
		raw, err := s.rawSince(ctx, []Target{t}, from)
		if err != nil {
			return nil, err
		}
		h.Buckets = bucketize(raw[t.ID], from, to, size)
	}
	h.Stats = Sum(h.Buckets, from, to)
	// 失败点读明细(7 天视图也读 —— 明细正好保留 7 天)。
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, status, error FROM probe_results
		 WHERE target_id = ? AND generation = ? AND at >= ? AND status NOT IN ('OK', 'SKIPPED')
		 ORDER BY at DESC, id DESC LIMIT 50`, t.ID, t.Generation, from.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FailurePoint
		if err := rows.Scan(&f.At, &f.Status, &f.Error); err != nil {
			return nil, err
		}
		f.Label = f.Status.Label()
		h.Failures = append(h.Failures, f)
	}
	return h, rows.Err()
}
