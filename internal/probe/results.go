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

// hourCounts 是一条结果在小时汇总里占的份额。
type hourCounts struct {
	attempts, ok, lossy, dns, skipped, ssh, tool, blocked int
	vantage, api, expired                                 int
	sent, received, rttN                                  int
	rttSum, rttMin, rttMax                                int64
}

func countsOf(r Result) hourCounts {
	var c hourCounts
	switch {
	case r.Status == StatusSkipped:
		c.skipped = 1
	case r.Status == StatusDNSError:
		c.dns = 1
	case r.Status == StatusSSHError:
		c.ssh = 1
	case r.Status == StatusToolError:
		c.tool = 1
	case r.Status == StatusBlocked:
		c.blocked = 1
	case r.Status == StatusVantageError:
		c.vantage = 1
	case r.Status == StatusAPIError:
		c.api = 1
	case r.Status == StatusExpired:
		c.expired = 1
	case r.Status.Attempted():
		c.attempts = 1
		switch r.Status {
		case StatusOK:
			c.ok = 1
		case StatusLoss:
			c.lossy = 1
		}
		c.sent, c.received = r.counts()
		c.rttN, c.rttSum, c.rttMin, c.rttMax = r.rttStats()
	}
	return c
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
	if err := s.saveRoundTx(ctx, tx, samples, updates); err != nil {
		return err
	}
	return tx.Commit()
}

// saveRoundTx 是 SaveRound 的事务内版本:Boce 的结果要与去重键、执行记录写进同一个事务。
func (s *Store) saveRoundTx(ctx context.Context, tx querier, samples []Sample, updates []TargetUpdate) error {
	for _, sm := range samples {
		at := sm.At.UTC().Format(time.RFC3339)
		c := countsOf(sm.Result)
		var minUS, maxUS int64
		if c.rttN > 0 {
			minUS, maxUS = c.rttMin, c.rttMax
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, dns_us, error,
			                           sent, received, rtt_min_us, rtt_max_us, peer, rtt_n)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sm.TargetID, sm.Generation, sm.Source, at, string(sm.Status),
			sm.RTT.Microseconds(), sm.DNS.Microseconds(), sm.Error,
			c.sent, c.received, minUS, maxUS, sm.Peer, storedRTTN(c)); err != nil {
			return err
		}
		hour := sm.At.UTC().Truncate(time.Hour).Format(time.RFC3339)
		// UPSERT 里 SET 右侧引用的是【旧行】的值,所以 rtt_min 要先判断旧行有没有 RTT 样本。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, lossy, dns_errors, skipped,
			                          ssh_errors, tool_errors, blocked, sent, received, rtt_n,
			                          rtt_sum_us, rtt_min_us, rtt_max_us, vantage_errors, api_errors, expired)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(target_id, generation, hour) DO UPDATE SET
			  attempts = attempts + excluded.attempts,
			  ok = ok + excluded.ok,
			  lossy = lossy + excluded.lossy,
			  dns_errors = dns_errors + excluded.dns_errors,
			  skipped = skipped + excluded.skipped,
			  ssh_errors = ssh_errors + excluded.ssh_errors,
			  tool_errors = tool_errors + excluded.tool_errors,
			  blocked = blocked + excluded.blocked,
			  vantage_errors = vantage_errors + excluded.vantage_errors,
			  api_errors = api_errors + excluded.api_errors,
			  expired = expired + excluded.expired,
			  sent = sent + excluded.sent,
			  received = received + excluded.received,
			  rtt_n = rtt_n + excluded.rtt_n,
			  rtt_sum_us = rtt_sum_us + excluded.rtt_sum_us,
			  rtt_min_us = CASE WHEN excluded.rtt_n = 0 THEN rtt_min_us
			                    WHEN rtt_n = 0 THEN excluded.rtt_min_us
			                    ELSE MIN(rtt_min_us, excluded.rtt_min_us) END,
			  rtt_max_us = MAX(rtt_max_us, excluded.rtt_max_us)`,
			sm.TargetID, sm.Generation, hour, c.attempts, c.ok, c.lossy, c.dns, c.skipped,
			c.ssh, c.tool, c.blocked, c.sent, c.received, c.rttN, c.rttSum, minUS, maxUS,
			c.vantage, c.api, c.expired); err != nil {
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
	return nil
}

// storedRTTN 是明细里记的 RTT 样本数:等于收到数时记 0(与升级前的明细同一个约定)。
func storedRTTN(c hourCounts) int {
	if c.rttN == c.received {
		return 0
	}
	return c.rttN
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
// 卡片上的丢包率 / 失败率由同一批 Bucket 相加得到 —— 分母、样本数与点阵永远对得上。
type Bucket struct {
	At string `json:"at"`
	// Attempts / OK / Lossy / Failures 按轮计:发起了几轮、几轮全部收到、几轮部分丢包、几轮全失败。
	Attempts int `json:"attempts"`
	OK       int `json:"ok"`
	Lossy    int `json:"lossy"`
	Failures int `json:"failures"`
	// 没有发起的几种,各自单列,都不进分母。
	DNSErrors  int `json:"dns_errors"`
	Skipped    int `json:"skipped"`
	SSHErrors  int `json:"ssh_errors"`
	ToolErrors int `json:"tool_errors"`
	Blocked    int `json:"blocked"`
	// Boce 序列独有的三种:拨测点异常、接口异常、任务未取得结果。同样不进分母。
	VantageErrors int `json:"vantage_errors"`
	APIErrors     int `json:"api_errors"`
	Expired       int `json:"expired"`
	// Sent / Received 按包计(TCP 建连按建连计,TCP 握手近似按 SYN 计)。
	// LossRate = (Sent − Received) / Sent;一次都没发起时为 nil。它在三种口径下分别是
	// ICMP 丢包率 / TCP 连接失败率 / TCP 握手超时无应答率,名字由序列的 metric 决定。
	Sent     int      `json:"sent"`
	Received int      `json:"received"`
	LossRate *float64 `json:"loss_rate"`
	RTTAvgMs *float64 `json:"rtt_avg_ms"`
	RTTMinMs *float64 `json:"rtt_min_ms"`
	RTTMaxMs *float64 `json:"rtt_max_ms"`
	// State 是点阵的颜色:OK / PARTIAL / FAIL / DNS / SSH / TOOL / BLOCKED / VANTAGE / API / EXPIRED / SKIP / NONE。
	// NONE = 这个窗口一个样本都没有 —— **不按正常画**。SSH(采集失败)与 TOOL(工具不可用)
	// 也不按失败画:那几轮根本没有发出去,目标那头是什么样子并不知道。
	State string `json:"state"`

	rttN                   int
	rttSum, rttMin, rttMax int64
}

// Stats 是一段时间的合计。
type Stats struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Attempts 是实际发起的轮数(TCP 即建连数),失败率的分母;没有发起的几种都不在分母里。
	Attempts   int `json:"attempts"`
	OK         int `json:"ok"`
	Lossy      int `json:"lossy"`
	Failures   int `json:"failures"`
	DNSErrors  int `json:"dns_errors"`
	Skipped    int `json:"skipped"`
	SSHErrors  int `json:"ssh_errors"`
	ToolErrors int `json:"tool_errors"`
	Blocked    int `json:"blocked"`
	// Boce 序列独有的三种,不进分母。
	VantageErrors int `json:"vantage_errors"`
	APIErrors     int `json:"api_errors"`
	Expired       int `json:"expired"`
	Sent          int `json:"sent"`
	Received      int `json:"received"`
	// FailureRate 是 Failures / Attempts(0~1):TCP 建连的连接失败率;ICMP 与 TCP 握手近似时
	// 是「整轮失败」的比例。LossRate 是 (Sent − Received) / Sent:ICMP 的丢包率、TCP 握手近似的
	// 握手超时无应答率;TCP 建连时与 FailureRate 相同。一次都没发起过时两者都是 nil,不是 0。
	FailureRate *float64 `json:"failure_rate"`
	LossRate    *float64 `json:"loss_rate"`
	RTTAvgMs    *float64 `json:"rtt_avg_ms"`
	RTTMinMs    *float64 `json:"rtt_min_ms"`
	RTTMaxMs    *float64 `json:"rtt_max_ms"`
}

func (b *Bucket) addCounts(c hourCounts) {
	b.Attempts += c.attempts
	b.OK += c.ok
	b.Lossy += c.lossy
	b.Failures += c.attempts - c.ok - c.lossy
	b.DNSErrors += c.dns
	b.Skipped += c.skipped
	b.SSHErrors += c.ssh
	b.ToolErrors += c.tool
	b.Blocked += c.blocked
	b.VantageErrors += c.vantage
	b.APIErrors += c.api
	b.Expired += c.expired
	b.Sent += c.sent
	b.Received += c.received
	if c.rttN > 0 {
		b.addRTT(c.rttN, c.rttSum, c.rttMin, c.rttMax)
	}
}

func (b *Bucket) addRTT(n int, sum, min, max int64) {
	if b.rttN == 0 || min < b.rttMin {
		b.rttMin = min
	}
	if max > b.rttMax {
		b.rttMax = max
	}
	b.rttN += n
	b.rttSum += sum
}

func ms(us int64) *float64 {
	v := float64(us) / 1000
	return &v
}

func ratio(num, den int) *float64 {
	if den <= 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}

func (b *Bucket) finish() {
	if b.rttN > 0 {
		b.RTTAvgMs = ms(b.rttSum / int64(b.rttN))
		b.RTTMinMs = ms(b.rttMin)
		b.RTTMaxMs = ms(b.rttMax)
	}
	b.LossRate = ratio(b.Sent-b.Received, b.Sent)
	switch {
	case b.Attempts > 0 && b.Failures == 0 && b.Lossy == 0:
		b.State = "OK"
	case b.Attempts > 0 && b.OK == 0 && b.Lossy == 0:
		b.State = "FAIL"
	case b.Attempts > 0:
		b.State = "PARTIAL"
	case b.DNSErrors > 0:
		b.State = "DNS"
	case b.SSHErrors > 0:
		b.State = "SSH"
	case b.ToolErrors > 0:
		b.State = "TOOL"
	case b.Blocked > 0:
		b.State = "BLOCKED"
	case b.VantageErrors > 0:
		b.State = "VANTAGE"
	case b.APIErrors > 0:
		b.State = "API"
	case b.Expired > 0:
		b.State = "EXPIRED"
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
		st.OK += b.OK
		st.Lossy += b.Lossy
		st.Failures += b.Failures
		st.DNSErrors += b.DNSErrors
		st.Skipped += b.Skipped
		st.SSHErrors += b.SSHErrors
		st.ToolErrors += b.ToolErrors
		st.Blocked += b.Blocked
		st.VantageErrors += b.VantageErrors
		st.APIErrors += b.APIErrors
		st.Expired += b.Expired
		st.Sent += b.Sent
		st.Received += b.Received
		if b.rttN > 0 {
			acc.addRTT(b.rttN, b.rttSum, b.rttMin, b.rttMax)
		}
	}
	st.FailureRate = ratio(st.Failures, st.Attempts)
	st.LossRate = ratio(st.Sent-st.Received, st.Sent)
	acc.finish()
	st.RTTAvgMs, st.RTTMinMs, st.RTTMaxMs = acc.RTTAvgMs, acc.RTTMinMs, acc.RTTMaxMs
	return st
}

// rawRow 是从明细表读出来的一行。
type rawRow struct {
	targetID int64
	gen      int
	at       time.Time
	res      Result
}

func (r rawRow) status() Status { return r.res.Status }

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
		SELECT target_id, generation, at, status, rtt_us, error, sent, received, rtt_min_us, rtt_max_us, peer, rtt_n
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
		var rtt, minUS, maxUS int64
		if err := rows.Scan(&r.targetID, &r.gen, &at, &r.res.Status, &rtt, &r.res.Error,
			&r.res.Sent, &r.res.Received, &minUS, &maxUS, &r.res.Peer, &r.res.RTTN); err != nil {
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
		r.res.RTT = time.Duration(rtt) * time.Microsecond
		r.res.RTTMin = time.Duration(minUS) * time.Microsecond
		r.res.RTTMax = time.Duration(maxUS) * time.Microsecond
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
		out[int(r.at.Sub(from)/size)].addCounts(countsOf(r.res))
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
		SELECT hour, attempts, ok, lossy, dns_errors, skipped, ssh_errors, tool_errors, blocked,
		       sent, received, rtt_n, rtt_sum_us, rtt_min_us, rtt_max_us, vantage_errors, api_errors, expired
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
		var c hourCounts
		if err := rows.Scan(&b.At, &c.attempts, &c.ok, &c.lossy, &c.dns, &c.skipped, &c.ssh, &c.tool, &c.blocked,
			&c.sent, &c.received, &c.rttN, &c.rttSum, &c.rttMin, &c.rttMax, &c.vantage, &c.api, &c.expired); err != nil {
			return nil, err
		}
		b.addCounts(c)
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
	// Failures 是这段时间里最近的若干次异常(失败、部分丢包、采集失败等),最新的在前。
	Failures []FailurePoint `json:"failures"`
}

// FailurePoint 是一次异常的时间与类别。
type FailurePoint struct {
	At     string `json:"at"`
	Status Status `json:"status"`
	Label  string `json:"label"`
	Error  string `json:"error"`
	// Sent / Received 只对 ICMP 的部分丢包与全丢有意义;Peer 是那一轮实际连的 IP。
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
	Peer     string `json:"peer"`
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
		SELECT at, status, error, sent, received, peer FROM probe_results
		 WHERE target_id = ? AND generation = ? AND at >= ? AND status NOT IN ('OK', 'SKIPPED')
		 ORDER BY at DESC, id DESC LIMIT 50`, t.ID, t.Generation, from.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FailurePoint
		if err := rows.Scan(&f.At, &f.Status, &f.Error, &f.Sent, &f.Received, &f.Peer); err != nil {
			return nil, err
		}
		f.Label = f.Status.LabelFor(t.Metric)
		h.Failures = append(h.Failures, f)
	}
	return h, rows.Err()
}
