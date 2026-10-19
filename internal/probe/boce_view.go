package probe

import (
	"context"
	"fmt"
	"time"
)

// BoceView 是汇总 Boce 序列时要的外部事实,一次读齐(卡片、节点详情都用它)。
type BoceView struct {
	Config     BoceConfig
	State      *BoceState
	Tasks      map[int64]*BoceTask
	Vantages   map[int]BoceVantage
	NodePaused map[int64]bool
}

// LoadBoceView 读汇总 Boce 序列要的东西。
func (s *Store) LoadBoceView(ctx context.Context, cfg BoceConfig) (*BoceView, error) {
	v := &BoceView{Config: cfg, Tasks: map[int64]*BoceTask{}, NodePaused: map[int64]bool{}}
	var err error
	if v.State, err = s.GetBoceState(ctx); err != nil {
		return nil, err
	}
	tasks, err := listBoceTasksQ(ctx, s.db, ``)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		v.Tasks[tasks[i].ID] = &tasks[i]
	}
	if v.Vantages, err = s.boceVantageMap(ctx); err != nil {
		return nil, err
	}
	ns, err := s.nodeSettingsMap(ctx)
	if err != nil {
		return nil, err
	}
	for id, n := range ns {
		v.NodePaused[id] = n.BocePaused
	}
	return v, nil
}

// BoceSeriesInfo 是一条 Boce 序列在卡片 / 详情上要多给的东西:属于哪个任务、哪个拨测点。
// 地区与运营商只有接口实际返回的说法。
type BoceSeriesInfo struct {
	TaskID      int64  `json:"task_id"`
	TaskName    string `json:"task_name"`
	VantageID   int    `json:"vantage_id"`
	VantageName string `json:"vantage_name"`
	ISPName     string `json:"isp_name"`
	Area        string `json:"area"`
	// Missing 为真表示这个拨测点已经从 Boce 节点列表里消失(不会被自动换成别的付费拨测点)。
	Missing   bool   `json:"missing"`
	Schedule  string `json:"schedule"`
	NextRunAt string `json:"next_run_at"`
	// AutoOn 为真表示这条序列此刻会被自动检测(任务开着、没暂停、全局可用)。
	AutoOn bool `json:"auto_on"`
}

func (v *BoceView) info(t Target) *BoceSeriesInfo {
	in := &BoceSeriesInfo{TaskID: t.BoceTaskID, VantageID: t.VantageID, VantageName: t.Label}
	if vv, ok := v.Vantages[t.VantageID]; ok {
		in.VantageName, in.ISPName, in.Area, in.Missing = vv.Label(), vv.ISPName, vv.Area, vv.MissingSince != ""
	}
	if task := v.Tasks[t.BoceTaskID]; task != nil {
		in.TaskName = task.Name
		in.Schedule = task.ScheduleSpec.Describe()
		in.AutoOn = v.AutoReason(task, t.NodeID) == ""
		if in.AutoOn {
			in.NextRunAt = task.NextRunAt
		}
	}
	return in
}

// AutoReason 回答这个任务此刻为什么不会被自动检测;空串 = 会。
func (v *BoceView) AutoReason(task *BoceTask, nodeID int64) string {
	switch {
	case task == nil:
		return "任务已删除"
	case task.Archived():
		return "任务已归档(历史保留)"
	}
	if r := v.Config.BlockReason(v.State); r != "" {
		return r
	}
	switch {
	case task.AutoPausedReason != "":
		return "任务已自动停下:" + task.AutoPausedReason
	case v.NodePaused[nodeID]:
		return "这台节点的 Boce 自动检测已暂停"
	case !task.Enabled:
		return "自动检测没有开启(可以手动检测)"
	}
	return ""
}

// interval 是这条序列的参考间隔(点阵每格的宽度、数据过期的判据)。
func (v *BoceView) interval(t Target) time.Duration {
	if task := v.Tasks[t.BoceTaskID]; task != nil {
		if sc, err := task.Schedule(); err == nil {
			return sc.TypicalInterval()
		}
	}
	return time.Hour
}

// stale 回答「本该有一轮结果却没有」:按时间表找上一个早于 now - 5 分钟的时间槽,
// 那之后没有任何样本就是过期了。夜里关闭时段不会被判成过期 —— 那时本来就不测。
func (v *BoceView) stale(t Target, lastAt, now time.Time) (bool, time.Time) {
	task := v.Tasks[t.BoceTaskID]
	if task == nil || v.AutoReason(task, t.NodeID) != "" {
		return false, time.Time{}
	}
	sc, err := task.Schedule()
	if err != nil {
		return false, time.Time{}
	}
	prev := sc.Prev(now.Add(-5 * time.Minute))
	if prev.IsZero() {
		return false, time.Time{}
	}
	since := prev
	if created, err := time.Parse(time.RFC3339, task.UpdatedAt); err == nil && created.After(prev) {
		return false, time.Time{} // 刚改过时间表 / 刚建:还没轮到
	}
	if lastAt.IsZero() || lastAt.Before(since.Add(-time.Minute)) {
		return true, prev
	}
	return false, time.Time{}
}

// boceState 是 Boce 序列在「最近一次结果」之前要先判的几种状态;第二个返回值为假表示交给通用判据。
func (v *BoceView) state(t Target, sm Summary, sc SummaryContext, lastAt, now time.Time) (string, string, bool) {
	task := v.Tasks[t.BoceTaskID]
	switch {
	case task == nil:
		return StatePaused, "任务已删除", true
	case task.Archived():
		return StatePaused, "任务已归档(历史保留)", true
	case !t.Enabled:
		return StatePaused, "这个拨测点已从任务里移除(历史保留)", true
	case sc.NodeDisabled != nil && sc.NodeDisabled(t.NodeID):
		return StateDisabled, "节点已停用,不检测", true
	case t.RefState == "INVALID":
		return StateInvalid, "被测入口已失效:" + t.RefError, true
	}
	if reason := v.AutoReason(task, t.NodeID); reason != "" {
		if sm.Last != nil {
			reason += ";上次结果:" + sm.Last.Label
		}
		return StatePaused, reason, true
	}
	if lastAt.IsZero() {
		if task.NextRunAt != "" {
			if n, err := time.Parse(time.RFC3339, task.NextRunAt); err == nil {
				return StatePending, "等待第一次检测(" + n.In(v.taskLoc(task)).Format("01-02 15:04") + ")", true
			}
		}
		return StatePending, "等待第一次检测", true
	}
	if sm.Stale {
		if ok, prev := v.stale(t, lastAt, now); ok {
			return StateStale, fmt.Sprintf("数据过期:%s 那一轮没有结果", prev.In(v.taskLoc(task)).Format("01-02 15:04")), true
		}
	}
	return "", "", false
}

func (v *BoceView) taskLoc(t *BoceTask) *time.Location {
	if sc, err := t.Schedule(); err == nil && sc.Loc != nil {
		return sc.Loc
	}
	return time.UTC
}
