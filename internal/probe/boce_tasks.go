package probe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/boce"
	"github.com/litebox/litebox/internal/netprobe"
)

// Boce 任务:一台节点上的一个入口 + 若干拨测点 + 一张时间表。
//
// 「任务定义」与「单次执行记录」分开:改任务、暂停任务、归档任务都不删已有的检测历史;
// 只有显式删除才连序列与历史一起删(执行记录与估算消耗保留,预算不会因为删任务凭空多出来)。

// 错误。
var (
	ErrBoceTaskNotFound = errors.New("Boce 任务不存在")
	// ErrBoceIPv6 Boce 的 TCPing / Ping 文档都没有说明支持 IPv6:不提交,也不改用 IPv4 测(那测的就不是这个入口了)。
	ErrBoceIPv6 = errors.New("Boce TCPing / Ping 接口都没有说明支持 IPv6 目标:面板不提交 IPv6 入口,也不会改用 IPv4 去测(那测的就不是这个入口了)")
	// ErrBoceTestType 不认识的测法。
	ErrBoceTestType = errors.New("检测方式只能是 TCPing 或 Ping")
	// ErrBoceTestTypeImmutable 测法创建后不能改:换测法就是另一条测量,旧的点阵与丢包率与它不可比。
	ErrBoceTestTypeImmutable = errors.New("任务创建后不能更换检测方式:换测法就是另一条测量,请复制任务后修改")
	// ErrBoceTargetNotAllowed 被测目标只能是这台节点的订阅入口 —— 不收自由输入的地址。
	ErrBoceTargetNotAllowed = errors.New("被测目标只能从这台节点的订阅入口里选")
	ErrBoceRefImmutable     = errors.New("任务创建后不能更换被测入口:换目标就是另一条测量,请复制任务后修改")
	ErrBoceArchived         = errors.New("任务已归档,先恢复再修改")
)

// BoceTask 是一个任务的定义与最近状态。
type BoceTask struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	// TestType 是测法(TCPING / PING),创建后不可改。0043 的 check_type 列已冻结,看这一列。
	TestType boce.Method `json:"test_type"`
	Ref      string      `json:"ref"`
	Family   Family      `json:"family"`
	// VantageIDs 是选中的拨测点(有序)。
	VantageIDs []int `json:"vantage_ids"`
	ScheduleSpec
	MonthPointsLimit int  `json:"month_points_limit"`
	Enabled          bool `json:"enabled"`
	// AutoPausedReason 非空 = 连续被 Boce 明确拒绝之后自动停了,要管理员重新打开。
	AutoPausedReason    string `json:"auto_paused_reason"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	NextRunAt           string `json:"next_run_at"`
	LastRunAt           string `json:"last_run_at"`
	LastState           string `json:"last_state"`
	LastError           string `json:"last_error"`
	ArchivedAt          string `json:"archived_at"`
	Note                string `json:"note"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// Archived 回答任务是不是归档了。
func (t *BoceTask) Archived() bool { return t.ArchivedAt != "" }

// Schedule 解析时间表。存库前已经校验过,这里坏了只可能是库被手改过 —— 返回错误,调度跳过它。
func (t *BoceTask) Schedule() (Schedule, error) {
	_, sc, err := t.ScheduleSpec.Normalize()
	return sc, err
}

// BoceTaskInput 是创建 / 修改任务的输入。Enabled 为 nil 表示不改(创建时默认关)。
// TestType 创建时空 = TCPing;修改时空 = 不改,给了别的值就拒绝(测法不能改)。
type BoceTaskInput struct {
	Name       string      `json:"name"`
	TestType   boce.Method `json:"test_type"`
	Ref        string      `json:"ref"`
	Family     Family      `json:"family"`
	VantageIDs []int       `json:"vantage_ids"`
	ScheduleSpec
	MonthPointsLimit int    `json:"month_points_limit"`
	Enabled          *bool  `json:"enabled"`
	Note             string `json:"note"`
}

const boceTaskColumns = `id, node_id, name, test_type, ref, family, vantage_ids, schedule_mode, interval_min, times,
	window_start, window_end, weekdays, timezone, month_points_limit, enabled, auto_paused_reason, consecutive_failures,
	next_run_at, last_run_at, last_state, last_error, archived_at, note, created_at, updated_at`

func scanBoceTask(r scanner) (*BoceTask, error) {
	var t BoceTask
	var vids, times string
	var enabled int
	if err := r.Scan(&t.ID, &t.NodeID, &t.Name, &t.TestType, &t.Ref, &t.Family, &vids, &t.Mode, &t.IntervalMin, &times,
		&t.WindowStart, &t.WindowEnd, &t.Weekdays, &t.Timezone, &t.MonthPointsLimit, &enabled, &t.AutoPausedReason,
		&t.ConsecutiveFailures, &t.NextRunAt, &t.LastRunAt, &t.LastState, &t.LastError, &t.ArchivedAt, &t.Note,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.VantageIDs = parseIntList(vids)
	t.Times = []string{}
	if times != "" {
		t.Times = strings.Split(times, ",")
	}
	return &t, nil
}

func parseIntList(raw string) []int {
	var out []int
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []int{}
	}
	return out
}

func listBoceTasksQ(ctx context.Context, q querier, where string, args ...any) ([]BoceTask, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+boceTaskColumns+` FROM boce_tasks `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoceTask{}
	for rows.Next() {
		t, err := scanBoceTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetBoceTask 读一个任务。
func (s *Store) GetBoceTask(ctx context.Context, id int64) (*BoceTask, error) {
	t, err := scanBoceTask(s.db.QueryRowContext(ctx, `SELECT `+boceTaskColumns+` FROM boce_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBoceTaskNotFound
	}
	return t, err
}

// ListBoceTasks 列出一台节点的任务(含归档的,排在最后)。
func (s *Store) ListBoceTasks(ctx context.Context, nodeID int64) ([]BoceTask, error) {
	return listBoceTasksQ(ctx, s.db, `WHERE node_id = ? ORDER BY archived_at != '', id`, nodeID)
}

// ListAllBoceTasks 列出未删除节点上的全部任务(设置页的总览、费用汇总用)。
func (s *Store) ListAllBoceTasks(ctx context.Context) ([]BoceTask, error) {
	return listBoceTasksQ(ctx, s.db, `WHERE node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)
		ORDER BY archived_at != '', node_id, id`)
}

// BoceCandidates 是一台节点上能被 Boce 测的入口:订阅入口(不含管理通道 —— 不把 sshd 交给第三方拨测点去扫)。
// IPv6 的也列出来,但带着 Note 说明为什么选不了 —— 不能让它安静地从下拉里消失。
func (s *Store) BoceCandidates(ctx context.Context, nodeID int64) ([]Candidate, error) {
	cands, err := s.Candidates(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := []Candidate{}
	for _, c := range cands {
		if c.Kind != KindService {
			continue
		}
		c.Note = ""
		if reason := boceHostProblem(c.Family, c.Host); reason != "" {
			c.Note = reason
		}
		out = append(out, c)
	}
	return out, nil
}

// boceHostProblem 回答一个地址能不能交给 Boce 测;空串 = 能。
func boceHostProblem(f Family, host string) string {
	if f == FamilyV6 {
		return ErrBoceIPv6.Error()
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.Is4() {
			return ErrBoceIPv6.Error()
		}
		if r := netprobe.RestrictedAddr(ip); r != "" {
			return "这个入口的地址是" + r + ",外部拨测点连不到"
		}
	}
	return ""
}

func (s *Store) normalizeBoceInput(ctx context.Context, in *BoceTaskInput, vmap map[int]BoceVantage, keep map[int]bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if len([]rune(in.Name)) > 64 {
		return errors.New("任务名称最长 64 个字符")
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 200 {
		return errors.New("备注最长 200 个字符")
	}
	if len(in.VantageIDs) == 0 {
		return errors.New("至少选一个 Boce 拨测点")
	}
	if len(in.VantageIDs) > boce.MaxNodeIDs {
		return fmt.Errorf("一个任务最多选 %d 个拨测点", boce.MaxNodeIDs)
	}
	seen := map[int]bool{}
	ids := make([]int, 0, len(in.VantageIDs))
	for _, id := range in.VantageIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		v, ok := vmap[id]
		if !ok {
			return fmt.Errorf("拨测点 #%d 不在 Boce 节点列表里(先在系统设置里同步一次节点列表)", id)
		}
		// 已经从上游列表消失的拨测点:原来就选着的可以留着(界面上标着),新加的不收。
		if v.MissingSince != "" && !keep[id] {
			return fmt.Errorf("拨测点「%s」已经从 Boce 节点列表里消失,不能新加", v.Label())
		}
		ids = append(ids, id)
	}
	in.VantageIDs = ids
	if in.MonthPointsLimit < 0 || in.MonthPointsLimit > 100000000 {
		return errors.New("单任务每月波点上限必须是 0~100000000(0 = 只受全局预算约束)")
	}
	sp, _, err := in.ScheduleSpec.Normalize()
	if err != nil {
		return err
	}
	in.ScheduleSpec = sp
	return nil
}

// CreateBoceTask 在一台节点上建任务。默认不开自动检测 —— 管理员看过费用估算再打开。
func (s *Store) CreateBoceTask(ctx context.Context, nodeID int64, in BoceTaskInput) (*BoceTask, error) {
	if in.TestType == "" {
		in.TestType = boce.MethodTCPing
	}
	if !in.TestType.Valid() {
		return nil, ErrBoceTestType
	}
	if in.Family == "" {
		in.Family = FamilyV4
	}
	if in.Family != FamilyV4 {
		return nil, ErrBoceIPv6
	}
	cands, err := s.Candidates(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var hit *Candidate
	for i := range cands {
		if cands[i].Ref == in.Ref && cands[i].Kind == KindService {
			hit = &cands[i]
			break
		}
	}
	if hit == nil {
		return nil, ErrBoceTargetNotAllowed
	}
	if reason := boceHostProblem(hit.Family, hit.Host); reason != "" {
		if hit.Family == FamilyV6 {
			return nil, ErrBoceIPv6
		}
		return nil, errors.New(reason)
	}
	vmap, err := s.boceVantageMap(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.normalizeBoceInput(ctx, &in, vmap, nil); err != nil {
		return nil, err
	}
	if in.Name == "" {
		in.Name = hit.Label
	}
	enabled := in.Enabled != nil && *in.Enabled
	now := s.now().UTC()
	nowTS := now.Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// check_type 是 0043 的列,CHECK 只收 'TCPING',已冻结;测法写 test_type。
	res, err := tx.ExecContext(ctx, `INSERT INTO boce_tasks (node_id, name, check_type, test_type, ref, family, vantage_ids, schedule_mode,
		interval_min, times, window_start, window_end, weekdays, timezone, month_points_limit, enabled, note, created_at, updated_at)
		VALUES (?,?,'TCPING',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, in.Name, in.TestType, hit.Ref, FamilyV4, intsJSON(in.VantageIDs), in.Mode, in.IntervalMin, strings.Join(in.Times, ","),
		in.WindowStart, in.WindowEnd, in.Weekdays, in.Timezone, in.MonthPointsLimit, b2i(enabled), in.Note, nowTS, nowTS)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	t, err := scanBoceTask(tx.QueryRowContext(ctx, `SELECT `+boceTaskColumns+` FROM boce_tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.syncBoceSeriesTx(ctx, tx, t, vmap, hit.Host, boceTargetPort(t.TestType, hit.Port)); err != nil {
		return nil, err
	}
	if err := setNextRunTx(ctx, tx, t, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBoceTask(ctx, id)
}

// UpdateBoceTask 改任务。被测入口不能换;拨测点去掉的序列停用(历史保留),加回来的接着用原来那条。
func (s *Store) UpdateBoceTask(ctx context.Context, id int64, in BoceTaskInput) (*BoceTask, error) {
	t, err := s.GetBoceTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Archived() {
		return nil, ErrBoceArchived
	}
	if (in.Ref != "" && in.Ref != t.Ref) || (in.Family != "" && in.Family != t.Family) {
		return nil, ErrBoceRefImmutable
	}
	if in.TestType != "" && in.TestType != t.TestType {
		return nil, ErrBoceTestTypeImmutable
	}
	vmap, err := s.boceVantageMap(ctx)
	if err != nil {
		return nil, err
	}
	keep := map[int]bool{}
	for _, v := range t.VantageIDs {
		keep[v] = true
	}
	if err := s.normalizeBoceInput(ctx, &in, vmap, keep); err != nil {
		return nil, err
	}
	if in.Name == "" {
		in.Name = t.Name
	}
	enabled := t.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := s.now().UTC()
	nowTS := now.Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 重新打开自动检测 = 管理员看过了:清掉自动停下的原因与连续失败计数。
	clear := ""
	if enabled && (!t.Enabled || t.AutoPausedReason != "") && in.Enabled != nil {
		clear = `, auto_paused_reason = '', consecutive_failures = 0`
	}
	if _, err := tx.ExecContext(ctx, `UPDATE boce_tasks SET name = ?, vantage_ids = ?, schedule_mode = ?, interval_min = ?,
		times = ?, window_start = ?, window_end = ?, weekdays = ?, timezone = ?, month_points_limit = ?, enabled = ?, note = ?,
		updated_at = ?`+clear+` WHERE id = ?`,
		in.Name, intsJSON(in.VantageIDs), in.Mode, in.IntervalMin, strings.Join(in.Times, ","), in.WindowStart, in.WindowEnd,
		in.Weekdays, in.Timezone, in.MonthPointsLimit, b2i(enabled), in.Note, nowTS, id); err != nil {
		return nil, err
	}
	nt, err := scanBoceTask(tx.QueryRowContext(ctx, `SELECT `+boceTaskColumns+` FROM boce_tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.syncBoceSeriesTx(ctx, tx, nt, vmap, "", 0); err != nil {
		return nil, err
	}
	if err := setNextRunTx(ctx, tx, nt, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBoceTask(ctx, id)
}

// SetBoceTaskEnabled 开 / 关一个任务的自动检测。打开时清掉自动停下的原因。
func (s *Store) SetBoceTaskEnabled(ctx context.Context, id int64, enabled bool) (*BoceTask, error) {
	t, err := s.GetBoceTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Archived() {
		return nil, ErrBoceArchived
	}
	now := s.now().UTC()
	q := `UPDATE boce_tasks SET enabled = ?, updated_at = ? WHERE id = ?`
	if enabled {
		q = `UPDATE boce_tasks SET enabled = ?, updated_at = ?, auto_paused_reason = '', consecutive_failures = 0 WHERE id = ?`
	}
	if _, err := s.db.ExecContext(ctx, q, b2i(enabled), now.Format(time.RFC3339), id); err != nil {
		return nil, err
	}
	t.Enabled = enabled
	if enabled {
		t.AutoPausedReason = ""
	}
	if err := setNextRunTx(ctx, s.db, t, now); err != nil {
		return nil, err
	}
	return s.GetBoceTask(ctx, id)
}

// ArchiveBoceTask 归档 / 恢复。归档:停止执行、序列停用、历史保留;恢复后自动检测仍是关的,要管理员再打开。
func (s *Store) ArchiveBoceTask(ctx context.Context, id int64, archived bool) (*BoceTask, error) {
	t, err := s.GetBoceTask(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	nowTS := now.Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	at := ""
	if archived {
		at = nowTS
	}
	if _, err := tx.ExecContext(ctx, `UPDATE boce_tasks SET archived_at = ?, enabled = 0, next_run_at = '', updated_at = ? WHERE id = ?`,
		at, nowTS, id); err != nil {
		return nil, err
	}
	t.ArchivedAt, t.Enabled = at, false
	if err := s.syncBoceSeriesTx(ctx, tx, t, nil, "", 0); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetBoceTask(ctx, id)
}

// DeleteBoceTask 删任务:序列与它们的历史随外键一起删;执行记录留着(task_id 置空),
// 那是已经花掉的估算消耗,删了它当月预算就凭空多出一截。
func (s *Store) DeleteBoceTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM boce_tasks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBoceTaskNotFound
	}
	return nil
}

// DeleteBoceTasksByNode 删一台节点的全部任务。节点是软删除,外键的 CASCADE 兜不住,删节点时显式调它。
func (s *Store) DeleteBoceTasksByNode(ctx context.Context, nodeID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM boce_tasks WHERE node_id = ?`, nodeID)
	return err
}

// syncBoceSeriesTx 让 probe_targets 里这个任务的序列与它选的拨测点一致:
// 选着的有一条且启用;去掉的停用(不删,历史保留);归档的全部停用。
// host / port 非空时作为新序列的初始解析结果(创建任务时用候选的值,之后每轮由引擎重新解析)。
func (s *Store) syncBoceSeriesTx(ctx context.Context, tx querier, t *BoceTask, vmap map[int]BoceVantage, host string, port int) error {
	existing, err := listQ(ctx, tx, `WHERE source = 'BOCE' AND boce_task_id = ?`, t.ID)
	if err != nil {
		return err
	}
	byVantage := map[int]Target{}
	for _, e := range existing {
		byVantage[e.VantageID] = e
	}
	want := map[int]bool{}
	if !t.Archived() {
		for _, v := range t.VantageIDs {
			want[v] = true
		}
	}
	now := s.now().UTC().Format(time.RFC3339)
	if host == "" && len(existing) > 0 {
		host, port = existing[0].ResolvedHost, existing[0].ResolvedPort
	}
	gen, genSince := 1, now
	if len(existing) > 0 {
		gen, genSince = existing[0].Generation, existing[0].GenerationSince
	}
	for _, v := range t.VantageIDs {
		if !want[v] {
			continue
		}
		label := fmt.Sprintf("拨测点 #%d", v)
		if vv, ok := vmap[v]; ok {
			label = vv.Label()
		}
		if e, ok := byVantage[v]; ok {
			if vmap == nil {
				label = e.Label
			}
			if _, err := tx.ExecContext(ctx, `UPDATE probe_targets SET enabled = 1, label = ?, updated_at = ? WHERE id = ?`,
				label, now, e.ID); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO probe_targets (node_id, direction, protocol, tcp_mode, kind, ref, family,
			label, source, enabled, resolved_host, resolved_port, generation, generation_since, created_at, updated_at,
			boce_task_id, vantage_id)
			VALUES (?, 'INGRESS', ?, 'CONNECT', 'SERVICE', ?, ?, ?, 'BOCE', 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.NodeID, boceProtocol(t.TestType), t.Ref, t.Family, label, host, port, gen, genSince, now, now, t.ID, v); err != nil {
			return err
		}
	}
	for _, e := range existing {
		if want[e.VantageID] || !e.Enabled {
			continue
		}
		// 停用时清掉告警状态:已经不测的序列不能留着一个「连续失败」挂在那里。
		if _, err := tx.ExecContext(ctx, `UPDATE probe_targets SET enabled = 0, updated_at = ?,
			consecutive_failures = 0, alert_state = 'OK', alert_since = '', alert_notified = 0, recovery_pending = 0
			WHERE id = ?`, now, e.ID); err != nil {
			return err
		}
	}
	return nil
}

// boceTargetPort 是 Boce 序列记的端口:Ping 与端口无关,一律记 0 —— 入口换了端口而地址没变时,
// ping 测的还是同一个东西,不该换代、把历史切成两段。
func boceTargetPort(m boce.Method, port int) int {
	if m == boce.MethodPing {
		return 0
	}
	return port
}

// boceSubmitHost 是提交给 Boce 的 host:TCPing 是「地址:端口」,Ping 只要地址。
func boceSubmitHost(m boce.Method, host string, port int) string {
	if m == boce.MethodPing {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// boceGrace 是错过的时间槽还补不补的界线:10 分钟之内补最近的一个,再晚就跳过 ——
// 一台该 9 点测的机器在 9 点 40 才补测,那个数字已经说明不了 9 点的情况,只是白花一次钱。
const boceGrace = 10 * time.Minute

// dueSlot 回答此刻这个任务有没有到期的时间槽:返回时间槽与执行时间。
func dueSlot(sc Schedule, taskID int64, now time.Time) (slot time.Time, ok bool) {
	p := sc.Prev(now)
	if p.IsZero() {
		return time.Time{}, false
	}
	if now.Before(p.Add(sc.Stagger(taskID, p))) || now.Sub(p) > boceGrace {
		return time.Time{}, false
	}
	if end := sc.windowEnd(p); !end.IsZero() && !now.Before(end) {
		// 已经进入关闭时段:不再创建这个时间槽的付费任务。
		return time.Time{}, false
	}
	return p, true
}

// nextRun 是这个任务下一次自动执行的时间(给人看的);不会自动执行时为零值。
func nextRun(t *BoceTask, now time.Time) time.Time {
	if !t.Enabled || t.Archived() || t.AutoPausedReason != "" {
		return time.Time{}
	}
	sc, err := t.Schedule()
	if err != nil {
		return time.Time{}
	}
	if p := sc.Prev(now); !p.IsZero() {
		if exec := p.Add(sc.Stagger(t.ID, p)); exec.After(now) && now.Sub(p) <= boceGrace {
			return exec
		}
	}
	n := sc.Next(now)
	if n.IsZero() {
		return n
	}
	return n.Add(sc.Stagger(t.ID, n))
}

func setNextRunTx(ctx context.Context, q querier, t *BoceTask, now time.Time) error {
	next := ""
	if n := nextRun(t, now); !n.IsZero() {
		next = n.UTC().Format(time.RFC3339)
	}
	_, err := q.ExecContext(ctx, `UPDATE boce_tasks SET next_run_at = ? WHERE id = ?`, next, t.ID)
	return err
}
