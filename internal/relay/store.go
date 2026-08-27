package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
	"github.com/litebox/litebox/internal/relayaddr"
)

// Store 读写 node_relays。
//
// 这张表里没有任何敏感字段:转发规则本身只是"哪个端口通向哪台机器",
// 凭据全部在落地那边。所以不需要 cipher。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const relayColumns = `r.id, r.node_id, n.name, r.engine,
	r.display_name, r.listen_port, r.public_port,
	r.target_kind, r.target_inbound_id, r.target_external_id, r.target_host, r.target_port,
	r.access_tier_id, t.code, t.name, t.level,
	r.sort_order, r.subscription_enabled, r.public_remark, r.enabled,
	r.created_at, r.updated_at,
	CASE r.target_kind
	     WHEN 'ADDRESS' THEN r.target_host || ':' || CAST(r.target_port AS TEXT)
	     ELSE COALESCE(tin.display_name || ' / ' || ti.display_name, tp.display_name, '')
	     END AS target_name,
	CASE r.target_kind
	     WHEN 'INBOUND'  THEN (ti.id IS NOT NULL AND ti.deployed_protocol != '')
	     WHEN 'EXTERNAL' THEN (tp.id IS NOT NULL)
	     WHEN 'ADDRESS'  THEN 1
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
		&r.ID, &r.NodeID, &r.NodeName, &r.Engine,
		&r.DisplayName, &r.ListenPort, &r.PublicPort,
		&r.TargetKind, &targetNodeID, &targetExternalID, &r.TargetHost, &r.TargetPort,
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
	NodeID int64
	// Engine 留空按 nginx。
	Engine      string
	DisplayName string
	ListenPort  int
	// PublicPort 留 0 表示跟随 ListenPort。
	PublicPort       int
	TargetKind       string
	TargetInboundID  int64
	TargetExternalID int64
	// TargetHost / TargetPort 只在 ADDRESS 下用。
	TargetHost string
	TargetPort int
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
	engine, err := ParseEngine(p.Engine)
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
	target := targetSpec{kind: kind, inboundID: p.TargetInboundID, externalID: p.TargetExternalID,
		host: p.TargetHost, port: p.TargetPort}
	if err := s.checkTarget(ctx, engine, &target); err != nil {
		return nil, err
	}
	if err := s.checkPortFree(ctx, p.NodeID, p.ListenPort, 0); err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO node_relays (node_id, engine, display_name, listen_port, public_port,
			target_kind, target_inbound_id, target_external_id, target_host, target_port,
			access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.NodeID, string(engine), name, p.ListenPort, p.PublicPort,
		string(kind), target.inboundArg(), target.externalArg(), target.host, target.port,
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
	// TargetInboundID / TargetExternalID / TargetHost+TargetPort
	// 按规则原有的 TargetKind 取用其中一组。
	TargetInboundID  int64
	TargetExternalID int64
	TargetHost       string
	TargetPort       int
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
	target := targetSpec{kind: cur.TargetKind, inboundID: p.TargetInboundID, externalID: p.TargetExternalID,
		host: p.TargetHost, port: p.TargetPort}
	if err := s.checkTarget(ctx, cur.Engine, &target); err != nil {
		return nil, err
	}
	if p.ListenPort != cur.ListenPort {
		if err := s.checkPortFree(ctx, cur.NodeID, p.ListenPort, id); err != nil {
			return nil, err
		}
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE node_relays SET display_name = ?, listen_port = ?, public_port = ?,
		       target_inbound_id = ?, target_external_id = ?, target_host = ?, target_port = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		name, p.ListenPort, p.PublicPort,
		target.inboundArg(), target.externalArg(), target.host, target.port,
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
func (s *Store) HostIDsTargetingNode(ctx context.Context, targetID int64) ([]HostRef, error) {
	return s.hostRefs(ctx, `r.target_kind = 'INBOUND' AND r.target_inbound_id IN (
		SELECT id FROM node_inbounds WHERE node_id = ? AND deleted_at IS NULL)`, targetID)
}

// HostIDsTargetingInbound 同上,但只针对一个入站。
//
// 改一个入站的公网端口或协议时用它:同机别的入站没变,
// 把指向它们的中转主机也标脏会白 reload 一遍 nginx。
func (s *Store) HostIDsTargetingInbound(ctx context.Context, inboundID int64) ([]HostRef, error) {
	return s.hostRefs(ctx, `r.target_kind = 'INBOUND' AND r.target_inbound_id = ?`, inboundID)
}

// HostIDsTargetingExternal 同上,落地是外部代理。
func (s *Store) HostIDsTargetingExternal(ctx context.Context, targetID int64) ([]HostRef, error) {
	return s.hostRefs(ctx, `r.target_kind = 'EXTERNAL' AND r.target_external_id = ?`, targetID)
}

// hostRefs 带引擎返回:标脏要按引擎分,见 HostRef。
func (s *Store) hostRefs(ctx context.Context, cond string, arg any) ([]HostRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.node_id, r.engine FROM node_relays r
		  WHERE r.deleted_at IS NULL AND `+cond+` ORDER BY r.node_id, r.engine`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := make([]HostRef, 0)
	for rows.Next() {
		var ref HostRef
		if err := rows.Scan(&ref.NodeID, &ref.Engine); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// targetSpec 是一条规则的落地三选一。
//
// 校验通过后,不属于这种落地的那几栏被归零 —— 表里的 CHECK 要求另外两组
// 是空的,而表单可能把上一次选的入口 id 一起带上来。
type targetSpec struct {
	kind       TargetKind
	inboundID  int64
	externalID int64
	host       string
	port       int
}

func (t targetSpec) inboundArg() any {
	if t.kind == TargetInbound {
		return t.inboundID
	}
	return nil
}

func (t targetSpec) externalArg() any {
	if t.kind == TargetExternal {
		return t.externalID
	}
	return nil
}

// checkTarget 确认落地存在且能被这种引擎转发到,并归一化 target。
func (s *Store) checkTarget(ctx context.Context, engine Engine, t *targetSpec) error {
	kind, nodeID, externalID := t.kind, t.inboundID, t.externalID
	if kind != TargetAddress {
		t.host, t.port = "", 0
	}
	switch kind {
	case TargetAddress:
		host, err := relayaddr.NormalizeHost(t.host)
		if err != nil {
			return err
		}
		if err := validatePort(t.port, "落地端口"); err != nil {
			return err
		}
		t.host, t.inboundID, t.externalID = host, 0, 0
		return nil
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
		var name, protocol string
		err := s.db.QueryRowContext(ctx,
			`SELECT display_name, protocol FROM external_proxies
			  WHERE id = ? AND deleted_at IS NULL`, externalID).Scan(&name, &protocol)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("外部代理不存在: id=%d", externalID)
		}
		if err != nil {
			return err
		}
		// nginx stream 这边只渲染 TCP,而 Hysteria2 与 TUIC 是纯 UDP。
		// 「透传不理解协议」这句话只对 TCP 成立 —— 配下去的话 nginx 起得来、
		// 规则也下发得下去,只是用户永远连不上,而面板从头到尾全绿。
		// realm 同时搬 UDP,所以它没有这条限制。
		p := externalproxy.Protocol(protocol)
		if engine == EngineRealm && !p.RelayableByRealm() {
			return fmt.Errorf("外部代理「%s」是 %s,realm 转发不了它", name, p.Label())
		}
		if engine != EngineRealm && !p.RelayableByNginx() {
			return fmt.Errorf("外部代理「%s」是 %s,走的是 UDP,而 nginx 透传只搬 TCP 字节",
				name, p.Label())
		}
		return nil
	}
	return fmt.Errorf("未知的落地去向 %q", kind)
}

// checkPortFree 确认监听端口没有被这台机器上的别的东西占着。
//
// 实现整个搬去了 nodeport 包,与 sing-box 入站那一侧共用一份 ——
// 原来两处各写一遍,而 Mieru 带来的是第三处:它占的是**一整段**端口,
// 于是原来那种逐张表 `listen_port = ?` 的写法在三处都失效了。
// 漏查的后果三处一模一样:nginx 或 mita 其中一个 bind 失败、服务起不来,
// 而问题要到部署的健康检查才暴露,那时配置已经换过去了。
func (s *Store) checkPortFree(ctx context.Context, nodeID int64, port int, excludeRelayID int64) error {
	return nodeport.Free(ctx, s.db, nodeID,
		mieru.PortRange{Start: port, End: port},
		nodeport.Skip{Kind: nodeport.KindRelay, ID: excludeRelayID})
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
