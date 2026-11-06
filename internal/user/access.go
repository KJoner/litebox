package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
)

// 直接入口授权(VNext AUTH01–04,迁移 0049)。
//
// 规则只在视图里(user_effective_inbounds / user_effective_mieru_inbounds 的第三条分支),
// 这里只管「谁被直接授权了哪些入口」这张表与用户的模式。预览也不另写一遍规则:
// 在一个事务里把改动写进去、读视图、再回滚。

// 入口授权模式。
const (
	AccessInherit    = "INHERIT"
	AccessDirectOnly = "DIRECT_ONLY"
)

// 入口种类:两张入口表的 id 会撞,所以「种类 + id」才是入口的身份。
const (
	EntrySingBox = "SINGBOX"
	EntryMieru   = "MIERU"
)

// EntryRef 指向一个入口。
type EntryRef struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

func (e EntryRef) key() string { return fmt.Sprintf("%s:%d", e.Kind, e.ID) }

// EntryInfo 是一个入口连同它所在的机器(给界面列名字用)。
type EntryInfo struct {
	EntryRef
	Name     string `json:"name"`
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	Enabled  bool   `json:"enabled"`
	// Restricted 是「仅指定用户」。
	Restricted bool `json:"restricted"`
}

// AccessView 是一个用户的入口授权现状。
type AccessView struct {
	Mode   string      `json:"access_mode"`
	Grants []EntryInfo `json:"grants"`
	// Effective 是此刻他能用的全部入口(三条规则合起来,硬条件在 Go 侧另判)。
	Effective []EntryInfo `json:"effective"`
}

// AccessDiff 是一次改动前后归属集合的差。
type AccessDiff struct {
	Added   []EntryInfo `json:"added"`
	Removed []EntryInfo `json:"removed"`
	// AffectedNodes 是变更前后两个集合涉及的机器的并集 —— 撤权必须取并集,
	// 只看变更后的会漏掉「被移出」的那台机器,而它恰恰要重新下发才能删掉凭据(AUTH02)。
	AffectedNodes []int64 `json:"affected_node_ids"`
}

var (
	// ErrBadAccessMode 表示模式不是 INHERIT / DIRECT_ONLY。
	ErrBadAccessMode = errors.New("授权模式只能是 INHERIT 或 DIRECT_ONLY")
	// ErrEntryNotFound 表示授权指向的入口不存在或已删除。
	ErrEntryNotFound = errors.New("授权的入口不存在或已删除")
	// ErrBadEntryKind 表示入口种类不是 SINGBOX / MIERU(转发与外部代理不走这张表)。
	ErrBadEntryKind = errors.New("入口种类只能是 SINGBOX 或 MIERU")
)

// Access 返回一个用户的授权现状。
func (s *Store) Access(ctx context.Context, userID int64) (*AccessView, error) {
	var mode string
	if err := s.db.QueryRowContext(ctx,
		`SELECT access_mode FROM proxy_users WHERE id = ? AND deleted_at IS NULL`, userID).Scan(&mode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	grants, err := s.grantInfos(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	eff, err := effectiveEntries(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	return &AccessView{Mode: mode, Grants: grants, Effective: eff}, nil
}

// PreviewAccess 回答「这么改之后,他多了哪些入口、少了哪些、涉及哪几台机器」,不落库。
// 规则只在视图里:事务里写进去、读视图、回滚 —— 预览与真改用的是同一份判据。
func (s *Store) PreviewAccess(ctx context.Context, userID int64, mode string, grants []EntryRef) (*AccessDiff, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := effectiveEntries(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := writeAccess(ctx, tx, userID, mode, grants); err != nil {
		return nil, err
	}
	after, err := effectiveEntries(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	return diffEntries(before, after), nil
}

// SetAccess 换掉一个用户的模式与直接授权集合(整体替换),返回前后差异。
func (s *Store) SetAccess(ctx context.Context, userID int64, mode string, grants []EntryRef) (*AccessDiff, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := effectiveEntries(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := writeAccess(ctx, tx, userID, mode, grants); err != nil {
		return nil, err
	}
	after, err := effectiveEntries(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return diffEntries(before, after), nil
}

// EntryAccess 是一个入口反过来看的授权:开没开「仅指定用户」、直接授权了谁。
type EntryAccess struct {
	Restricted bool          `json:"restricted"`
	Users      []GrantedUser `json:"users"`
}

// GrantedUser 是被直接授权的一个用户。
type GrantedUser struct {
	ID          int64  `json:"id"`
	UserCode    string `json:"user_code"`
	DisplayName string `json:"display_name"`
}

// EntryAccessOf 返回一个入口的反向授权。
func (s *Store) EntryAccessOf(ctx context.Context, e EntryRef) (*EntryAccess, int64, error) {
	nodeID, restricted, err := entryState(ctx, s.db, e)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.user_code, u.display_name
		  FROM user_inbound_grants g
		  JOIN proxy_users u ON u.id = g.proxy_user_id AND u.deleted_at IS NULL
		 WHERE g.entry_kind = ? AND g.entry_id = ?
		 ORDER BY u.user_code`, e.Kind, e.ID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := &EntryAccess{Restricted: restricted, Users: []GrantedUser{}}
	for rows.Next() {
		var g GrantedUser
		if err := rows.Scan(&g.ID, &g.UserCode, &g.DisplayName); err != nil {
			return nil, 0, err
		}
		out.Users = append(out.Users, g)
	}
	return out, nodeID, rows.Err()
}

// SetEntryAccess 换掉一个入口的「仅指定用户」开关与被直接授权的用户集合,返回它所在的机器(要重新下发)。
func (s *Store) SetEntryAccess(ctx context.Context, e EntryRef, restricted bool, userIDs []int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	nodeID, _, err := entryState(ctx, tx, e)
	if err != nil {
		return 0, err
	}
	table := "node_inbounds"
	if e.Kind == EntryMieru {
		table = "node_mieru_inbounds"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET restricted = ?, updated_at = ? WHERE id = ?`,
		boolInt(restricted), time.Now().UTC().Format(time.RFC3339), e.ID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_inbound_grants WHERE entry_kind = ? AND entry_id = ?`, e.Kind, e.ID); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seen := map[int64]bool{}
	for _, uid := range userIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_users WHERE id = ? AND deleted_at IS NULL`, uid).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("%w(用户 id=%d)", ErrNotFound, uid)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at)
			VALUES (?, ?, ?, ?)`, uid, e.Kind, e.ID, now); err != nil {
			return 0, err
		}
	}
	return nodeID, tx.Commit()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func writeAccess(ctx context.Context, q queryer, userID int64, mode string, grants []EntryRef) error {
	if mode != AccessInherit && mode != AccessDirectOnly {
		return ErrBadAccessMode
	}
	res, err := q.ExecContext(ctx, `UPDATE proxy_users SET access_mode = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		mode, time.Now().UTC().Format(time.RFC3339), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM user_inbound_grants WHERE proxy_user_id = ?`, userID); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seen := map[string]bool{}
	for _, g := range grants {
		if g.Kind != EntrySingBox && g.Kind != EntryMieru {
			return fmt.Errorf("%w,收到 %q", ErrBadEntryKind, g.Kind)
		}
		if seen[g.key()] {
			continue
		}
		seen[g.key()] = true
		if _, _, err := entryState(ctx, q, g); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at)
			VALUES (?, ?, ?, ?)`, userID, g.Kind, g.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// entryState 返回入口所在的机器与「仅指定用户」开关;入口不存在或已删除时 ErrEntryNotFound。
func entryState(ctx context.Context, q queryer, e EntryRef) (int64, bool, error) {
	table := ""
	switch e.Kind {
	case EntrySingBox:
		table = "node_inbounds"
	case EntryMieru:
		table = "node_mieru_inbounds"
	default:
		return 0, false, fmt.Errorf("%w,收到 %q", ErrBadEntryKind, e.Kind)
	}
	var nodeID int64
	var restricted int
	err := q.QueryRowContext(ctx, `SELECT node_id, restricted FROM `+table+` WHERE id = ? AND deleted_at IS NULL`, e.ID).
		Scan(&nodeID, &restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("%w(%s)", ErrEntryNotFound, e.key())
	}
	return nodeID, restricted == 1, err
}

func (s *Store) grantInfos(ctx context.Context, q queryer, userID int64) ([]EntryInfo, error) {
	return entryInfos(ctx, q, `
		SELECT 'SINGBOX', i.id, i.display_name, i.node_id, n.name, i.enabled, i.restricted
		  FROM user_inbound_grants g
		  JOIN node_inbounds i ON i.id = g.entry_id AND i.deleted_at IS NULL
		  JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
		 WHERE g.proxy_user_id = ? AND g.entry_kind = 'SINGBOX'
		UNION ALL
		SELECT 'MIERU', m.id, m.display_name, m.node_id, n.name, m.enabled, m.restricted
		  FROM user_inbound_grants g
		  JOIN node_mieru_inbounds m ON m.id = g.entry_id AND m.deleted_at IS NULL
		  JOIN nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
		 WHERE g.proxy_user_id = ? AND g.entry_kind = 'MIERU'`, userID, userID)
}

func effectiveEntries(ctx context.Context, q queryer, userID int64) ([]EntryInfo, error) {
	return entryInfos(ctx, q, `
		SELECT 'SINGBOX', i.id, i.display_name, i.node_id, n.name, i.enabled, i.restricted
		  FROM `+access.EffectiveInboundsView+` e
		  JOIN node_inbounds i ON i.id = e.inbound_id
		  JOIN nodes n ON n.id = i.node_id
		 WHERE e.proxy_user_id = ?
		UNION ALL
		SELECT 'MIERU', m.id, m.display_name, m.node_id, n.name, m.enabled, m.restricted
		  FROM user_effective_mieru_inbounds e
		  JOIN node_mieru_inbounds m ON m.id = e.mieru_inbound_id
		  JOIN nodes n ON n.id = m.node_id
		 WHERE e.proxy_user_id = ?`, userID, userID)
}

func entryInfos(ctx context.Context, q queryer, query string, args ...any) ([]EntryInfo, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EntryInfo{}
	for rows.Next() {
		var e EntryInfo
		var enabled, restricted int
		if err := rows.Scan(&e.Kind, &e.ID, &e.Name, &e.NodeID, &e.NodeName, &enabled, &restricted); err != nil {
			return nil, err
		}
		e.Enabled, e.Restricted = enabled == 1, restricted == 1
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeName != out[j].NodeName {
			return out[i].NodeName < out[j].NodeName
		}
		return strings.Compare(out[i].key(), out[j].key()) < 0
	})
	return out, rows.Err()
}

func diffEntries(before, after []EntryInfo) *AccessDiff {
	in := func(list []EntryInfo) map[string]EntryInfo {
		m := make(map[string]EntryInfo, len(list))
		for _, e := range list {
			m[e.key()] = e
		}
		return m
	}
	b, a := in(before), in(after)
	d := &AccessDiff{Added: []EntryInfo{}, Removed: []EntryInfo{}, AffectedNodes: []int64{}}
	nodes := map[int64]bool{}
	for k, e := range a {
		if _, ok := b[k]; !ok {
			d.Added = append(d.Added, e)
		}
		nodes[e.NodeID] = true
	}
	for k, e := range b {
		if _, ok := a[k]; !ok {
			d.Removed = append(d.Removed, e)
		}
		nodes[e.NodeID] = true
	}
	for id := range nodes {
		d.AffectedNodes = append(d.AffectedNodes, id)
	}
	sort.Slice(d.AffectedNodes, func(i, j int) bool { return d.AffectedNodes[i] < d.AffectedNodes[j] })
	sort.Slice(d.Added, func(i, j int) bool { return d.Added[i].key() < d.Added[j].key() })
	sort.Slice(d.Removed, func(i, j int) bool { return d.Removed[i].key() < d.Removed[j].key() })
	return d
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
