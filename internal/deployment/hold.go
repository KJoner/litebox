package deployment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 「暂停自动下发」(R03)。
//
// 恢复主控库不会回滚节点:恢复的是一份旧快照,而节点上跑着的可能是更新的配置 —— 快照之后撤掉的人,
// 节点上已经没有他的凭据了。旧快照一启动,协调器把库里欠着的下发捞出来、巡检把停了的服务重新下发,
// 都会把那份旧配置(连同已经撤掉的凭据)推回节点上去。
//
// 所以恢复脚本在启动主控之前把这个开关打开:打开期间协调器只排队、不下发,巡检只报告、不恢复;
// 管理员核对过配置版本、撤权与计量之后,在界面上解除。管理员自己点的「部署」不受影响 ——
// 那是他核对过之后的显式动作。

// HoldSettingKey 是 system_settings 里那一行的键。值为空表示没有暂停。
const HoldSettingKey = "deploy_hold"

// HoldInfo 是暂停的原因与开始时间。
type HoldInfo struct {
	Held   bool   `json:"held"`
	Reason string `json:"reason"`
	Since  string `json:"since"`
}

// ReadHold 读暂停状态。读不到(表不在、库连不上)按没有暂停算 —— 这个开关只在恢复流程里打开,
// 读失败时把整个面板的自动下发停掉,换来的是另一种看不见的故障。
func ReadHold(ctx context.Context, db *sql.DB) HoldInfo {
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = ?`, HoldSettingKey).Scan(&raw); err != nil || raw == "" {
		return HoldInfo{}
	}
	var h HoldInfo
	if json.Unmarshal([]byte(raw), &h) != nil {
		return HoldInfo{Held: true, Reason: raw}
	}
	h.Held = true
	return h
}

// SetHold 打开暂停。
func SetHold(ctx context.Context, db *sql.DB, reason string) error {
	if reason == "" {
		return errors.New("要写明为什么暂停")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(HoldInfo{Held: true, Reason: reason, Since: now})
	_, err := db.ExecContext(ctx, `INSERT INTO system_settings (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		HoldSettingKey, string(b), now)
	return err
}

// ClearHold 解除暂停。
func ClearHold(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE system_settings SET value = '', updated_at = ? WHERE key = ?`,
		time.Now().UTC().Format(time.RFC3339), HoldSettingKey)
	return err
}
