package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
)

// Store 读写 node_relays。
//
// 这张表里没有任何敏感字段:转发规则本身只是"哪个端口通向哪台机器",
// 凭据全部在落地那边。所以不需要 cipher。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const relayColumns = `r.id, r.node_id, n.name,
	r.display_name, r.listen_port, r.public_port,
	r.target_kind, r.target_inbound_id, r.target_external_id,
	r.access_tier_id, t.code, t.name, t.level,
	r.sort_order, r.subscription_enabled, r.public_remark, r.enabled,
	r.created_at, r.updated_at,
	COALESCE(tin.display_name || ' / ' || ti.display_name, tp.display_name, '') AS target_name,
	CASE r.target_kind
	     WHEN 'INBOUND'  THEN (ti.id IS NOT NULL AND ti.deployed_protocol != '')
	     WHEN 'EXTERNAL' THEN (tp.id IS NOT NULL)
	     ELSE 0 END AS target_ready`

// relayFrom 固定带上等级表与落地表。
//
// 落地用 LEFT JOIN:落地被删掉之后这条规则仍然要在管理页面上看得见 ——
// 查不出来的话管理员只会看到 nginx 上莫名少了一个 server 块,
// 而面板上没有任何东西能解释它去哪了。可见性由视图负责(那边是 INNER JOIN)。
const relayFrom = ` FROM node_relays r
	JOIN nodes n ON n.id = r.node_id
	JOIN access_tiers t ON t.id = r.access_tier_id
	LEFT JOIN node_inbounds ti ON ti.id = r.target_inbound_id AND ti.deleted_at IS NULL
	LEFT JOIN nodes tin ON tin.id = ti.node_id AND tin.deleted_at IS NULL
	LEFT JOIN external_proxies tp ON tp.id = r.target_external_id AND tp.deleted_at IS NULL `

func scanRelay(scan func(dest ...any) error) (*Relay, error) {
	var r Relay
	var targetNodeID, targetExternalID sql.NullInt64
	err := scan(
		&r.ID, &r.NodeID, &r.NodeName,
		&r.DisplayName, &r.ListenPort, &r.PublicPort,
		&r.TargetKind, &targetNodeID, &targetExternalID,
		&r.AccessTierID, &r.AccessTierCode, &r.AccessTierName, &r.AccessTierLevel,
		&r.SortOrder, &r.SubscriptionEnabled, &r.PublicRemark, &r.Enabled,
		&r.CreatedAt, &r.UpdatedAt,
		&r.TargetName, &r.TargetReady,
	)
	if err != nil {
		return nil, err
	}
	r.TargetInboundID = targetNodeID.Int64
	r.TargetExternalID = targetExternalID.Int64
	return &r, nil
}

func (s *Store) queryList(ctx context.Context, where string, args ...any) ([]*Relay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+relayColumns+relayFrom+where+` ORDER BY r.sort_order, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 不返回 nil 切片:Go 的 nil 切片序列化成 JSON null 而不是 [],
	// 而前端把它当数组用。
	list := make([]*Relay, 0)
	for rows.Next() {
		r, err := scanRelay(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// List 返回全部未删除的转发规则。
func (s *Store) List(ctx context.Context) ([]*Relay, error) {
	return s.queryList(ctx, `WHERE r.deleted_at IS NULL`)
}

// ListByNode 返回某台中转主机上的全部规则(含已停用的)。
func (s *Store) ListByNode(ctx context.Context, nodeID int64) ([]*Relay, error) {
	return s.queryList(ctx, `WHERE r.deleted_at IS NULL AND r.node_id = ?`, nodeID)
}

// EnabledForNode 返回要渲染进 nginx 配置的规则。
//
// 只看 enabled,不看 subscription_enabled —— 后者是"这条线路进不进订阅",
// 与"nginx 上要不要监听这个端口"是两件事。把订阅下架的线路一并从 nginx 上
// 撤掉,会立刻踢断还在用旧订阅的人;而下架的本意通常只是"别再发给新用户"。
func (s *Store) EnabledForNode(ctx context.Context, nodeID int64) ([]*Relay, error) {
	return s.queryList(ctx,
		`WHERE r.deleted_at IS NULL AND r.node_id = ? AND r.enabled = 1`, nodeID)
}

// Get 按 ID 读取。
func (s *Store) Get(ctx context.Context, id int64) (*Relay, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+relayColumns+relayFrom+`WHERE r.id = ? AND r.deleted_at IS NULL`, id)
	r, err := scanRelay(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// CreateParams 是新增一条转发规则的参数。
type CreateParams struct {
	NodeID      int64
	DisplayName string
	ListenPort  int
	// PublicPort 留 0 表示跟随 ListenPort。
	PublicPort       int
	TargetKind       string
	TargetInboundID  int64
	TargetExternalID int64
	// AccessTierID 留 0 表示普通组。
	AccessTierID        int64
	SortOrder           int
	SubscriptionEnabled *bool
	PublicRemark        string
	Enabled             *bool
}

func (s *Store) Create(ctx context.Context, p CreateParams) (*Relay, error) {
	name, err := validateName(p.DisplayName)
	if err != nil {
		return nil, err
	}
	kind, err := ParseTargetKind(p.TargetKind)
	if err != nil {
		return nil, err
	}
	if err := validatePort(p.ListenPort, "监听端口"); err != nil {
		return nil, err
	}
	if p.PublicPort != 0 {
		if err := validatePort(p.PublicPort, "公网端口"); err != nil {
			return nil, err
		}
	}
	if p.AccessTierID == 0 {
		p.AccessTierID = access.TierNormalID
	}
	// 迁移里没给 access_tier_id 写外键,这道校验就是唯一的拦截点 ——
	// 指向不存在的等级会让这条线路从 user_effective_relays 里整个消失
	// (视图是 INNER JOIN),表现为"规则在,但谁都用不到"。
	if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
		return nil, err
	}
	if err := s.checkTarget(ctx, kind, p.TargetInboundID, p.TargetExternalID); err != nil {
		return nil, err
	}
	if err := s.checkPortFree(ctx, p.NodeID, p.ListenPort, 0); err != nil {
		return nil, err
	}

	targetNode, targetExternal := targetArgs(kind, p.TargetInboundID, p.TargetExternalID)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO node_relays (node_id, display_name, listen_port, public_port,
			target_kind, target_inbound_id, target_external_id,
			access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.NodeID, name, p.ListenPort, p.PublicPort,
		string(kind), targetNode, targetExternal,
		p.AccessTierID, p.SortOrder, boolOr(p.SubscriptionEnabled, true),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, true), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w:该主机上已有规则监听 %d", ErrPortConflict, p.ListenPort)
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// UpdateParams 是可修改的字段。
//
// **NodeID 与 TargetKind 不在里面**:换一条规则所属的机器或落地种类,
// 等于换成另一条线路,而用户手上那份订阅里的地址不会跟着变。
// 要那么做就删掉重建,让它显式一点。
type UpdateParams struct {
	DisplayName string
	ListenPort  int
	PublicPort  int
	// TargetInboundID / TargetExternalID 按规则原有的 TargetKind 取用其中一个。
	TargetInboundID  int64
	TargetExternalID int64
	// AccessTierID 为 0 表示保持原值,不回落到普通组 ——
	// 漏传把 VIP 线路降成普通组等于给全体用户开门,而且不报错。
	AccessTierID int64
	SortOrder    int
	// SubscriptionEnabled / Enabled 为 nil 表示保持原值,同上。
	SubscriptionEnabled *bool
	Enabled             *bool
	PublicRemark        string
}

func (s *Store) Update(ctx context.Context, id int64, p UpdateParams) (*Relay, error) {
	cur, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	name, err := validateName(p.DisplayName)
	if err != nil {
		return nil, err
	}
	if err := validatePort(p.ListenPort, "监听端口"); err != nil {
		return nil, err
	}
	if p.PublicPort != 0 {
		if err := validatePort(p.PublicPort, "公网端口"); err != nil {
			return nil, err
		}
	}
	tierID := p.AccessTierID
	if tierID == 0 {
		tierID = cur.AccessTierID
	} else if err := access.NewStore(s.db).Validate(ctx, tierID); err != nil {
		return nil, err
	}
	if err := s.checkTarget(ctx, cur.TargetKind, p.TargetInboundID, p.TargetExternalID); err != nil {
		return nil, err
	}
	if p.ListenPort != cur.ListenPort {
		if err := s.checkPortFree(ctx, cur.NodeID, p.ListenPort, id); err != nil {
			return nil, err
		}
	}

	targetNode, targetExternal := targetArgs(cur.TargetKind, p.TargetInboundID, p.TargetExternalID)
	_, err = s.db.ExecContext(ctx, `
		UPDATE node_relays SET display_name = ?, listen_port = ?, public_port = ?,
		       target_inbound_id = ?, target_external_id = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		name, p.ListenPort, p.PublicPort, targetNode, targetExternal,
		tierID, p.SortOrder, boolOr(p.SubscriptionEnabled, cur.SubscriptionEnabled),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, cur.Enabled),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w:该主机上已有规则监听 %d", ErrPortConflict, p.ListenPort)
		}
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete 软删除一条规则。
//
// 软删除而不是物理删除,与用户、节点一致:物理删掉之后,
// 万一是误删,等级、排序、备注与展示名全部要重配,而用户手上的订阅
// 早就已经变了 —— 那时候能翻出这一行来看看原来配的是什么很有用。
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_relays SET deleted_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HostIDsTargetingNode 返回哪些中转主机上有规则指向这个落地节点。
//
// 用于跨节点脏标记:B 的地址或公网端口一变,指向它的 A 上那份 nginx 配置
// 就过时了 —— 不传播的话 A 会把流量转到一个没人监听的端口上,
// 而面板上两台机器都显示正常。
func (s *Store) HostIDsTargetingNode(ctx context.Context, targetID int64) ([]int64, error) {
	return s.hostIDs(ctx, `r.target_kind = 'INBOUND' AND r.target_inbound_id IN (
		SELECT id FROM node_inbounds WHERE node_id = ? AND deleted_at IS NULL)`, targetID)
}

// HostIDsTargetingInbound 同上,但只针对一个入站。
//
// 改一个入站的公网端口或协议时用它:同机别的入站没变,
// 把指向它们的中转主机也标脏会白 reload 一遍 nginx。
func (s *Store) HostIDsTargetingInbound(ctx context.Context, inboundID int64) ([]int64, error) {
	return s.hostIDs(ctx, `r.target_kind = 'INBOUND' AND r.target_inbound_id = ?`, inboundID)
}

// HostIDsTargetingExternal 同上,落地是外部代理。
func (s *Store) HostIDsTargetingExternal(ctx context.Context, targetID int64) ([]int64, error) {
	return s.hostIDs(ctx, `r.target_kind = 'EXTERNAL' AND r.target_external_id = ?`, targetID)
}

func (s *Store) hostIDs(ctx context.Context, cond string, arg any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.node_id FROM node_relays r
		  WHERE r.deleted_at IS NULL AND `+cond+` ORDER BY r.node_id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// checkTarget 确认落地存在且能被转发到。
func (s *Store) checkTarget(ctx context.Context, kind TargetKind, nodeID, externalID int64) error {
	switch kind {
	case TargetInbound:
		if nodeID == 0 {
			return errors.New("请选择落地入口")
		}
		// 落地是一个【入站】。中转机上根本没有入站行,所以"落地不能是
		// 中转角色"这条限制现在由数据本身保证 —— 面板不为 A→B→C 三跳
		// 做任何编排,也就不假装支持它。
		var count int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM node_inbounds i
			  JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
			 WHERE i.id = ? AND i.deleted_at IS NULL`, nodeID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("落地入口不存在: id=%d", nodeID)
		}
		return nil
	case TargetExternal:
		if externalID == 0 {
			return errors.New("请选择落地的外部代理")
		}
		var count int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_proxies WHERE id = ? AND deleted_at IS NULL`,
			externalID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("外部代理不存在: id=%d", externalID)
		}
		return nil
	}
	return fmt.Errorf("未知的落地去向 %q", kind)
}

// checkPortFree 确认监听端口没有被这台机器上的别的东西占着。
//
// 除了别的转发规则(靠唯一索引兜底),还要避开这台机器自己的 sing-box:
// LANDING 角色的 A 同时跑着入站与 V2Ray API。撞上去的表现是 nginx 起不来,
// 而那要等到部署的健康检查才发现 —— 到那时前一份配置已经被换掉了。
func (s *Store) checkPortFree(ctx context.Context, nodeID int64, port int, excludeRelayID int64) error {
	var role string
	var apiPort int
	err := s.db.QueryRowContext(ctx,
		`SELECT role, api_port FROM nodes WHERE id = ? AND deleted_at IS NULL`,
		nodeID).Scan(&role, &apiPort)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("中转主机不存在: id=%d", nodeID)
	}
	if err != nil {
		return err
	}
	var count int
	if role != "RELAY" {
		if port == apiPort {
			return fmt.Errorf("%w:%d 是这台机器上 V2Ray API 的端口", ErrPortConflict, port)
		}
		// 同机的 sing-box 入站【逐个】查,不是只查一个。
		// 少查一个的后果是那个入站 bind 失败、整个 sing-box 起不来,
		// 而问题要到部署的健康检查才暴露。
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM node_inbounds
			  WHERE node_id = ? AND listen_port = ? AND deleted_at IS NULL`,
			nodeID, port).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w:%d 是这台机器上一个 sing-box 入站的监听端口",
				ErrPortConflict, port)
		}
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_relays
		  WHERE node_id = ? AND listen_port = ? AND deleted_at IS NULL AND id != ?`,
		nodeID, port, excludeRelayID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w:该主机上已有规则监听 %d", ErrPortConflict, port)
	}
	return nil
}

func targetArgs(kind TargetKind, nodeID, externalID int64) (any, any) {
	if kind == TargetInbound {
		return nodeID, nil
	}
	return nil, externalID
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
