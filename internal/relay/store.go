package relay

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
	"github.com/litebox/litebox/internal/relayaddr"

	"github.com/litebox/litebox/internal/database"
)

// Store 读写 node_relays。
//
// 这张表里没有任何敏感字段:转发规则本身只是"哪个端口通向哪台机器",
// 凭据全部在落地那边。所以不需要 cipher。
type Store struct {
	db   *sql.DB
	hook ChangeHook
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const relayColumns = `r.id, r.node_id, n.name, r.engine,
	r.display_name, r.listen_port, r.public_port,
	r.target_kind, r.target_inbound_id, r.target_external_id, r.target_host, r.target_port,
	r.target_relay_id, r.target_endpoint_id, r.target_addr_mode, r.target_legacy_host, r.target_legacy_port,
	r.access_tier_id, t.code, t.name, t.level,
	r.sort_order, r.subscription_enabled, r.public_remark, r.enabled,
	r.created_at, r.updated_at,
	CASE r.target_kind
	     WHEN 'ADDRESS' THEN r.target_host || ':' || CAST(r.target_port AS TEXT)
	     WHEN 'RELAY'   THEN COALESCE(trn.display_name || ' / ' || tr.display_name, '')
	     ELSE COALESCE(tin.display_name || ' / ' || ti.display_name, tp.display_name, '')
	     END AS target_name,
	CASE r.target_kind
	     WHEN 'INBOUND'  THEN CASE WHEN ti.id IS NOT NULL AND ti.deployed_protocol <> '' THEN 1 ELSE 0 END
	     WHEN 'EXTERNAL' THEN CASE WHEN tp.id IS NOT NULL THEN 1 ELSE 0 END
	     WHEN 'ADDRESS'  THEN 1
	     WHEN 'RELAY'    THEN CASE WHEN tr.id IS NOT NULL AND tr.enabled = 1 THEN 1 ELSE 0 END
	     ELSE 0 END AS target_ready`

// target_ready 每个分支都写成整数(复核批次三在 PG 上撞到):原来 INBOUND / EXTERNAL 两支是布尔表达式、
// 另外两支是 1 / 0,SQLite 不在乎,PG 报 「CASE types integer and boolean cannot be matched」——
// PG 部署上任何读转发规则的接口(列表、详情、新增之后读回)都会失败。

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
	LEFT JOIN external_proxies tp ON tp.id = r.target_external_id AND tp.deleted_at IS NULL
	LEFT JOIN node_relays tr ON tr.id = r.target_relay_id AND tr.deleted_at IS NULL
	LEFT JOIN nodes trn ON trn.id = tr.node_id AND trn.deleted_at IS NULL `

func scanRelay(scan func(dest ...any) error) (*Relay, error) {
	var r Relay
	var targetNodeID, targetExternalID, targetRelayID, targetEndpointID sql.NullInt64
	var addrMode string
	err := scan(
		&r.ID, &r.NodeID, &r.NodeName, &r.Engine,
		&r.DisplayName, &r.ListenPort, &r.PublicPort,
		&r.TargetKind, &targetNodeID, &targetExternalID, &r.TargetHost, &r.TargetPort,
		&targetRelayID, &targetEndpointID, &addrMode, &r.TargetAddr.LegacyHost, &r.TargetAddr.LegacyPort,
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
	r.TargetRelayID = targetRelayID.Int64
	r.TargetAddr.Mode = chaingraph.AddrMode(addrMode)
	r.TargetAddr.EndpointID = targetEndpointID.Int64
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
	// TargetRelayID 只在 RELAY 下用。
	TargetRelayID int64
	// TargetEndpointID 是落地为入站或转发规则时选的地址条目;0 只在目标恰好有零条或一条时可以。
	TargetEndpointID int64
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
	pr, err := s.prepare(ctx, p)
	if err != nil {
		return nil, err
	}
	id, err := insertRelay(ctx, s.db, pr)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// prepared 是校验通过、归一化之后的一条新规则。
type prepared struct {
	p      CreateParams
	name   string
	kind   TargetKind
	engine Engine
	target targetSpec
}

// prepare 做 Create 的全部校验,不写库。批量创建与单条创建共用这一份(一个中转配多个落地)。
func (s *Store) prepare(ctx context.Context, p CreateParams) (*prepared, error) {
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
		relayID: p.TargetRelayID, endpointID: p.TargetEndpointID, host: p.TargetHost, port: p.TargetPort}
	if err := s.checkTarget(ctx, 0, p.NodeID, engine, &target, nil); err != nil {
		return nil, err
	}
	if err := s.checkPortFree(ctx, p.NodeID, p.ListenPort, 0); err != nil {
		return nil, err
	}
	return &prepared{p: p, name: name, kind: kind, engine: engine, target: target}, nil
}

type execer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertRelay(ctx context.Context, q execer, pr *prepared) (int64, error) {
	p := pr.p
	now := time.Now().UTC().Format(time.RFC3339)
	id, err := database.InsertID(ctx, q, `
		INSERT INTO node_relays (node_id, engine, display_name, listen_port, public_port,
			target_kind, target_inbound_id, target_external_id, target_host, target_port,
			target_relay_id, target_endpoint_id, target_addr_mode, target_legacy_host, target_legacy_port,
			access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.NodeID, string(pr.engine), pr.name, p.ListenPort, p.PublicPort,
		string(pr.kind), pr.target.inboundArg(), pr.target.externalArg(), pr.target.host, pr.target.port,
		pr.target.relayArg(), pr.target.endpointArg(), string(pr.target.addr.Mode),
		pr.target.addr.LegacyHost, pr.target.addr.LegacyPort,
		p.AccessTierID, p.SortOrder, boolOr(p.SubscriptionEnabled, true),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, true), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w:该主机上已有规则监听 %d", ErrPortConflict, p.ListenPort)
		}
		return 0, err
	}
	return id, nil
}

// UpdateParams 是可修改的字段。
//
// **NodeID 不在里面**:换一条规则所属的机器等于换成另一条线路,而用户手上那份订阅里的地址不会跟着变。
// 落地种类可以在入站 / 外部代理 / 另一条转发之间换(v23.3,画布与统一选择器里的「设置转发目标」)——
// 那与在同一种落地里换一个目标是同一件事:订阅地址是这条规则自己的,协议参数跟着认证终点走。
// 与「指定地址」之间不能互换:那一种不进订阅、不拨测,表单上连访问等级都没有,换过去等于换了一条性质不同的线路。
type UpdateParams struct {
	// TargetKind 留空表示保持原来的落地种类。
	TargetKind  string
	DisplayName string
	ListenPort  int
	PublicPort  int
	// TargetInboundID / TargetExternalID / TargetHost+TargetPort
	// 按规则原有的 TargetKind 取用其中一组。
	TargetInboundID  int64
	TargetExternalID int64
	TargetRelayID    int64
	// TargetEndpointID 为 0 且落地没变时沿用原来的地址选择(包括待确认的兼容快照)。
	TargetEndpointID int64
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
	kind := cur.TargetKind
	if strings.TrimSpace(p.TargetKind) != "" {
		if kind, err = ParseTargetKind(strings.TrimSpace(p.TargetKind)); err != nil {
			return nil, err
		}
		if kind != cur.TargetKind && (kind == TargetAddress || cur.TargetKind == TargetAddress) {
			return nil, errors.New("「指定地址」与其它落地种类之间不能互换,请新建一条规则")
		}
	}
	target := targetSpec{kind: kind, inboundID: p.TargetInboundID, externalID: p.TargetExternalID,
		relayID: p.TargetRelayID, endpointID: p.TargetEndpointID, host: p.TargetHost, port: p.TargetPort}
	if err := s.checkTarget(ctx, id, cur.NodeID, cur.Engine, &target, cur); err != nil {
		return nil, err
	}
	if p.ListenPort != cur.ListenPort {
		if err := s.checkPortFree(ctx, cur.NodeID, p.ListenPort, id); err != nil {
			return nil, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state any
	if s.hook != nil {
		if state, err = s.hook.BeforeRelayChange(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE node_relays SET display_name = ?, listen_port = ?, public_port = ?, target_kind = ?,
		       target_inbound_id = ?, target_external_id = ?, target_host = ?, target_port = ?,
		       target_relay_id = ?, target_endpoint_id = ?, target_addr_mode = ?,
		       target_legacy_host = ?, target_legacy_port = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		name, p.ListenPort, p.PublicPort, string(kind),
		target.inboundArg(), target.externalArg(), target.host, target.port,
		target.relayArg(), target.endpointArg(), string(target.addr.Mode),
		target.addr.LegacyHost, target.addr.LegacyPort,
		tierID, p.SortOrder, boolOr(p.SubscriptionEnabled, cur.SubscriptionEnabled),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, cur.Enabled),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w:该主机上已有规则监听 %d", ErrPortConflict, p.ListenPort)
		}
		return nil, err
	}
	if s.hook != nil {
		if err := s.hook.AfterRelayChange(ctx, tx, id, state); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ChangeHook 在一条转发规则被修改的同一个事务里被调用(v23.3):改之前一次、改之后一次。
//
// 由 node.Service 实现:一条转发换了去向,经它过去的那些代理入口的认证终点就跟着换了 ——
// 新终点要补凭据、旧终点要暂时保留凭据直到发起方切过去,而这几件事必须与改动一起提交(G01),
// 不能留一个「保存完成之后再通知一次」的丢失窗口。relay 包不认识这些概念,所以只留一个钩子。
type ChangeHook interface {
	BeforeRelayChange(ctx context.Context, tx *sql.Tx, relayID int64) (any, error)
	AfterRelayChange(ctx context.Context, tx *sql.Tx, relayID int64, state any) error
}

// SetChangeHook 装上钩子。
func (s *Store) SetChangeHook(h ChangeHook) { s.hook = h }

// Delete 软删除一条规则。
//
// 软删除而不是物理删除,与用户、节点一致:物理删掉之后,
// 万一是误删,等级、排序、备注与展示名全部要重配,而用户手上的订阅
// 早就已经变了 —— 那时候能翻出这一行来看看原来配的是什么很有用。
func (s *Store) Delete(ctx context.Context, id int64) error {
	// 还有入口把它当下一跳:拒绝,并说清是谁(v23.3)。删掉之后那些入口的配置要么渲染不出来,
	// 要么被悄悄改成直连 —— 两种都不能发生,管理员先给它们重选目标或解除关系。
	refs, err := chaingraph.Referrers(ctx, s.db, chaingraph.Ref{Kind: chaingraph.KindNginx, ID: id})
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ReferencedError{Referrers: refs}
	}
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

// ReferencedError 表示还有入口把这条规则(或它的某条地址)当下一跳,删不得。
type ReferencedError struct {
	Referrers []chaingraph.Referrer
}

func (e *ReferencedError) Error() string {
	parts := make([]string, 0, len(e.Referrers))
	for _, r := range e.Referrers {
		parts = append(parts, r.Describe())
	}
	return "还有入口把它当下一跳,先给它们重选目标或解除关系:" + strings.Join(parts, ";")
}

// targetSpec 是一条规则的落地四选一。
//
// 校验通过后,不属于这种落地的那几栏被归零 —— 表里的 CHECK 要求另外几组
// 是空的,而表单可能把上一次选的入口 id 一起带上来。
type targetSpec struct {
	kind       TargetKind
	inboundID  int64
	externalID int64
	relayID    int64
	endpointID int64
	host       string
	port       int
	// addr 是校验后定下来的地址选择(落地为入站或转发规则时)。
	addr chaingraph.AddrSel
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

func (t targetSpec) relayArg() any {
	if t.kind == TargetRelay {
		return t.relayID
	}
	return nil
}

func (t targetSpec) endpointArg() any {
	if t.addr.Mode == chaingraph.AddrEndpoint {
		return t.addr.EndpointID
	}
	return nil
}

// keepAddr 在落地没变、又没选新地址时沿用原来的选择(包括待确认的兼容快照)。
func keepAddr(cur *Relay, t *targetSpec) *chaingraph.AddrSel {
	if cur == nil || t.endpointID != 0 || cur.TargetKind != t.kind {
		return nil
	}
	if (t.kind == TargetInbound && cur.TargetInboundID == t.inboundID) ||
		(t.kind == TargetRelay && cur.TargetRelayID == t.relayID) {
		k := cur.TargetAddr
		return &k
	}
	return nil
}

// checkTarget 确认落地存在且能被这种引擎转发到,并归一化 target。selfID 是正在修改的规则(新增时为 0)。
func (s *Store) checkTarget(ctx context.Context, selfID, nodeID int64, engine Engine, t *targetSpec, cur *Relay) error {
	kind := t.kind
	keep := keepAddr(cur, t)
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
		t.host, t.inboundID, t.externalID, t.relayID, t.addr = host, 0, 0, 0, chaingraph.AddrSel{}
		return nil
	case TargetInbound:
		t.externalID, t.relayID = 0, 0
		if t.inboundID == 0 {
			return errors.New("请选择落地入口")
		}
		// 落地是一个【入站】。中转机上根本没有入站行,所以"落地不能是中转角色"这条限制由数据本身保证。
		ref := chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: t.inboundID}
		if _, err := chaingraph.Load(ctx, s.db, ref); err != nil {
			if errors.Is(err, chaingraph.ErrNotFound) {
				return fmt.Errorf("落地入口不存在: id=%d", t.inboundID)
			}
			return err
		}
		addr, err := chaingraph.PickAddr(ctx, s.db, ref, t.endpointID, keep)
		if err != nil {
			return err
		}
		t.addr = addr
		return s.checkNoForwardLoop(ctx, selfID, &chaingraph.Hop{To: ref, Addr: addr})
	case TargetExternal:
		t.inboundID, t.relayID, t.addr = 0, 0, chaingraph.AddrSel{}
		if t.externalID == 0 {
			return errors.New("请选择落地的外部代理")
		}
		var name, protocol string
		err := s.db.QueryRowContext(ctx,
			`SELECT display_name, protocol FROM external_proxies
			  WHERE id = ? AND deleted_at IS NULL`, t.externalID).Scan(&name, &protocol)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("外部代理不存在: id=%d", t.externalID)
		}
		if err != nil {
			return err
		}
		return checkExternalForEngines(name, externalproxy.Protocol(protocol), []Engine{engine})
	case TargetRelay:
		t.inboundID, t.externalID = 0, 0
		if t.relayID == 0 {
			return errors.New("请选择要转发到的转发规则")
		}
		if t.relayID == selfID {
			return errors.New("转发规则不能指向它自己")
		}
		entry, err := chaingraph.Load(ctx, s.db, chaingraph.Ref{Kind: chaingraph.KindNginx, ID: t.relayID})
		if errors.Is(err, chaingraph.ErrNotFound) {
			return fmt.Errorf("转发规则不存在: id=%d", t.relayID)
		}
		if err != nil {
			return err
		}
		if entry.NodeID == nodeID {
			// 同机两条规则串起来只是在本机多绕一圈,对外的落脚点一个字节都没变。
			return errors.New("不能转发到同一台机器上的另一条转发规则")
		}
		addr, err := chaingraph.PickAddr(ctx, s.db, entry.Ref, t.endpointID, keep)
		if err != nil {
			return err
		}
		t.addr = addr
		hop := &chaingraph.Hop{To: entry.Ref, Addr: addr}
		if err := s.checkNoForwardLoop(ctx, selfID, hop); err != nil {
			return err
		}
		// 级联到最后的外部代理时,沿途每一种引擎都要能搬它(纯 UDP 的协议不能经 nginx)。
		path, err := chaingraph.Walk(ctx, s.db, hop)
		if err != nil {
			return err
		}
		engines := []Engine{engine}
		for _, st := range path.Transparent {
			if st.Entry.Kind == chaingraph.KindRealm {
				engines = append(engines, EngineRealm)
			} else {
				engines = append(engines, EngineNginx)
			}
		}
		if path.Auth != nil && path.Auth.Ref.Kind == chaingraph.KindExternal {
			return checkExternalForEngines(path.Auth.Entry.Name, externalproxy.Protocol(path.Auth.Entry.Protocol), engines)
		}
		return nil
	}
	return fmt.Errorf("未知的落地去向 %q", kind)
}

// checkExternalForEngines 确认一路上的每一种转发引擎都能搬这个外部代理。
//
// nginx 的 stream 配置里 TCP 与 UDP 都会渲染,但纯 UDP 的协议(Hysteria2 / TUIC)经 nginx 转发从没实测过、
// 部署拨测也测不了它(节点上的探测客户端不含 QUIC)—— 照样放行的话 nginx 起得来、规则也下发得下去,
// 用户连不连得上没有任何一层验证过。所以这条路先不收;realm 这一侧实测过搬 UDP,它没有这条限制。
func checkExternalForEngines(name string, p externalproxy.Protocol, engines []Engine) error {
	for _, e := range engines {
		if e == EngineRealm && !p.RelayableByRealm() {
			return fmt.Errorf("外部代理「%s」是 %s,realm 转发不了它", name, p.Label())
		}
		if e != EngineRealm && !p.RelayableByNginx() {
			return fmt.Errorf("外部代理「%s」是 %s(纯 UDP),经 nginx 转发没有实测过、拨测也测不了,不能经 nginx 转发;改用 realm",
				name, p.Label())
		}
	}
	return nil
}

// checkNoForwardLoop 确认把 selfID 这条规则指向 hop 之后不成环(纯转发环,或经代理入口绕回来的混合环)。
func (s *Store) checkNoForwardLoop(ctx context.Context, selfID int64, hop *chaingraph.Hop) error {
	path, err := chaingraph.Walk(ctx, s.db, hop)
	if err != nil {
		return err
	}
	loops := path.Loop
	if selfID != 0 {
		for _, st := range path.Steps {
			if st.Ref.Kind.IsRelay() && st.Ref.ID == selfID {
				loops = true
			}
		}
	}
	if loops {
		return errors.New("这条转发规则的去向成环了:沿下一跳走下去又回到了路径上已经出现过的入口")
	}
	return nil
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
	// SQLite 认错误文本,PG 认 SQLSTATE 23505 —— 判据只在 database.IsUniqueViolation 一处。
	return database.IsUniqueViolation(err)
}

// ValidateTarget 只读地问「这条规则(selfID,新增时为 0)指向这个落地行不行」,与保存走同一个 checkTarget。
// 统一的目标选择器拿它给候选标「不可选」并写明原因 —— 不能让人到保存时才发现一个确定不可用的候选。
func (s *Store) ValidateTarget(ctx context.Context, selfID, nodeID int64, engine Engine,
	kind TargetKind, targetID, endpointID int64) error {
	t := targetSpec{kind: kind, endpointID: endpointID}
	switch kind {
	case TargetInbound:
		t.inboundID = targetID
	case TargetExternal:
		t.externalID = targetID
	case TargetRelay:
		t.relayID = targetID
	default:
		return fmt.Errorf("候选只能是入站、外部代理或转发规则,收到 %q", kind)
	}
	return s.checkTarget(ctx, selfID, nodeID, engine, &t, nil)
}
