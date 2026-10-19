package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/boce"
)

// Boce 的执行记录与预算。
//
// 一轮 = 一次 Boce 任务 = boce_runs 的一行。预算预留与插入这一行在同一个事务里 ——
// 数据库只有一条连接,事务天然串行,两轮并发也不会一起越过上限。

// 执行状态。
const (
	BoceRunCreating = "CREATING"
	BoceRunUnknown  = "UNKNOWN"
	BoceRunPolling  = "POLLING"
	BoceRunDone     = "DONE"
	BoceRunTimeout  = "TIMEOUT"
	BoceRunExpired  = "EXPIRED"
	BoceRunFailed   = "FAILED"
	BoceRunSkipped  = "SKIPPED"
)

// 触发方式。
const (
	BoceTriggerSchedule = "SCHEDULE"
	BoceTriggerManual   = "MANUAL"
)

// 取结果的节奏(官方建议约 10 秒一次,最多等 2 分钟)。
const (
	BocePollEvery = 10 * time.Second
	BoceMaxWait   = 2 * time.Minute
	// BoceRunRetention 是执行记录的保留期。
	BoceRunRetention = 90 * 24 * time.Hour
)

// BoceRunStateText 是执行状态给人看的说法。
func BoceRunStateText(st string) string {
	switch st {
	case BoceRunCreating:
		return "正在创建"
	case BoceRunUnknown:
		return "创建结果不确定(不重试,按已扣费估算)"
	case BoceRunPolling:
		return "等待结果"
	case BoceRunDone:
		return "已完成"
	case BoceRunTimeout:
		return "超时(2 分钟内没有全部完成)"
	case BoceRunExpired:
		return "结果已失效"
	case BoceRunFailed:
		return "创建失败(没有扣费)"
	case BoceRunSkipped:
		return "未执行"
	}
	return st
}

// BoceRun 是一轮执行。
type BoceRun struct {
	ID int64 `json:"id"`
	// TaskID 为 0 表示任务已经删了(执行记录与估算消耗留着)。
	TaskID  int64  `json:"task_id"`
	NodeID  int64  `json:"node_id"`
	Trigger string `json:"trigger"`
	// TestType 是这一轮提交时的测法;取结果按它选接口。
	TestType       boce.Method  `json:"test_type"`
	Slot           string       `json:"slot"`
	State          string       `json:"state"`
	StateText      string       `json:"state_text"`
	VendorTaskID   string       `json:"vendor_task_id"`
	Host           string       `json:"host"`
	Generation     int          `json:"generation"`
	VantageIDs     []int        `json:"vantage_ids"`
	Points         int          `json:"points"`
	PointsReserved int          `json:"points_reserved"`
	DayKey         string       `json:"day_key"`
	MonthKey       string       `json:"month_key"`
	Attempts       int          `json:"attempts"`
	Polls          int          `json:"polls"`
	NextPollAt     string       `json:"next_poll_at"`
	Results        int          `json:"results"`
	ErrorCode      int          `json:"error_code"`
	Error          string       `json:"error"`
	CreatedAt      string       `json:"created_at"`
	SentAt         string       `json:"sent_at"`
	FinishedAt     string       `json:"finished_at"`
	Items          []BoceResult `json:"items"`
}

// BoceResult 是一个拨测点在一轮里的结果(供应商原始字段 + 我们归的类)。
type BoceResult struct {
	VantageID  int     `json:"vantage_id"`
	TargetID   int64   `json:"target_id"`
	At         string  `json:"at"`
	Status     Status  `json:"status"`
	Label      string  `json:"label"`
	NodeName   string  `json:"node_name"`
	OriginIP   string  `json:"origin_ip"`
	IP         string  `json:"ip"`
	IPRegion   string  `json:"ip_region"`
	IPISP      string  `json:"ip_isp"`
	PacketLoss float64 `json:"packet_loss"`
	Sent       int     `json:"sent"`
	Received   int     `json:"received"`
	RTTAvgMs   float64 `json:"rtt_avg_ms"`
	RTTMinMs   float64 `json:"rtt_min_ms"`
	RTTMaxMs   float64 `json:"rtt_max_ms"`
	ErrorCode  int     `json:"error_code"`
	Error      string  `json:"error"`
	Charged    bool    `json:"charged"`
}

const boceRunColumns = `id, COALESCE(task_id, 0), node_id, trigger_kind, test_type, slot, state, vendor_task_id, host, generation, vantage_ids,
	points, points_reserved, day_key, month_key, attempts, polls, next_poll_at, results, error_code, error,
	created_at, sent_at, finished_at`

func scanBoceRun(r scanner) (*BoceRun, error) {
	var b BoceRun
	var vids string
	if err := r.Scan(&b.ID, &b.TaskID, &b.NodeID, &b.Trigger, &b.TestType, &b.Slot, &b.State, &b.VendorTaskID, &b.Host, &b.Generation, &vids,
		&b.Points, &b.PointsReserved, &b.DayKey, &b.MonthKey, &b.Attempts, &b.Polls, &b.NextPollAt, &b.Results,
		&b.ErrorCode, &b.Error, &b.CreatedAt, &b.SentAt, &b.FinishedAt); err != nil {
		return nil, err
	}
	b.VantageIDs = parseIntList(vids)
	b.StateText = BoceRunStateText(b.State)
	b.Items = []BoceResult{}
	return &b, nil
}

func listBoceRunsQ(ctx context.Context, q querier, where string, args ...any) ([]BoceRun, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+boceRunColumns+` FROM boce_runs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoceRun{}
	for rows.Next() {
		b, err := scanBoceRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// GetBoceRun 读一轮(含拨测点明细)。
func (s *Store) GetBoceRun(ctx context.Context, id int64) (*BoceRun, error) {
	list, err := listBoceRunsQ(ctx, s.db, `WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	if err := s.fillBoceItems(ctx, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

// ListBoceRuns 列出一个任务最近的执行记录(新的在前,含明细)。
func (s *Store) ListBoceRuns(ctx context.Context, taskID int64, limit int) ([]BoceRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	list, err := listBoceRunsQ(ctx, s.db, `WHERE task_id = ? ORDER BY id DESC LIMIT ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	return list, s.fillBoceItems(ctx, list)
}

func (s *Store) fillBoceItems(ctx context.Context, runs []BoceRun) error {
	if len(runs) == 0 {
		return nil
	}
	idx := map[int64]int{}
	ph := make([]string, len(runs))
	args := make([]any, len(runs))
	for i, r := range runs {
		idx[r.ID] = i
		ph[i] = "?"
		args[i] = r.ID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, vantage_id, target_id, at, status, node_name, origin_ip, ip, ip_region,
		ip_isp, packet_loss, sent, received, rtt_avg_ms, rtt_min_ms, rtt_max_ms, error_code, error, charged
		FROM boce_results WHERE run_id IN (`+strings.Join(ph, ",")+`) ORDER BY run_id, vantage_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var runID int64
		var it BoceResult
		var charged int
		if err := rows.Scan(&runID, &it.VantageID, &it.TargetID, &it.At, &it.Status, &it.NodeName, &it.OriginIP, &it.IP,
			&it.IPRegion, &it.IPISP, &it.PacketLoss, &it.Sent, &it.Received, &it.RTTAvgMs, &it.RTTMinMs, &it.RTTMaxMs,
			&it.ErrorCode, &it.Error, &charged); err != nil {
			return err
		}
		it.Charged = charged == 1
		if i, ok := idx[runID]; ok {
			it.Label = it.Status.LabelFor(BoceMetric(runs[i].TestType))
			runs[i].Items = append(runs[i].Items, it)
		}
	}
	return rows.Err()
}

// ---------- 预算 ----------

// BoceUsage 是面板自己的估算消耗(不是 Boce 账户的真实余额 —— 那可能受面板以外的调用影响)。
type BoceUsage struct {
	DayKey       string  `json:"day_key"`
	MonthKey     string  `json:"month_key"`
	TodayPoints  int     `json:"today_points"`
	MonthPoints  int     `json:"month_points"`
	TotalPoints  int     `json:"total_points"`
	DailyLimit   int     `json:"daily_limit"`
	MonthlyLimit int     `json:"monthly_limit"`
	TodayYuan    float64 `json:"today_yuan"`
	MonthYuan    float64 `json:"month_yuan"`
	TotalYuan    float64 `json:"total_yuan"`
	// Timezone 是切日 / 切月的时区。
	Timezone string `json:"timezone"`
}

func budgetKeys(cfg BoceConfig, now time.Time) (day, month string) {
	lt := now.In(cfg.loc())
	return lt.Format("2006-01-02"), lt.Format("2006-01")
}

func sumPoints(ctx context.Context, q querier, where string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(points), 0) FROM boce_runs WHERE `+where, args...).Scan(&n)
	return n, err
}

// BoceUsage 读今日、本月与累计的估算消耗。
func (s *Store) BoceUsage(ctx context.Context, cfg BoceConfig) (BoceUsage, error) {
	day, month := budgetKeys(cfg, s.now())
	u := BoceUsage{DayKey: day, MonthKey: month, DailyLimit: cfg.DailyLimit, MonthlyLimit: cfg.MonthlyLimit,
		Timezone: cfg.loc().String()}
	var err error
	if u.TodayPoints, err = sumPoints(ctx, s.db, `day_key = ?`, day); err != nil {
		return u, err
	}
	if u.MonthPoints, err = sumPoints(ctx, s.db, `month_key = ?`, month); err != nil {
		return u, err
	}
	if u.TotalPoints, err = sumPoints(ctx, s.db, `1 = 1`); err != nil {
		return u, err
	}
	u.TodayYuan = float64(u.TodayPoints) * cfg.PointPrice
	u.MonthYuan = float64(u.MonthPoints) * cfg.PointPrice
	u.TotalYuan = float64(u.TotalPoints) * cfg.PointPrice
	return u, nil
}

// BoceTaskUsage 是各任务本月 / 累计的估算消耗(按任务 ID)。
func (s *Store) BoceTaskUsage(ctx context.Context, cfg BoceConfig) (map[int64][2]int, error) {
	_, month := budgetKeys(cfg, s.now())
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, COALESCE(SUM(CASE WHEN month_key = ? THEN points ELSE 0 END), 0),
		COALESCE(SUM(points), 0) FROM boce_runs WHERE task_id IS NOT NULL GROUP BY task_id`, month)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][2]int{}
	for rows.Next() {
		var id int64
		var m, total int
		if err := rows.Scan(&id, &m, &total); err != nil {
			return nil, err
		}
		out[id] = [2]int{m, total}
	}
	return out, rows.Err()
}

// boceRunRequest 是要预留的一轮。
type boceRunRequest struct {
	Task       *BoceTask
	Trigger    string
	Slot       time.Time
	IdemKey    string
	Host       string
	Generation int
	Vantages   []int
	// SkipReason 非空时直接记一行 SKIPPED(比如云实例停着),不占预算、不发请求。
	SkipReason string
}

// boceReservation 是预留的结果。
type boceReservation struct {
	Run *BoceRun
	// Exists 为真表示这个幂等键已经有一行了:这一轮已经处理过,什么都不做。
	Exists bool
	// Limit 非空表示被预算拦下(值是哪一道:RUN / TASK / DAY / MONTH),Run 是那一行 SKIPPED。
	Limit string
	// DayUsed / MonthUsed 是预留之后的今日 / 本月估算消耗(给 80% 提醒用)。
	DayUsed, MonthUsed int
}

// 预算拦下的几道。
const (
	boceLimitRun   = "RUN"
	boceLimitTask  = "TASK"
	boceLimitDay   = "DAY"
	boceLimitMonth = "MONTH"
)

// reserveBoceRun 在一个事务里:查幂等键 → 查四道预算 → 插入这一轮(CREATING 或 SKIPPED)。
func (s *Store) reserveBoceRun(ctx context.Context, cfg BoceConfig, req boceRunRequest) (boceReservation, error) {
	var res boceReservation
	now := s.now().UTC()
	nowTS := now.Format(time.RFC3339)
	day, month := budgetKeys(cfg, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM boce_runs WHERE idem_key = ?`, req.IdemKey).Scan(&exists); err != nil {
		return res, err
	}
	if exists > 0 {
		res.Exists = true
		return res, nil
	}
	points := len(req.Vantages) * cfg.ppc()
	dayUsed, err := sumPoints(ctx, tx, `day_key = ?`, day)
	if err != nil {
		return res, err
	}
	monthUsed, err := sumPoints(ctx, tx, `month_key = ?`, month)
	if err != nil {
		return res, err
	}
	taskUsed, err := sumPoints(ctx, tx, `task_id = ? AND month_key = ?`, req.Task.ID, month)
	if err != nil {
		return res, err
	}
	state, errText := BoceRunCreating, req.SkipReason
	if errText != "" {
		state, points = BoceRunSkipped, 0
	} else {
		switch {
		case cfg.RunLimit > 0 && points > cfg.RunLimit:
			res.Limit = boceLimitRun
			errText = fmt.Sprintf("这一轮要 %d 波点,超过单次任务上限 %d", points, cfg.RunLimit)
		case req.Task.MonthPointsLimit > 0 && taskUsed+points > req.Task.MonthPointsLimit:
			res.Limit = boceLimitTask
			errText = fmt.Sprintf("这个任务本月已用约 %d 波点,再测一轮(%d)会超过任务上限 %d", taskUsed, points, req.Task.MonthPointsLimit)
		case dayUsed+points > cfg.DailyLimit:
			res.Limit = boceLimitDay
			errText = fmt.Sprintf("今日已用约 %d 波点,再测一轮(%d)会超过每日上限 %d", dayUsed, points, cfg.DailyLimit)
		case monthUsed+points > cfg.MonthlyLimit:
			res.Limit = boceLimitMonth
			errText = fmt.Sprintf("本月已用约 %d 波点,再测一轮(%d)会超过每月上限 %d", monthUsed, points, cfg.MonthlyLimit)
		}
		if res.Limit != "" {
			state, points = BoceRunSkipped, 0
			errText = SkipBudget + ":" + errText
		}
	}
	slot := ""
	if !req.Slot.IsZero() {
		slot = req.Slot.UTC().Format(time.RFC3339)
	}
	finished := ""
	if state == BoceRunSkipped {
		finished = nowTS
	}
	testType := req.Task.TestType
	if !testType.Valid() {
		testType = boce.MethodTCPing
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO boce_runs (task_id, node_id, trigger_kind, test_type, slot, idem_key, state, host, generation,
		vantage_ids, points, points_reserved, day_key, month_key, error, created_at, finished_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.Task.ID, req.Task.NodeID, req.Trigger, testType, slot, req.IdemKey, state, req.Host, req.Generation,
		intsJSON(req.Vantages), points, points, day, month, errText, nowTS, finished)
	if err != nil {
		return res, err
	}
	id, _ := r.LastInsertId()
	run, err := scanBoceRun(tx.QueryRowContext(ctx, `SELECT `+boceRunColumns+` FROM boce_runs WHERE id = ?`, id))
	if err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.Run = run
	res.DayUsed, res.MonthUsed = dayUsed+points, monthUsed+points
	return res, nil
}

// markBoceSent 记下拿到的任务 ID:进入取结果阶段。
func (s *Store) markBoceSent(ctx context.Context, runID int64, vendorID string, attempts int) error {
	now := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE boce_runs SET state = 'POLLING', vendor_task_id = ?, attempts = ?, sent_at = ?,
		next_poll_at = ? WHERE id = ? AND state = 'CREATING'`,
		vendorID, attempts, now.Format(time.RFC3339), now.Add(BocePollEvery).Format(time.RFC3339), runID)
	return err
}

// endBoceRun 收尾一轮(失败 / 不确定 / 跳过 / 完成)。points 是最终的估算消耗。
func (s *Store) endBoceRun(ctx context.Context, q querier, runID int64, state string, points, attempts, code int, errText string) error {
	_, err := q.ExecContext(ctx, `UPDATE boce_runs SET state = ?, points = ?, attempts = MAX(attempts, ?), error_code = ?,
		error = ?, next_poll_at = '', finished_at = ? WHERE id = ?`,
		state, points, attempts, code, truncateText(errText, 300), s.now().UTC().Format(time.RFC3339), runID)
	return err
}

// pollingBoceRuns 是该去取结果的那几轮。
func (s *Store) pollingBoceRuns(ctx context.Context, now time.Time) ([]BoceRun, error) {
	return listBoceRunsQ(ctx, s.db, `WHERE state = 'POLLING' AND next_poll_at <= ? ORDER BY next_poll_at, id`,
		now.UTC().Format(time.RFC3339))
}

// creatingBoceRuns 是停在 CREATING 的那几轮。启动时引擎把它们记成 UNKNOWN:不知道请求发没发出去,
// 重发就可能扣两次钱。
func (s *Store) creatingBoceRuns(ctx context.Context) ([]BoceRun, error) {
	return listBoceRunsQ(ctx, s.db, `WHERE state = 'CREATING'`)
}

// bumpBocePoll 记一次没取到结果的轮询,10 秒后再问。
func (s *Store) bumpBocePoll(ctx context.Context, runID int64, errText string) error {
	now := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE boce_runs SET polls = polls + 1, next_poll_at = ?, error = ? WHERE id = ? AND state = 'POLLING'`,
		now.Add(BocePollEvery).Format(time.RFC3339), truncateText(errText, 300), runID)
	return err
}

// boceRunByIdem 按幂等键读一轮。
func (s *Store) boceRunByIdem(ctx context.Context, key string) (*BoceRun, error) {
	list, err := listBoceRunsQ(ctx, s.db, `WHERE idem_key = ?`, key)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetBoceRun(ctx, list[0].ID)
}

// PruneBoceRuns 删掉超出保留期的执行记录(明细随外键一起删)。正在进行的不删。
func (s *Store) PruneBoceRuns(ctx context.Context, now time.Time) (int64, error) {
	cut := now.UTC().Add(-BoceRunRetention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM boce_runs WHERE created_at < ? AND state NOT IN ('CREATING', 'POLLING')`, cut)
	if err != nil {
		return 0, err
	}
	// 按日 / 月去重的提醒键过了保留期也没用了(那一天、那一个月早就过去);接口异常与低余额那两个键不按时间删。
	if _, err := s.db.ExecContext(ctx, `DELETE FROM boce_marks WHERE at < ? AND (key LIKE 'budget-%')`, cut); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- 结果落库 ----------

// classifyBoceItem 把一个拨测点的结果归类。
//
// error_code 非 0 是拨测点的事(上游:「视为拨测节点未响应」),不是目标不通;
// 一个包都没发出去也说明不了目标怎样。只有拨测点真的发了包,收发数才进比例。
func classifyBoceItem(m boce.Method, it boce.Item) (Status, string) {
	switch {
	case it.ErrorCode != 0:
		msg := "拨测点未响应"
		if it.NodeOffline() {
			msg = "拨测点已下线(不扣波点)"
		}
		if it.Error != "" {
			msg += ":" + it.Error
		}
		return StatusVantageError, msg
	case it.Sent <= 0:
		return StatusVantageError, "拨测点没有返回有效数据"
	case it.Received >= it.Sent:
		return StatusOK, ""
	case it.Received > 0:
		return StatusLoss, fmt.Sprintf("%s 收到 %d/%d", m.Label(), it.Received, it.Sent)
	}
	if m == boce.MethodPing {
		return StatusTimeout, fmt.Sprintf("Ping %d 个包全部没有回应", it.Sent)
	}
	return StatusTimeout, fmt.Sprintf("TCPing %d 次全部失败", it.Sent)
}

func msDur(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

// boceEmit 是落库之后要推的一条序列告警。
type boceEmit struct {
	t      Target
	u      TargetUpdate
	kind   string
	result Result
}

// boceSave 是一次落库的参数。
type boceSave struct {
	run   *BoceRun
	items []boce.Item
	// final 为真时这一轮收尾:没出结果的拨测点记 EXPIRED,state 写成 finalState。
	final      bool
	finalState string
	// finalNote 写进这一轮与任务的最近错误;missingNote 是没出结果的拨测点那一行的说明。
	finalNote   string
	missingNote string
	// failStatus 非空时不看 items,给这一轮的每个拨测点记这个状态(接口异常 / 未执行),error 是 failNote。
	failStatus Status
	failNote   string
	// alert 是告警阈值与免打扰。
	alertAfter int
	quiet      bool
	ppc        int
}

// saveBoceResults 把结果写进同一个事务:boce_results(按 run + 拨测点去重)、probe_results / probe_hourly、
// 序列状态与告警、这一轮的计数与收尾、任务的最近状态。返回要推的告警与新落库的条数。
func (s *Store) saveBoceResults(ctx context.Context, p boceSave) ([]boceEmit, int, error) {
	run := p.run
	now := s.now().UTC()
	nowTS := now.Format(time.RFC3339)
	at := now
	if v, err := time.Parse(time.RFC3339, run.SentAt); err == nil {
		at = v
	} else if v, err := time.Parse(time.RFC3339, run.CreatedAt); err == nil {
		at = v
	}
	atTS := at.UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	series := map[int]Target{}
	if run.TaskID > 0 {
		list, err := listQ(ctx, tx, `WHERE source = 'BOCE' AND boce_task_id = ?`, run.TaskID)
		if err != nil {
			return nil, 0, err
		}
		for _, t := range list {
			series[t.VantageID] = t
		}
	}
	asked := map[int]bool{}
	for _, v := range run.VantageIDs {
		asked[v] = true
	}
	have := map[int]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT vantage_id FROM boce_results WHERE run_id = ?`, run.ID)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, 0, err
		}
		have[v] = true
	}
	rows.Close()

	type pending struct {
		vantage int
		res     Result
	}
	var fresh []pending
	insert := func(v int, st Status, it *boce.Item, errText string, charged bool) error {
		var tid int64
		if t, ok := series[v]; ok {
			tid = t.ID
		}
		var item boce.Item
		if it != nil {
			item = *it
		}
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO boce_results (run_id, vantage_id, target_id, at, status, node_name,
			origin_ip, ip, ip_region, ip_isp, packet_loss, sent, received, rtt_avg_ms, rtt_min_ms, rtt_max_ms, error_code, error, charged)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			run.ID, v, tid, atTS, string(st), item.NodeName, item.OriginIP, item.IP, item.IPRegion, item.IPISP, item.PacketLoss,
			item.Sent, item.Received, item.RTTAvgMs, item.RTTMinMs, item.RTTMaxMs, item.ErrorCode, truncateText(errText, 300), b2i(charged))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return nil // 这个拨测点这一轮已经落过库:按「Boce 任务 ID + 拨测点 ID」去重
		}
		have[v] = true
		res := Result{Status: st, Error: errText}
		if it != nil && (st == StatusOK || st == StatusLoss || st == StatusTimeout) {
			res.Sent, res.Received = it.Sent, it.Received
			res.Peer = it.IP
			if st != StatusTimeout && it.RTTAvgMs > 0 {
				res.RTT, res.RTTMin, res.RTTMax = msDur(it.RTTAvgMs), msDur(it.RTTMinMs), msDur(it.RTTMaxMs)
				res.RTTN = it.Received
			}
		}
		fresh = append(fresh, pending{vantage: v, res: res})
		return nil
	}

	if p.failStatus != "" {
		for _, v := range run.VantageIDs {
			if have[v] {
				continue
			}
			if err := insert(v, p.failStatus, nil, p.failNote, false); err != nil {
				return nil, 0, err
			}
		}
	} else {
		for i := range p.items {
			it := p.items[i]
			if !asked[it.NodeID] {
				continue // 上游给了一个我们没要的拨测点:不收
			}
			st, msg := classifyBoceItem(run.TestType, it)
			if err := insert(it.NodeID, st, &it, msg, !it.NodeOffline()); err != nil {
				return nil, 0, err
			}
		}
		if p.final {
			note := p.missingNote
			if note == "" {
				note = "这个拨测点没有出结果"
			}
			for _, v := range run.VantageIDs {
				if have[v] {
					continue
				}
				// 没出结果的按已扣费算:不知道上游扣没扣,估算宁可偏高。
				if err := insert(v, StatusExpired, nil, note, true); err != nil {
					return nil, 0, err
				}
			}
		}
	}

	// 写进序列:样本、小时汇总、序列状态与告警。
	var samples []Sample
	updates := map[int64]*TargetUpdate{}
	var emits []boceEmit
	for _, f := range fresh {
		t, ok := series[f.vantage]
		if !ok {
			continue // 任务删了 / 序列没了:明细留在 boce_results 里
		}
		samples = append(samples, Sample{TargetID: t.ID, Generation: run.Generation, Source: SourceBoce, At: at, Result: f.res})
		if run.Generation != t.Generation {
			continue // 入口地址在这一轮之后已经变了:旧口径的样本照存,不动当前口径的状态
		}
		u := updates[t.ID]
		if u == nil {
			u = updateOf(t)
			updates[t.ID] = u
		}
		u.LastStatus, u.LastAt = string(f.res.Status), atTS
		if f.res.Status.Reachable() {
			u.LastSuccessAt = atTS
		}
		a, kinds := decideAlert(alertOf(u), f.res.Status, p.alertAfter, p.quiet, nowTS)
		applyAlert(u, a)
		for _, k := range kinds {
			emits = append(emits, boceEmit{t: t, u: *u, kind: k, result: f.res})
		}
	}
	list := make([]TargetUpdate, 0, len(updates))
	for _, u := range updates {
		list = append(list, *u)
	}
	if err := s.saveRoundTx(ctx, tx, samples, list); err != nil {
		return nil, 0, err
	}

	// 这一轮的计数与收尾。
	var total, offline int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN charged = 0 THEN 1 ELSE 0 END), 0)
		FROM boce_results WHERE run_id = ?`, run.ID).Scan(&total, &offline); err != nil {
		return nil, 0, err
	}
	if p.final {
		var points int
		switch {
		case p.failStatus != "" && p.finalState == BoceRunUnknown:
			// 发出去了但不知道结果:按预留的全部算(宁可估高,不能让预算漏掉一笔可能的扣费)。
			points = run.PointsReserved
		case p.failStatus != "":
			points = 0 // 明确没建任务 / 没执行
		default:
			// 上游说节点下线(not found node)的拨测点不扣波点。
			points = run.PointsReserved - offline*p.ppc
			if points < 0 {
				points = 0
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE boce_runs SET state = ?, results = ?, points = ?, next_poll_at = '',
			finished_at = ?, error = CASE WHEN ? != '' THEN ? ELSE error END WHERE id = ?`,
			p.finalState, total, points, nowTS, p.finalNote, truncateText(p.finalNote, 300), run.ID); err != nil {
			return nil, 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE boce_runs SET results = ?, polls = polls + 1, next_poll_at = ? WHERE id = ?`,
			total, now.Add(BocePollEvery).Format(time.RFC3339), run.ID); err != nil {
			return nil, 0, err
		}
	}
	if run.TaskID > 0 && (p.final || p.failStatus != "") {
		state := p.finalState
		errText := p.finalNote
		if p.failStatus != "" && errText == "" {
			errText = p.failNote
		}
		if _, err := tx.ExecContext(ctx, `UPDATE boce_tasks SET last_run_at = ?, last_state = ?, last_error = ? WHERE id = ?`,
			atTS, state, truncateText(errText, 300), run.TaskID); err != nil {
			return nil, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return emits, len(fresh), nil
}

// noteBoceTaskFailure 记一次任务级的明确失败(参数错误之类);连续 3 次自动停下这个任务的自动检测。
func (s *Store) noteBoceTaskFailure(ctx context.Context, taskID int64, reason string) (paused bool, err error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE boce_tasks SET consecutive_failures = consecutive_failures + 1 WHERE id = ?`, taskID); err != nil {
		return false, err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE boce_tasks SET enabled = 0, next_run_at = '', auto_paused_reason = ?
		WHERE id = ? AND consecutive_failures >= ? AND auto_paused_reason = ''`,
		truncateText("连续 3 次被 Boce 拒绝:"+reason, 300), taskID, boceTaskFailuresToPause)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

func (s *Store) noteBoceTaskOK(ctx context.Context, taskID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE boce_tasks SET consecutive_failures = 0 WHERE id = ? AND consecutive_failures > 0`, taskID)
	return err
}

// boceTaskFailuresToPause 是任务连续几次被明确拒绝后自动停下。
const boceTaskFailuresToPause = 3

// errBoceRunExists 表示这个幂等键已经处理过。
var errBoceRunExists = errors.New("这一轮已经处理过")
