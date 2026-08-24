package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
	"github.com/litebox/litebox/internal/subscription"
)

// Mieru 入口(V13)。
//
// 它与 sing-box 入站(node_inbounds)是**两类东西**,只是回答同一个问题:
// 用户连哪个端口、连上之后去哪。服务端是另一个进程(mita),凭据靠
// `mita apply config` 下发,流量走 Unix socket 上的管理 gRPC ——
// 混进 node_inbounds 会让配置渲染、部署、拨测、流量采集四条路径每一处
// 都要先判断"这一行是不是真的 sing-box 入站",而判断写漏的表现是
// 渲染器把它当成 sing-box 入站写进 config.json,服务起不来,
// 而报错指向的是别的入口。
//
// **端口在这里是一段而不是一个数**:多端口跳跃是 mieru 的主要抗封锁特性。
// 三层端口的含义与 node_inbounds 那三列一一对应,只是每层都成了一对起止。

var (
	ErrMieruInboundNotFound = errors.New("Mieru 入口不存在")
	// ErrMieruNotOnLanding 中转机上不跑 mita。
	//
	// 与 ErrInboundNotOnLanding 分开而不是共用一个:两句话要说的
	// 是不同的事实,合成一句「这台机器不能有入口」会让管理员以为
	// 中转机上什么都放不了,而它恰恰是专门放转发规则的。
	ErrMieruNotOnLanding = errors.New("中转角色的节点不跑 Mieru")
)

// MieruInbound 是一台机器上的一个 Mieru 入口。
type MieruInbound struct {
	ID     int64 `json:"id"`
	NodeID int64 `json:"node_id"`
	// NodeName 与 NodeDisplayName 只给管理页面用,**不进订阅**。
	NodeName        string `json:"node_name"`
	NodeDisplayName string `json:"node_display_name"`

	// DisplayName 是订阅与门户里显示的名字。刻意没有内部名称字段。
	DisplayName string `json:"display_name"`

	// ListenPorts 是 mita 实际监听的那一段;PublicPorts 是 IPv4 条目在订阅里
	// 用的那一段(空表示跟随 ListenPorts);IPv6PublicPorts 是 IPv6 条目用的
	// (空表示跟随 PublicPorts)。
	//
	// **空段要原样留着**,解析放在订阅生成时 —— 写死成当时的监听段之后,
	// 管理员再改监听段,订阅条目会继续停在旧号码上,
	// 而他当初看到的是两个空输入框。
	ListenPorts     mieru.PortRange `json:"-"`
	PublicPorts     mieru.PortRange `json:"-"`
	IPv6PublicPorts mieru.PortRange `json:"-"`

	// 下面这几个是给前端的扁平形式。PortRange 序列化成 {Start,End} 之后
	// 前端要多认一层结构,而它在表单里本来就是两个输入框。
	ListenPortStart     int `json:"listen_port_start"`
	ListenPortEnd       int `json:"listen_port_end"`
	PublicPortStart     int `json:"public_port_start"`
	PublicPortEnd       int `json:"public_port_end"`
	IPv6PublicPortStart int `json:"ipv6_public_port_start"`
	IPv6PublicPortEnd   int `json:"ipv6_public_port_end"`

	IPv6Enabled     bool   `json:"ipv6_enabled"`
	IPv6DisplayName string `json:"ipv6_display_name"`
	// IPv6EntryName 是派生字段:IPv6 条目在订阅里实际显示的名字。
	// 回落只有 subscription.IPv6EntryName 一处实现,前端渲染它、
	// 不自己拼后缀 —— 与 sing-box 入站那一侧一模一样的规矩。
	IPv6EntryName string `json:"ipv6_entry_name"`

	Transport    mieru.Transport    `json:"transport"`
	Multiplexing mieru.Multiplexing `json:"multiplexing"`
	MTU          int                `json:"mtu"`

	// 节点上【当前正在生效】的那几项,只在部署成功时写入。订阅只看它们。
	DeployedTransport       mieru.Transport    `json:"deployed_transport"`
	DeployedMultiplexing    mieru.Multiplexing `json:"deployed_multiplexing"`
	DeployedMTU             int                `json:"deployed_mtu"`
	DeployedListenPortStart int                `json:"deployed_listen_port_start"`
	DeployedListenPortEnd   int                `json:"deployed_listen_port_end"`

	AccessTierID    int64  `json:"access_tier_id"`
	AccessTierCode  string `json:"access_tier_code"`
	AccessTierName  string `json:"access_tier_name"`
	AccessTierLevel int    `json:"access_tier_level"`

	// EgressSocksPort 是 mita 与本机 sing-box 之间那一跳的回环端口。
	// 0 表示这个入口是**直连**的(mita 配置里整个 egress 段不渲染)。
	EgressSocksPort int `json:"egress_socks_port"`
	// 出口去向。含义与 node_inbounds 上那三列一字不差。
	ChainTargetKind       string `json:"chain_target_kind"`
	ChainTargetInboundID  int64  `json:"chain_target_inbound_id"`
	ChainTargetExternalID int64  `json:"chain_target_external_id"`
	// ChainCode 是这条链路在【落地入站】的流量统计里的计数器名。
	// 凭据本身永不出现在 API 响应里。
	ChainCode       string `json:"chain_code"`
	ChainUUID       string `json:"-"`
	ChainSSPassword string `json:"-"`

	SortOrder           int    `json:"sort_order"`
	SubscriptionEnabled bool   `json:"subscription_enabled"`
	PublicRemark        string `json:"public_remark"`
	Enabled             bool   `json:"enabled"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Deployed 表示这个入口确实已经跑在节点上了。
//
// 判据是 deployed_transport 而不是节点级的部署状态:一台部署过很多次的
// 机器上,刚加的这个入口仍然还不存在 —— 与 sing-box 入站看
// deployed_protocol 是同一条道理。
func (m MieruInbound) Deployed() bool { return m.DeployedTransport != "" }

// DeployedListenPorts 是节点上当前真正在听的那一段。
func (m MieruInbound) DeployedListenPorts() mieru.PortRange {
	return mieru.PortRange{Start: m.DeployedListenPortStart, End: m.DeployedListenPortEnd}
}

// EffectivePublicPorts 是 IPv4 条目实际要下发的端口段。
//
// 回落到【已生效的】监听段而不是期望的那一段:改端口到部署成功之间的窗口里,
// 回落到期望值会下发一批还没人监听的号码,而面板一个错都不报。
func (m MieruInbound) EffectivePublicPorts() mieru.PortRange {
	if !m.PublicPorts.Empty() {
		return m.PublicPorts
	}
	if d := m.DeployedListenPorts(); !d.Empty() {
		return d
	}
	return m.ListenPorts
}

// EffectiveIPv6PublicPorts 是 IPv6 条目实际要下发的端口段。
func (m MieruInbound) EffectiveIPv6PublicPorts() mieru.PortRange {
	if !m.IPv6PublicPorts.Empty() {
		return m.IPv6PublicPorts
	}
	return m.EffectivePublicPorts()
}

const mieruColumns = `m.id, m.node_id, n.name, n.display_name,
	m.display_name,
	m.listen_port_start, m.listen_port_end,
	m.public_port_start, m.public_port_end,
	m.ipv6_public_port_start, m.ipv6_public_port_end,
	m.ipv6_enabled, m.ipv6_display_name,
	m.transport, m.multiplexing, m.mtu,
	m.deployed_transport, m.deployed_multiplexing, m.deployed_mtu,
	m.deployed_listen_port_start, m.deployed_listen_port_end,
	m.egress_socks_port, m.chain_target_kind,
	m.chain_target_inbound_id, m.chain_target_external_id,
	m.chain_code, m.chain_uuid_encrypted, m.chain_ss_password_encrypted,
	m.access_tier_id, t.code, t.name, t.level,
	m.sort_order, m.subscription_enabled, m.public_remark, m.enabled,
	m.created_at, m.updated_at`

const mieruFrom = ` FROM node_mieru_inbounds m
	JOIN nodes        n ON n.id = m.node_id
	JOIN access_tiers t ON t.id = m.access_tier_id `

// scanMieruInbound 是 MieruInbound 的【唯一】构造入口。
// 各处自己拼的话,漏掉一个派生字段的表现是「列表里有、点进详情就没了」。
func (s *Store) scanMieruInbound(scan func(dest ...any) error) (*MieruInbound, error) {
	var m MieruInbound
	var chainUUIDEnc, chainSSEnc string
	// 目标列可空:直连的入口这两列是 NULL,扫进 int64 会报错。
	var chainInboundID, chainExternalID sql.NullInt64
	if err := scan(
		&m.ID, &m.NodeID, &m.NodeName, &m.NodeDisplayName,
		&m.DisplayName,
		&m.ListenPortStart, &m.ListenPortEnd,
		&m.PublicPortStart, &m.PublicPortEnd,
		&m.IPv6PublicPortStart, &m.IPv6PublicPortEnd,
		&m.IPv6Enabled, &m.IPv6DisplayName,
		&m.Transport, &m.Multiplexing, &m.MTU,
		&m.DeployedTransport, &m.DeployedMultiplexing, &m.DeployedMTU,
		&m.DeployedListenPortStart, &m.DeployedListenPortEnd,
		&m.EgressSocksPort, &m.ChainTargetKind,
		&chainInboundID, &chainExternalID,
		&m.ChainCode, &chainUUIDEnc, &chainSSEnc,
		&m.AccessTierID, &m.AccessTierCode, &m.AccessTierName, &m.AccessTierLevel,
		&m.SortOrder, &m.SubscriptionEnabled, &m.PublicRemark, &m.Enabled,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.ListenPorts = mieru.PortRange{Start: m.ListenPortStart, End: m.ListenPortEnd}
	m.PublicPorts = mieru.PortRange{Start: m.PublicPortStart, End: m.PublicPortEnd}
	m.IPv6PublicPorts = mieru.PortRange{
		Start: m.IPv6PublicPortStart, End: m.IPv6PublicPortEnd}
	m.ChainTargetInboundID = chainInboundID.Int64
	m.ChainTargetExternalID = chainExternalID.Int64
	for _, f := range []struct {
		enc  string
		dest *string
		what string
	}{
		{chainUUIDEnc, &m.ChainUUID, "链路 UUID"},
		{chainSSEnc, &m.ChainSSPassword, "链路 Shadowsocks 密钥"},
	} {
		if f.enc == "" {
			continue
		}
		var err error
		if *f.dest, err = s.cipher.Decrypt(f.enc); err != nil {
			return nil, fmt.Errorf("解密 Mieru 入口 %d 的%s: %w", m.ID, f.what, err)
		}
	}
	m.IPv6EntryName = subscription.IPv6EntryName(m.DisplayName, m.IPv6DisplayName)
	return &m, nil
}

func (s *Store) queryMieruInbounds(
	ctx context.Context, where string, args ...any,
) ([]*MieruInbound, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mieruColumns+mieruFrom+where+` ORDER BY m.sort_order, m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 绝不返回 nil 切片:Go 的 nil 切片序列化成 JSON null 而不是 [],
	// 而前端把它当数组用。
	list := make([]*MieruInbound, 0)
	for rows.Next() {
		m, err := s.scanMieruInbound(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

// MieruInboundsForNode 返回一台机器上全部未删除的 Mieru 入口(含已停用的)。
func (s *Store) MieruInboundsForNode(ctx context.Context, nodeID int64) ([]*MieruInbound, error) {
	return s.queryMieruInbounds(ctx, `WHERE m.deleted_at IS NULL AND m.node_id = ?`, nodeID)
}

// AllMieruInbounds 一次取出全部机器的 Mieru 入口,供列表页一次性挂到各自的
// 节点上 —— 逐节点查的话,10 台机器就是 10 次往返。
func (s *Store) AllMieruInbounds(ctx context.Context) ([]*MieruInbound, error) {
	return s.queryMieruInbounds(ctx, `WHERE m.deleted_at IS NULL`)
}

// GetMieruInbound 按 id 取一个入口。
func (s *Store) GetMieruInbound(ctx context.Context, id int64) (*MieruInbound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+mieruColumns+mieruFrom+`WHERE m.id = ? AND m.deleted_at IS NULL`, id)
	m, err := s.scanMieruInbound(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMieruInboundNotFound
	}
	return m, err
}

// MieruInboundParams 是新增或编辑一个 Mieru 入口时可填的字段。
//
// 新增与编辑收同一个结构体,理由与 InboundParams 一样:两边各写一份校验的话,
// 某天加了一项只改到一处,表现是"这个值编辑得进去、新建填不进去"。
type MieruInboundParams struct {
	DisplayName string

	// ListenPortStart/End 必填,且**总是一段** —— 单端口就是两者相等。
	// PublicPort* 与 IPv6PublicPort* 两端都留 0 表示跟随上一层。
	ListenPortStart     int
	ListenPortEnd       int
	PublicPortStart     int
	PublicPortEnd       int
	IPv6PublicPortStart int
	IPv6PublicPortEnd   int

	// IPv6Enabled 为 nil:新增时默认开,编辑时保持原值 —— 与 sing-box
	// 入站一致。默认开是因为默认关会让 IPv6 条目从所有人的订阅里
	// 静默消失,而没有人做过什么。
	IPv6Enabled *bool
	// IPv6DisplayName **空串表示「跟随 IPv4 名字」,不是「保持原值」**,
	// 与 InboundParams.IPv6DisplayName 一字不差的约定与理由。
	IPv6DisplayName string

	// Transport 留空按 TCP;Multiplexing 留空按 MULTIPLEXING_LOW;
	// MTU 留 0 表示不设置(用 mieru 的默认值)。
	Transport    string
	Multiplexing string
	MTU          int

	// AccessTierID 留 0:新增时落到普通组,编辑时**保持原值** ——
	// 漏传把 VIP 入口降成普通组等于给全体用户开门,而且不报错。
	AccessTierID int64
	SortOrder    int
	// SubscriptionEnabled / Enabled 为 nil:新增时默认开,编辑时保持原值。
	SubscriptionEnabled *bool
	Enabled             *bool
	PublicRemark        string
}

func (p MieruInboundParams) listenPorts() mieru.PortRange {
	return mieru.PortRange{Start: p.ListenPortStart, End: p.ListenPortEnd}
}

func (p MieruInboundParams) publicPorts() mieru.PortRange {
	return mieru.PortRange{Start: p.PublicPortStart, End: p.PublicPortEnd}
}

func (p MieruInboundParams) ipv6PublicPorts() mieru.PortRange {
	return mieru.PortRange{Start: p.IPv6PublicPortStart, End: p.IPv6PublicPortEnd}
}

// CreateMieruInbound 在一台落地机器上新增一个 Mieru 入口。
func (s *Store) CreateMieruInbound(
	ctx context.Context, nodeID int64, p MieruInboundParams,
) (*MieruInbound, error) {
	n, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if Role(n.Role).IsRelay() {
		return nil, ErrMieruNotOnLanding
	}
	if err := normalizeMieruParams(&p, ""); err != nil {
		return nil, err
	}
	clearMieruIPv6PortsWithoutAddress(n.IPv6Address, &p)
	if p.AccessTierID == 0 {
		p.AccessTierID = access.TierNormalID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 校验走事务而不是 s.db:连接池上限是 1,在打开的事务里再拿 *sql.DB
	// 查一次会直接死锁。
	if err := access.ValidateTier(ctx, tx, p.AccessTierID); err != nil {
		return nil, err
	}
	// 端口检查必须与插入在【同一个事务】里:分开做的话,两次并发创建
	// 会双双通过检查,然后双双插入 —— 而区间重叠没有唯一索引兜底。
	if err := nodeport.Free(ctx, tx, nodeID, p.listenPorts(), nodeport.Skip{}); err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO node_mieru_inbounds (
			node_id, display_name,
			listen_port_start, listen_port_end,
			public_port_start, public_port_end,
			ipv6_public_port_start, ipv6_public_port_end,
			ipv6_enabled, ipv6_display_name,
			transport, multiplexing, mtu,
			access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, p.DisplayName,
		p.ListenPortStart, p.ListenPortEnd,
		p.PublicPortStart, p.PublicPortEnd,
		p.IPv6PublicPortStart, p.IPv6PublicPortEnd,
		boolOr(p.IPv6Enabled, true), p.IPv6DisplayName,
		p.Transport, p.Multiplexing, p.MTU,
		p.AccessTierID, p.SortOrder, boolOr(p.SubscriptionEnabled, true),
		p.PublicRemark, boolOr(p.Enabled, true),
		now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetMieruInbound(ctx, id)
}

// UpdateMieruInbound 全量提交一个入口的可改字段。
//
// 全量而不是增量补丁,与 UpdateInbound / relay.Update 一致:补丁式的接口
// 要为每一个字段各定义一次"没传是什么意思",而那套约定迟早写漏一项。
func (s *Store) UpdateMieruInbound(
	ctx context.Context, id int64, p MieruInboundParams,
) (*MieruInbound, InboundEffect, error) {
	effect := InboundEffect{Changes: []string{}}
	cur, err := s.GetMieruInbound(ctx, id)
	if err != nil {
		return nil, effect, err
	}
	if err := normalizeMieruParams(&p, cur.DisplayName); err != nil {
		return nil, effect, err
	}
	n, err := s.Get(ctx, cur.NodeID)
	if err != nil {
		return nil, effect, err
	}
	clearMieruIPv6PortsWithoutAddress(n.IPv6Address, &p)
	if p.AccessTierID == 0 {
		// 留 0 表示**保持原值**,不是落回普通组 —— 漏传把 VIP 入口
		// 降成普通组等于给全体用户开门,而且不报错。
		p.AccessTierID = cur.AccessTierID
	} else if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
		return nil, effect, err
	}
	ipv6Enabled := boolOr(p.IPv6Enabled, cur.IPv6Enabled)
	subEnabled := boolOr(p.SubscriptionEnabled, cur.SubscriptionEnabled)
	enabled := boolOr(p.Enabled, cur.Enabled)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, effect, err
	}
	defer tx.Rollback()

	if err := nodeport.Free(ctx, tx, cur.NodeID, p.listenPorts(),
		nodeport.Skip{Kind: nodeport.KindMieru, ID: id}); err != nil {
		return nil, effect, err
	}

	track := func(label string, changed bool, from, to any) {
		if changed {
			effect.Changes = append(effect.Changes,
				fmt.Sprintf("%s %v → %v", label, from, to))
		}
	}
	track("展示名称", cur.DisplayName != p.DisplayName, cur.DisplayName, p.DisplayName)
	track("监听端口段", cur.ListenPorts != p.listenPorts(),
		cur.ListenPorts, p.listenPorts())
	track("公网端口段", cur.PublicPorts != p.publicPorts(),
		rangeLabel(cur.PublicPorts, "监听端口段"), rangeLabel(p.publicPorts(), "监听端口段"))
	track("IPv6 公网端口段", cur.IPv6PublicPorts != p.ipv6PublicPorts(),
		rangeLabel(cur.IPv6PublicPorts, "公网端口段"),
		rangeLabel(p.ipv6PublicPorts(), "公网端口段"))
	track("IPv6 条目", cur.IPv6Enabled != ipv6Enabled,
		onOffLabel(cur.IPv6Enabled), onOffLabel(ipv6Enabled))
	track("IPv6 条目名称", cur.IPv6DisplayName != p.IPv6DisplayName,
		nameFollowLabel(cur.IPv6DisplayName), nameFollowLabel(p.IPv6DisplayName))
	track("传输层", string(cur.Transport) != p.Transport, cur.Transport, p.Transport)
	track("多路复用", string(cur.Multiplexing) != p.Multiplexing,
		cur.Multiplexing, p.Multiplexing)
	track("MTU", cur.MTU != p.MTU, mtuLabel(cur.MTU), mtuLabel(p.MTU))
	track("访问等级", cur.AccessTierID != p.AccessTierID, cur.AccessTierID, p.AccessTierID)
	track("排序", cur.SortOrder != p.SortOrder, cur.SortOrder, p.SortOrder)
	track("下发订阅", cur.SubscriptionEnabled != subEnabled,
		onOffLabel(cur.SubscriptionEnabled), onOffLabel(subEnabled))
	track("启用", cur.Enabled != enabled, onOffLabel(cur.Enabled), onOffLabel(enabled))
	track("公开备注", cur.PublicRemark != p.PublicRemark, cur.PublicRemark, p.PublicRemark)

	// 进 mita 配置的字段才要重新部署。
	//
	// 公网端口段、IPv6 那三项、展示名、排序与订阅开关都只影响订阅内容 ——
	// 为它们重启 mita 会把这台机器上全部在线连接踢掉一次,换不来任何东西。
	effect.NeedsDeploy = cur.ListenPorts != p.listenPorts() ||
		string(cur.Transport) != p.Transport ||
		string(cur.Multiplexing) != p.Multiplexing ||
		cur.MTU != p.MTU ||
		cur.Enabled != enabled
	// 等级变更是**安全问题**,与 sing-box 入站一样必须自动标脏重新部署:
	// 等级调高后,被移出的用户凭据还留在 mita 的用户列表里,
	// 拖多久就多能用多久 —— 那是权限没有真正收回。
	effect.TierChanged = cur.AccessTierID != p.AccessTierID
	effect.SubscriptionChanged = cur.DisplayName != p.DisplayName ||
		cur.PublicPorts != p.publicPorts() ||
		cur.IPv6PublicPorts != p.ipv6PublicPorts() ||
		cur.IPv6Enabled != ipv6Enabled ||
		cur.IPv6DisplayName != p.IPv6DisplayName ||
		cur.SubscriptionEnabled != subEnabled ||
		cur.SortOrder != p.SortOrder

	if _, err := tx.ExecContext(ctx, `
		UPDATE node_mieru_inbounds
		   SET display_name = ?,
		       listen_port_start = ?, listen_port_end = ?,
		       public_port_start = ?, public_port_end = ?,
		       ipv6_public_port_start = ?, ipv6_public_port_end = ?,
		       ipv6_enabled = ?, ipv6_display_name = ?,
		       transport = ?, multiplexing = ?, mtu = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.DisplayName,
		p.ListenPortStart, p.ListenPortEnd,
		p.PublicPortStart, p.PublicPortEnd,
		p.IPv6PublicPortStart, p.IPv6PublicPortEnd,
		ipv6Enabled, p.IPv6DisplayName,
		p.Transport, p.Multiplexing, p.MTU,
		p.AccessTierID, p.SortOrder, subEnabled, p.PublicRemark, enabled,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, effect, err
	}
	if err := tx.Commit(); err != nil {
		return nil, effect, err
	}
	updated, err := s.GetMieruInbound(ctx, id)
	return updated, effect, err
}

// DeleteMieruInbound 软删除一个 Mieru 入口。
//
// 软删除与其余三类对象一致。这里没有 tag 那种"必须一直占着名字"的理由,
// 但仍然软删:mita 上的用户列表要等下一次部署才会真的少掉这个入口,
// 而在那之前它还在跑 —— 物理删除会让那段时间里的流量归属查不到来源。
func (s *Store) DeleteMieruInbound(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_mieru_inbounds SET deleted_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMieruInboundNotFound
	}
	return nil
}

// normalizeMieruParams 归一化并校验参数。新增与编辑共用。
func normalizeMieruParams(p *MieruInboundParams, fallbackName string) error {
	if p.DisplayName == "" {
		p.DisplayName = fallbackName
	}
	if err := validateEntryName(p.DisplayName, "展示名称"); err != nil {
		return err
	}
	if p.IPv6DisplayName != "" {
		if err := validateEntryName(p.IPv6DisplayName, "IPv6 条目名称"); err != nil {
			return err
		}
		// 与 sing-box 入站那一条一字不差:两条同名的节点在订阅里挑不出
		// 哪条走 IPv6,而它们是同一个入口的两个地址 ——
		// 「一条通、一条不通」正是最需要分辨的时候。
		if p.IPv6DisplayName == p.DisplayName {
			return errors.New("IPv6 条目名称不能与入口名称相同,否则订阅里会出现两条同名节点")
		}
	}

	listen := p.listenPorts()
	if listen.Empty() {
		return errors.New("监听端口段必须填写")
	}
	if err := listen.Validate("监听端口段"); err != nil {
		return err
	}
	if err := p.publicPorts().Validate("公网端口段"); err != nil {
		return err
	}
	if err := p.ipv6PublicPorts().Validate("IPv6 公网端口段"); err != nil {
		return err
	}

	transport, err := mieru.ParseTransport(p.Transport)
	if err != nil {
		return err
	}
	p.Transport = string(transport)
	multiplexing, err := mieru.ParseMultiplexing(p.Multiplexing)
	if err != nil {
		return err
	}
	p.Multiplexing = string(multiplexing)
	if err := mieru.ValidateMTU(p.MTU); err != nil {
		return err
	}
	if len([]rune(p.PublicRemark)) > 128 {
		return errors.New("公开备注不能超过 128 个字符")
	}
	return nil
}

// clearMieruIPv6PortsWithoutAddress 在机器没有 IPv6 地址时把 IPv6 端口段归零。
//
// 与 sing-box 入站那一侧的 clearIPv6PortWithoutAddress 同一条规矩:留着它,
// 下次这台机器填上 IPv6 会静默套用一个几个月前的端口段,而那些号码未必
// 还转发着 —— 用户拿到的是连不上的条目,面板一个错都不报。
func clearMieruIPv6PortsWithoutAddress(ipv6Address string, p *MieruInboundParams) {
	if ipv6Address == "" {
		p.IPv6PublicPortStart, p.IPv6PublicPortEnd = 0, 0
	}
}

// rangeLabel 把空段写成「跟随某某」而不是空串 ——
// 审计里出现「公网端口段  → 40000-40010」看不出左边是什么意思。
func rangeLabel(r mieru.PortRange, what string) string {
	if r.Empty() {
		return "跟随" + what
	}
	return r.String()
}

// mtuLabel 把 0 写成「默认」。审计里的「MTU 0 → 1400」会被读成
// "本来没有 MTU",而实际含义是"本来用的是 mieru 自己的默认值"。
func mtuLabel(mtu int) string {
	if mtu == 0 {
		return "默认"
	}
	return strconv.Itoa(mtu)
}

// MarkMieruDeployed 记下这个入口在节点上【当前生效】的那几项。
//
// 只在下发成功之后调。订阅只看这几列 —— 改配置到下发成功之间的窗口里
// (部署失败的话是永远),按期望值渲染会让用户拉到一份与节点上不符的参数,
// 而数据库、节点、面板三方都是"对的",只有订阅站在中间说了假话。
// ClearMieruDeployed 把「节点上已生效」的那几列清空。
//
// 卸载之后必须调:节点上已经没有这个实例了,而订阅只看 deployed_*
// —— 不清的话它会继续留在所有人的订阅里,而客户端连过去无人应答。
// 与 MarkDeployed 清理离场入站是同一条道理。
func (s *Store) ClearMieruDeployed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE node_mieru_inbounds
		   SET deployed_transport = '',
		       deployed_multiplexing = '',
		       deployed_mtu = 0,
		       deployed_listen_port_start = 0,
		       deployed_listen_port_end = 0,
		       updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) MarkMieruDeployed(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE node_mieru_inbounds
		   SET deployed_transport = transport,
		       deployed_multiplexing = multiplexing,
		       deployed_mtu = mtu,
		       deployed_listen_port_start = listen_port_start,
		       deployed_listen_port_end = listen_port_end,
		       updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMieruInboundNotFound
	}
	return nil
}
