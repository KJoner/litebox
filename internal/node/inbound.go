package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
	"github.com/litebox/litebox/internal/singbox"
	// 只为 IPv6 条目名那一条回落规则(subscription.IPv6EntryName)。方向看着
	// 是反的,但反过来更糟:把规则搬进 node 包,subscription 就要 import
	// 整个 node(含 deployment/sshx/nginx),而公开订阅路由刻意不依赖它们。
	"github.com/litebox/litebox/internal/subscription"
)

// 一台落地机器上的 sing-box 入站(V8)。
//
// V8 之前这些字段直接挂在 nodes 上,因为那时一台机器只有一个入站。
// 迁移 0019 把它们整体搬进 node_inbounds,存量节点各变成一行,
// nodes 上的原列就此冻结 —— 详见那份迁移的开头。
//
// 流量归属没有因此变复杂:V2Ray 的用户计数器名里没有入站维度,
// 同一个用户在同一台机器上的流量本来就是合并的,而那正是
// traffic_ledger 需要的口径。代价是入站级的【用户】流量永远拿不到。

var (
	ErrInboundNotFound = errors.New("入站不存在")
	// ErrInboundPortConflict 监听端口与这台机器上已有的东西冲突。
	//
	// **它就是 nodeport.ErrConflict 本身**,relay.ErrPortConflict 同理 ——
	// 检测已经统一到那一个包里,而三个包各留一个自己的哨兵会让
	// errors.Is 在跨包传递时静默失配:上层拿 node 的哨兵去判一个由
	// relay 那条路径产生的冲突,判不出来,于是 400 变成 500。
	// 名字保留是为了不动十几个调用点。
	//
	// 检测到就拒绝保存,**不自动挪端口** —— 自动避让会让用户手上那份订阅
	// 静默失效:客户端还连着旧端口,而那里已经没人监听了。
	ErrInboundPortConflict = nodeport.ErrConflict
	// ErrInboundNotOnLanding 中转机上没有 sing-box,谈不上入站。
	ErrInboundNotOnLanding = errors.New("中转角色的节点没有 sing-box 入站")
)

// Inbound 是一台机器上的一个 sing-box 入站。
//
// 敏感字段在这里已是明文,加解密由 Store 在读写边界完成。
type Inbound struct {
	ID     int64 `json:"id"`
	NodeID int64 `json:"node_id"`
	// NodeName 与 NodeDisplayName 只给管理页面用,**不进订阅**。
	NodeName        string `json:"node_name"`
	NodeDisplayName string `json:"node_display_name"`

	// Tag 是 sing-box 配置里的 inbound.tag,建库时分配、一经分配不可更改。
	// 它同时是入站级流量计数器的名字,改了会让历史曲线在那一刻断掉。
	Tag string `json:"tag"`
	// DisplayName 是订阅与门户里显示的名字。刻意没有内部名称字段。
	DisplayName string `json:"display_name"`

	// Protocol 是期望协议;DeployedProtocol 是节点上当前生效的那个。
	// 两者不一致就是"改了协议还没部署"—— 订阅与门户一律看后者。
	Protocol         singbox.Protocol `json:"protocol"`
	SSMethod         string           `json:"ss_method"`
	SSPassword       string           `json:"-"` // 入站级 PSK,永不出现在 API 响应中
	DeployedProtocol singbox.Protocol `json:"deployed_protocol"`
	DeployedSSMethod string           `json:"deployed_ss_method"`

	// Snell 专有(V14)。SnellVersion 为 0 表示这不是 Snell 入站。
	//
	// SnellPSK 打 json:"-" 与 SSPassword 同级,但它比那一份更敏感:
	// **Snell 的 psk 原样出现在每一个用户的客户端配置里**,而 SS 的节点 PSK
	// 只作为拼接的前半段。它泄漏出去等于把这个入口交出去 —— 见迁移 0030
	// 里关于空用户列表的那一段。
	SnellVersion  int    `json:"snell_version"`
	SnellPSK      string `json:"-"`
	SnellObfsMode string `json:"snell_obfs_mode"`
	// SnellObfsHost 只进客户端配置,不进节点配置(服务端根本没有这个字段)。
	// 所以改它既不用部署,也没有 deployed_ 镜像。
	SnellObfsHost string `json:"snell_obfs_host"`
	SnellV6Mode   string `json:"snell_v6_mode"`
	// SnellSharedPSK 为真时这个入口**不下发逐用户凭据**,所有人共用 psk。
	//
	// 唯一的理由是让 Clash / mihomo 能用 —— 它们的 snell proxy 没有
	// userkey 那一栏。代价:没有分用户流量、撤销一个人要换 psk
	// (所有人一起断)、用户额度对它不生效。
	SnellSharedPSK        bool   `json:"snell_shared_psk"`
	DeployedSnellVersion  int    `json:"deployed_snell_version"`
	DeployedSnellObfsMode string `json:"deployed_snell_obfs_mode"`
	DeployedSnellV6Mode   string `json:"deployed_snell_v6_mode"`
	// DeployedSnellSharedPSK 是节点上【当前正在生效】的那一份。
	// 订阅只看它 —— 改了模式还没下发时,节点上跑的仍是旧的那一种。
	DeployedSnellSharedPSK bool `json:"deployed_snell_shared_psk"`

	// ListenPort 是 sing-box 在这台机器上实际监听的端口;
	// PublicPort 是客户端连接的公网端口,0 表示跟随 ListenPort;
	// IPv6PublicPort 是 IPv6 条目用的公网端口,0 表示跟随 PublicPort。
	//
	// **0 要原样留着**,解析放在订阅生成时 —— 写死成当时的值之后,
	// 管理员再改监听端口,订阅条目会继续停在旧端口上,
	// 而他当初看到的是一个空输入框。
	ListenPort     int `json:"listen_port"`
	PublicPort     int `json:"public_port"`
	IPv6PublicPort int `json:"ipv6_public_port"`

	// IPv6Enabled 决定这个入口在订阅里要不要多出一条 IPv6 条目。
	// 机器没填 ipv6_address 时它没有意义 —— 展开的前提是两者都成立。
	IPv6Enabled bool `json:"ipv6_enabled"`
	// IPv6DisplayName 是 IPv6 条目的独立名称,**空串表示跟随 IPv4 名字**。
	//
	// 这里存的是原始值,不是解析后的名字:回落只有
	// subscription.IPv6EntryName 一处实现,API 层调它算出 ipv6_entry_name
	// 下发给前端。在这里存解析后的值,「改回跟随」就再也没法表达了。
	IPv6DisplayName string `json:"ipv6_display_name"`

	// IPv6EntryName 是 IPv6 条目在订阅里【实际】显示的名字,由 IPv6DisplayName
	// 回落而来(空则 IPv4 名字 + -IPV6)。不落库,只出现在 API 响应里。
	//
	// 由后端算好下发而不是让前端自己拼后缀:回落只有
	// subscription.IPv6EntryName 一处实现,前端再拼一遍的话,某天改了规则
	// 只会改到一边,表现是面板上显示的名字与用户客户端里那一条对不上,
	// 而两边都不报错。与「周期重置日只渲染后端给的 next_reset_at」同一条规矩。
	IPv6EntryName string `json:"ipv6_entry_name"`

	TCPFastOpen         bool `json:"tcp_fast_open"`
	DeployedTCPFastOpen bool `json:"deployed_tcp_fast_open"`

	RealityDest       string `json:"reality_dest"`
	RealityDestPort   int    `json:"reality_dest_port"`
	RealityPrivateKey string `json:"-"`
	RealityPublicKey  string `json:"reality_public_key"`
	RealityShortID    string `json:"reality_short_id"`

	HandshakeMaxRecordSize int     `json:"handshake_max_record_size"`
	HandshakeCheckedAt     *string `json:"handshake_checked_at"`

	// ChainTargetKind 为空表示这个入站的流量走 direct。
	// 落地指向的是【另一个入站】而不是一台机器:一台机器上有两个入站时,
	// "转发到 B"是有歧义的,而歧义的表现是流量进了管理员没打算用的那个入口。
	ChainTargetKind       ChainTargetKind `json:"chain_target_kind"`
	ChainTargetInboundID  int64           `json:"chain_target_inbound_id"`
	ChainTargetExternalID int64           `json:"chain_target_external_id"`
	// ChainCode 是链路凭据在【落地入站】的流量统计里的计数器名(chain_000001)。
	ChainCode       string `json:"chain_code"`
	ChainUUID       string `json:"-"`
	ChainSSPassword string `json:"-"`

	AccessTierID    int64  `json:"access_tier_id"`
	AccessTierCode  string `json:"access_tier_code"`
	AccessTierName  string `json:"access_tier_name"`
	AccessTierLevel int    `json:"access_tier_level"`

	SortOrder int `json:"sort_order"`
	// SubscriptionEnabled 为假时这个入口不再下发到新生成的订阅,
	// 但它仍然在节点上运行 —— 已经拿到订阅的人照常能连。
	SubscriptionEnabled bool   `json:"subscription_enabled"`
	PublicRemark        string `json:"public_remark"`
	// Enabled 为假时这个入站不再渲染进 sing-box 配置(下次部署生效)。
	// 与软删除不同:行还留着,重新打开不用重配等级、排序与握手目标。
	Enabled bool `json:"enabled"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EffectivePublicPort 是客户端实际要连的端口。
func (i Inbound) EffectivePublicPort() int {
	if i.PublicPort != 0 {
		return i.PublicPort
	}
	return i.ListenPort
}

// EffectiveIPv6PublicPort 是 IPv6 条目实际要连的端口。
func (i Inbound) EffectiveIPv6PublicPort() int {
	if i.IPv6PublicPort != 0 {
		return i.IPv6PublicPort
	}
	return i.EffectivePublicPort()
}

// Deployed 表示这个入站确实已经跑在节点上了。
//
// 判据是 deployed_protocol 而不是节点级的 deployed_config_sha256:
// 一台部署过很多次的机器上,刚加的那个入站仍然还不存在。
func (i Inbound) Deployed() bool { return i.DeployedProtocol != "" }

const inboundColumns = `i.id, i.node_id, n.name, n.display_name,
	i.tag, i.display_name, i.protocol, i.ss_method, i.ss_password_encrypted,
	i.deployed_protocol, i.deployed_ss_method,
	i.snell_version, i.snell_psk_encrypted, i.snell_obfs_mode, i.snell_obfs_host,
	i.snell_v6_mode, i.snell_shared_psk,
	i.deployed_snell_version, i.deployed_snell_obfs_mode,
	i.deployed_snell_v6_mode, i.deployed_snell_shared_psk,
	i.listen_port, i.public_port, i.ipv6_public_port,
	i.ipv6_enabled, i.ipv6_display_name,
	i.tcp_fast_open, i.deployed_tcp_fast_open,
	i.reality_dest, i.reality_dest_port, i.reality_privkey_encrypted, i.reality_pubkey,
	i.reality_short_id, i.handshake_max_record_size, i.handshake_checked_at,
	i.chain_target_kind, i.chain_target_inbound_id, i.chain_target_external_id,
	i.chain_code, i.chain_uuid_encrypted, i.chain_ss_password_encrypted,
	i.access_tier_id, t.code, t.name, t.level,
	i.sort_order, i.subscription_enabled, i.public_remark, i.enabled,
	i.created_at, i.updated_at`

// inboundFrom 固定带上节点与等级表:两者都是入站的必备上下文,
// 分两次查会出现"列表里有、详情里没有"这种前端只能靠猜的差异。
const inboundFrom = ` FROM node_inbounds i
	JOIN nodes        n ON n.id = i.node_id
	JOIN access_tiers t ON t.id = i.access_tier_id `

func (s *Store) scanInbound(scan func(dest ...any) error) (*Inbound, error) {
	var in Inbound
	var realityKeyEnc, ssKeyEnc, chainUUIDEnc, chainSSEnc, snellPSKEnc string
	// 目标列可空:直连的入站这两列是 NULL,扫进 int64 会报错。
	var chainInboundID, chainExternalID sql.NullInt64
	err := scan(
		&in.ID, &in.NodeID, &in.NodeName, &in.NodeDisplayName,
		&in.Tag, &in.DisplayName, &in.Protocol, &in.SSMethod, &ssKeyEnc,
		&in.DeployedProtocol, &in.DeployedSSMethod,
		&in.SnellVersion, &snellPSKEnc, &in.SnellObfsMode, &in.SnellObfsHost,
		&in.SnellV6Mode, &in.SnellSharedPSK,
		&in.DeployedSnellVersion, &in.DeployedSnellObfsMode,
		&in.DeployedSnellV6Mode, &in.DeployedSnellSharedPSK,
		&in.ListenPort, &in.PublicPort, &in.IPv6PublicPort,
		&in.IPv6Enabled, &in.IPv6DisplayName,
		&in.TCPFastOpen, &in.DeployedTCPFastOpen,
		&in.RealityDest, &in.RealityDestPort, &realityKeyEnc, &in.RealityPublicKey,
		&in.RealityShortID, &in.HandshakeMaxRecordSize, &in.HandshakeCheckedAt,
		&in.ChainTargetKind, &chainInboundID, &chainExternalID,
		&in.ChainCode, &chainUUIDEnc, &chainSSEnc,
		&in.AccessTierID, &in.AccessTierCode, &in.AccessTierName, &in.AccessTierLevel,
		&in.SortOrder, &in.SubscriptionEnabled, &in.PublicRemark, &in.Enabled,
		&in.CreatedAt, &in.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	in.ChainTargetInboundID = chainInboundID.Int64
	in.ChainTargetExternalID = chainExternalID.Int64
	for _, f := range []struct {
		enc  string
		dest *string
		what string
	}{
		{realityKeyEnc, &in.RealityPrivateKey, "REALITY 私钥"},
		{ssKeyEnc, &in.SSPassword, "Shadowsocks 密钥"},
		{chainUUIDEnc, &in.ChainUUID, "链路 UUID"},
		{chainSSEnc, &in.ChainSSPassword, "链路 Shadowsocks 密钥"},
		{snellPSKEnc, &in.SnellPSK, "Snell psk"},
	} {
		if f.enc == "" {
			continue
		}
		if *f.dest, err = s.cipher.Decrypt(f.enc); err != nil {
			return nil, fmt.Errorf("解密入站 %d 的%s: %w", in.ID, f.what, err)
		}
	}
	// 派生字段在这里填,scanInbound 是 Inbound 唯一的构造入口 ——
	// 各处自己算的话,漏掉一处的表现是「列表里有、点进详情就没了」。
	in.IPv6EntryName = subscription.IPv6EntryName(in.DisplayName, in.IPv6DisplayName)
	return &in, nil
}

func (s *Store) queryInbounds(ctx context.Context, where string, args ...any) ([]*Inbound, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+inboundColumns+inboundFrom+where+` ORDER BY i.sort_order, i.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 绝不返回 nil 切片:Go 的 nil 切片序列化成 JSON null 而不是 [],
	// 而前端把它当数组用。
	list := make([]*Inbound, 0)
	for rows.Next() {
		in, err := s.scanInbound(rows.Scan)
		if err != nil {
			return nil, err
		}
		list = append(list, in)
	}
	return list, rows.Err()
}

// InboundsForNode 返回一台机器上全部未删除的入站(含已停用的)。
func (s *Store) InboundsForNode(ctx context.Context, nodeID int64) ([]*Inbound, error) {
	return s.queryInbounds(ctx, `WHERE i.deleted_at IS NULL AND i.node_id = ?`, nodeID)
}

// AllInbounds 一次取出全部机器的入站,供列表页一次性挂到各自的节点上。
// 逐节点查的话,10 台机器就是 10 次往返。
func (s *Store) AllInbounds(ctx context.Context) ([]*Inbound, error) {
	return s.queryInbounds(ctx, `WHERE i.deleted_at IS NULL AND n.deleted_at IS NULL`)
}

// GetInbound 按 ID 读取。
func (s *Store) GetInbound(ctx context.Context, id int64) (*Inbound, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+inboundColumns+inboundFrom+`WHERE i.id = ? AND i.deleted_at IS NULL`, id)
	in, err := s.scanInbound(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInboundNotFound
	}
	return in, err
}

// InboundParams 是新增或编辑一个入站时可填的字段。
//
// 新增与编辑收同一个结构体:两边各写一份校验的话,某天加了一项只改到一处,
// 表现是"这个值编辑得进去、新建填不进去"这种谁都解释不了的怪事。
type InboundParams struct {
	DisplayName string
	// Protocol 留空按 VLESS_REALITY;SSMethod 只在 SHADOWSOCKS 下有意义。
	Protocol string
	SSMethod string
	// 以下四项只在 SNELL 下有意义。SnellVersion 留 0 表示用默认版本
	// (新建时)或保持原值(编辑时)—— 与 AccessTierID 同一个约定,
	// 而不是 IPv6DisplayName 那个"空串表示改回跟随"。
	// 版本没有"跟随"这种状态,漏传时保持原值是唯一说得通的解释。
	SnellVersion  int
	SnellObfsMode string
	SnellObfsHost string
	SnellV6Mode   string
	// SnellSharedPSK 与上面几项一样只在 SNELL 下有意义。
	// 它是普通布尔而不是指针:表单里它永远是一个明确的开关状态,
	// 而"漏传 = 关掉"在这里恰好是安全的方向 —— 关掉只会让入口
	// 回到逐用户凭据,那是更严的那一档。
	SnellSharedPSK bool
	// ListenPort 必填;PublicPort 留 0 表示跟随 ListenPort;
	// IPv6PublicPort 留 0 表示跟随 PublicPort。
	ListenPort     int
	PublicPort     int
	IPv6PublicPort int
	// IPv6Enabled 为 nil:新增时默认开,编辑时保持原值 —— 与
	// SubscriptionEnabled / Enabled 一致。默认开是因为默认关会让升级后
	// 全部双栈机器的 IPv6 条目从所有人的订阅里同时消失,而没有人做过什么。
	IPv6Enabled *bool
	// IPv6DisplayName **空串表示「跟随 IPv4 名字」,不是「保持原值」**。
	//
	// 与 AccessTierID 为 0 表示保持原值的约定相反,而这里必须相反:
	// 清空覆盖值是管理员表达「改回跟随」的唯一方式,当成保持原值的话,
	// 他把输入框清空、保存、再打开,名字还在,怎么点都回不到跟随状态。
	// 与外部代理「清空展示名覆盖值 = 跟随上游」是同一条道理。
	IPv6DisplayName string
	TCPFastOpen     bool
	// RealityDest 为空时使用默认候选目标的第一个(仅 VLESS)。
	RealityDest     string
	RealityDestPort int
	// AccessTierID 留 0:新增时落到普通组,编辑时**保持原值** ——
	// 漏传把 VIP 入口降成普通组等于给全体用户开门,而且不报错。
	AccessTierID int64
	SortOrder    int
	// SubscriptionEnabled / Enabled 为 nil:新增时默认开,编辑时保持原值。
	SubscriptionEnabled *bool
	Enabled             *bool
	PublicRemark        string
}

// CreateInbound 在一台落地机器上新增一个入站。
//
// REALITY 密钥对与 Shadowsocks PSK 一律生成,与本次选的协议无关 ——
// 两者都是纯本地计算,零成本,而缺了任何一个都会让"切协议"变成一个
// 可能在中途失败的复合操作。与 Store.Create 的理由一字不差。
func (s *Store) CreateInbound(ctx context.Context, nodeID int64, p InboundParams) (*Inbound, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	id, err := s.createInboundTx(ctx, tx, nodeID, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetInbound(ctx, id)
}

func (s *Store) createInboundTx(
	ctx context.Context, tx *sql.Tx, nodeID int64, p InboundParams,
) (int64, error) {
	var role, nodeName, displayName, ipv6, channel string
	var apiPort int
	err := tx.QueryRowContext(ctx,
		`SELECT role, name, display_name, api_port, ipv6_address, singbox_channel
		   FROM nodes WHERE id = ? AND deleted_at IS NULL`,
		nodeID).Scan(&role, &nodeName, &displayName, &apiPort, &ipv6, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if Role(role).IsRelay() {
		return 0, ErrInboundNotOnLanding
	}

	if err := normalizeInboundParams(&p, displayName); err != nil {
		return 0, err
	}
	if err := checkChannelSupportsProtocol(SingBoxChannel(channel), p.Protocol); err != nil {
		return 0, err
	}
	clearIPv6PortWithoutAddress(ipv6, &p.IPv6PublicPort)
	if p.AccessTierID == 0 {
		p.AccessTierID = access.TierNormalID
	}
	// 校验走事务而不是 s.db:连接池上限是 1,在打开的事务里再拿 *sql.DB
	// 查一次会直接死锁 —— 而新建节点连同第一个入站就走这条路径。
	if err := access.ValidateTier(ctx, tx, p.AccessTierID); err != nil {
		return 0, err
	}
	if err := checkInboundPortFree(ctx, tx, nodeID, p.ListenPort, apiPort, 0); err != nil {
		return 0, err
	}

	keys, err := GenerateRealityKeyPair()
	if err != nil {
		return 0, err
	}
	shortID, err := GenerateShortID(8)
	if err != nil {
		return 0, err
	}
	ssKey, err := GenerateSSKey()
	if err != nil {
		return 0, err
	}
	realityKeyEnc, err := s.cipher.Encrypt(keys.PrivateKey)
	if err != nil {
		return 0, fmt.Errorf("加密 REALITY 私钥: %w", err)
	}
	ssKeyEnc, err := s.cipher.Encrypt(ssKey)
	if err != nil {
		return 0, fmt.Errorf("加密 Shadowsocks 密钥: %w", err)
	}
	// Snell 的 psk 与 REALITY 密钥对、SS 密钥一样【无条件生成】,
	// 与本次选的协议无关:三者都是纯本地随机数,零成本,而缺了任何一个
	// 都会让"切协议"变成一个可能在中途失败的复合操作 —— 失败时入口停在
	// 半成品状态,而管理员看到的只是一句"生成密钥失败"。
	snellPSK, err := singbox.GenerateSnellKey()
	if err != nil {
		return 0, err
	}
	snellPSKEnc, err := s.cipher.Encrypt(snellPSK)
	if err != nil {
		return 0, fmt.Errorf("加密 Snell psk: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	// tag 先留空:它由 id 派生,而 id 要插入之后才知道。空串被
	// idx_node_inbounds_tag 的部分索引放过,是插入过程中唯一合法的中间态。
	res, err := tx.ExecContext(ctx, `
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, ss_method,
			ss_password_encrypted,
			snell_version, snell_psk_encrypted, snell_obfs_mode, snell_obfs_host,
			snell_v6_mode, snell_shared_psk,
			listen_port, public_port, ipv6_public_port,
			ipv6_enabled, ipv6_display_name, tcp_fast_open,
			reality_dest, reality_dest_port, reality_privkey_encrypted, reality_pubkey,
			reality_short_id, access_tier_id, sort_order, subscription_enabled,
			public_remark, enabled, created_at, updated_at)
		VALUES (?,'',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, p.DisplayName, p.Protocol, p.SSMethod, ssKeyEnc,
		p.SnellVersion, snellPSKEnc, p.SnellObfsMode, p.SnellObfsHost, p.SnellV6Mode,
		p.SnellSharedPSK,
		p.ListenPort, p.PublicPort, p.IPv6PublicPort,
		boolOr(p.IPv6Enabled, true), p.IPv6DisplayName, p.TCPFastOpen,
		p.RealityDest, p.RealityDestPort, realityKeyEnc, keys.PublicKey, shortID,
		p.AccessTierID, p.SortOrder, boolOr(p.SubscriptionEnabled, true),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, true), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w:该机器上已有入站监听 %d", ErrInboundPortConflict, p.ListenPort)
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE node_inbounds SET tag = ? WHERE id = ?`, InboundTagFor(id), id); err != nil {
		return 0, err
	}
	return id, nil
}

// InboundTagFor 是新建入站的 tag。
//
// 由 id 派生而不是按协议现算:tag 一经分配不可更改,而 id 天然唯一、
// 永不复用。带协议的话(vless-in),同机两个 VLESS 入站会撞名,
// 而 sing-box 对重名 tag 不报错 —— 后定义的直接覆盖前一个。
func InboundTagFor(id int64) string { return fmt.Sprintf("in-%d", id) }

// InboundEffect 描述一次入站变更对下游的影响。
type InboundEffect struct {
	// NeedsDeploy 表示节点配置变了,要重新部署才生效。
	//
	// 与访问等级变更不同:那一条是安全问题(被移出的用户凭据还留在节点上),
	// 入站参数变更是可用性问题 —— 立刻部署会让全部在线用户在管理员没准备好时
	// 断线,而部署完成前订阅仍下发旧参数,没有人会拿到连不上的东西。
	NeedsDeploy bool
	// TierChanged 表示可见性变了,必须自动标脏重新部署:
	// 等级调高后,被移出的用户凭据还留在节点上,拖多久就多能用多久。
	TierChanged bool
	// SubscriptionChanged 表示只影响订阅内容,不用碰节点。
	SubscriptionChanged bool
	// Changes 是给审计日志看的可读变更列表。
	//
	// 与节点那一层的 UpdateEffect.Changes 同一个用途:审计要回答
	// "他到底改了什么",而不是"他保存过一次"。写成枚举值
	// (VLESS_REALITY → SHADOWSOCKS)的话,几个月后翻日志的人
	// 还要再去查一遍这两个词的含义。
	Changes []string
}

// UpdateInbound 修改一个入站。
func (s *Store) UpdateInbound(
	ctx context.Context, id int64, p InboundParams,
) (*Inbound, InboundEffect, error) {
	cur, err := s.GetInbound(ctx, id)
	if err != nil {
		return nil, InboundEffect{}, err
	}
	var apiPort int
	var ipv6, channel string
	if err := s.db.QueryRowContext(ctx,
		`SELECT api_port, ipv6_address, singbox_channel FROM nodes WHERE id = ?`,
		cur.NodeID).Scan(&apiPort, &ipv6, &channel); err != nil {
		return nil, InboundEffect{}, err
	}
	// 版本留 0 表示保持原值 —— 编辑表单在非 Snell 协议下根本不渲染这一栏,
	// 漏传就会把一个 v6 入口变成 0,而 0 的意思是"这不是 Snell 入站"。
	if p.SnellVersion == 0 {
		p.SnellVersion = cur.SnellVersion
	}
	if err := normalizeInboundParams(&p, cur.DisplayName); err != nil {
		return nil, InboundEffect{}, err
	}
	if err := checkChannelSupportsProtocol(SingBoxChannel(channel), p.Protocol); err != nil {
		return nil, InboundEffect{}, err
	}
	clearIPv6PortWithoutAddress(ipv6, &p.IPv6PublicPort)

	tierID := p.AccessTierID
	if tierID == 0 {
		tierID = cur.AccessTierID
	} else if err := access.NewStore(s.db).Validate(ctx, tierID); err != nil {
		return nil, InboundEffect{}, err
	}
	if p.ListenPort != cur.ListenPort {
		if err := checkInboundPortFree(ctx, s.db, cur.NodeID, p.ListenPort, apiPort, id); err != nil {
			return nil, InboundEffect{}, err
		}
	}
	// 切到 VLESS 之前必须已经实测过握手目标 —— 与 V4 那条一字不差:
	// 不在切协议时顺带跑检测,那会让"切协议"变成一个可能中途失败的复合操作,
	// 失败时入站停在半成品状态,而管理员看到的只是一句"检测失败"。
	if err := checkInboundProtocolSwitch(cur, singbox.Protocol(p.Protocol), p.RealityDest); err != nil {
		return nil, InboundEffect{}, err
	}

	subEnabled := boolOr(p.SubscriptionEnabled, cur.SubscriptionEnabled)
	enabled := boolOr(p.Enabled, cur.Enabled)
	v6Enabled := boolOr(p.IPv6Enabled, cur.IPv6Enabled)
	effect := InboundEffect{
		// 进入配置文件的字段才要重新部署。公网端口、名称与排序只影响订阅内容,
		// 为它们重启 sing-box 会把这台机器上全部在线连接踢掉一次,换不来任何东西。
		NeedsDeploy: p.Protocol != string(cur.Protocol) ||
			p.SSMethod != cur.SSMethod ||
			p.SnellVersion != cur.SnellVersion ||
			p.SnellObfsMode != cur.SnellObfsMode ||
			p.SnellV6Mode != cur.SnellV6Mode ||
			p.SnellSharedPSK != cur.SnellSharedPSK ||
			p.ListenPort != cur.ListenPort ||
			p.TCPFastOpen != cur.TCPFastOpen ||
			p.RealityDest != cur.RealityDest ||
			p.RealityDestPort != cur.RealityDestPort ||
			enabled != cur.Enabled,
		TierChanged: tierID != cur.AccessTierID,
		// IPv6 的开关与名称只改订阅内容 —— 它们一个字节都不进节点配置,
		// 为它们重启 sing-box 会把这台机器上全部在线连接踢掉一次。
		// Snell 的混淆 Host 只影响客户端配置(服务端没有这个字段),
		// 所以它落在这一档而不是 NeedsDeploy —— 为它重启 sing-box
		// 会把这台机器上全部在线连接踢掉一次,换不来任何配置变化。
		SubscriptionChanged: p.SnellObfsHost != cur.SnellObfsHost ||
			p.DisplayName != cur.DisplayName ||
			p.PublicPort != cur.PublicPort ||
			p.IPv6PublicPort != cur.IPv6PublicPort ||
			v6Enabled != cur.IPv6Enabled ||
			p.IPv6DisplayName != cur.IPv6DisplayName ||
			p.SortOrder != cur.SortOrder ||
			subEnabled != cur.SubscriptionEnabled,
		Changes: []string{},
	}

	track := func(label string, changed bool, from, to any) {
		if changed {
			effect.Changes = append(effect.Changes,
				fmt.Sprintf("%s %v → %v", label, from, to))
		}
	}
	track("入口名称", p.DisplayName != cur.DisplayName, cur.DisplayName, p.DisplayName)
	track("落地协议", p.Protocol != string(cur.Protocol),
		cur.Protocol.Label(), singbox.Protocol(p.Protocol).Label())
	// 加密方法只在 Shadowsocks 下有意义。协议切走时它被清空,
	// 那不是"管理员改了方法",写进审计只会让人以为动了两处。
	track("加密方法", singbox.Protocol(p.Protocol) == singbox.ProtocolShadowsocks &&
		p.SSMethod != cur.SSMethod, orDash(cur.SSMethod), orDash(p.SSMethod))
	isSnell := singbox.Protocol(p.Protocol) == singbox.ProtocolSnell
	// 与加密方法同理:协议切走时这几项被清空,那不是"管理员改了版本"。
	track("Snell 版本", isSnell && p.SnellVersion != cur.SnellVersion,
		snellVersionLabel(cur.SnellVersion), snellVersionLabel(p.SnellVersion))
	track("Snell 混淆", isSnell && p.SnellObfsMode != cur.SnellObfsMode,
		orDash(cur.SnellObfsMode), orDash(p.SnellObfsMode))
	track("Snell 混淆 Host", isSnell && p.SnellObfsHost != cur.SnellObfsHost,
		orDash(cur.SnellObfsHost), orDash(p.SnellObfsHost))
	track("Snell 整形模式", isSnell && p.SnellV6Mode != cur.SnellV6Mode,
		orDash(cur.SnellV6Mode), orDash(p.SnellV6Mode))
	// 这一条单独记,而且要写清后果:它决定这个入口还有没有逐用户凭据,
	// 而那是"能不能把一个人踢下线"的前提。审计里只写 true → false
	// 的话,几个月后翻日志的人看不出那天发生了什么。
	track("Snell 凭据模式", isSnell && p.SnellSharedPSK != cur.SnellSharedPSK,
		sharedPSKLabel(cur.SnellSharedPSK), sharedPSKLabel(p.SnellSharedPSK))
	track("主机监听端口", p.ListenPort != cur.ListenPort, cur.ListenPort, p.ListenPort)
	track("公网端口", p.PublicPort != cur.PublicPort,
		followLabel(cur.PublicPort, "监听端口"), followLabel(p.PublicPort, "监听端口"))
	track("IPv6 公网端口", p.IPv6PublicPort != cur.IPv6PublicPort,
		followLabel(cur.IPv6PublicPort, "IPv4"), followLabel(p.IPv6PublicPort, "IPv4"))
	track("IPv6 条目", v6Enabled != cur.IPv6Enabled,
		onOffLabel(cur.IPv6Enabled), onOffLabel(v6Enabled))
	// 改名要单独记一条:用户客户端里会因此多出一份新节点,而旧的那份
	// 永远留着 —— 几个月后有人问"我这里怎么有两个一样的节点",
	// 审计里得查得到是谁在什么时候改的。
	track("IPv6 条目名称", p.IPv6DisplayName != cur.IPv6DisplayName,
		nameFollowLabel(cur.IPv6DisplayName), nameFollowLabel(p.IPv6DisplayName))
	track("TCP Fast Open", p.TCPFastOpen != cur.TCPFastOpen,
		onOffLabel(cur.TCPFastOpen), onOffLabel(p.TCPFastOpen))
	track("握手目标", p.RealityDest != cur.RealityDest,
		orDash(cur.RealityDest), orDash(p.RealityDest))
	track("访问等级", tierID != cur.AccessTierID, cur.AccessTierID, tierID)
	track("排序", p.SortOrder != cur.SortOrder, cur.SortOrder, p.SortOrder)
	track("下发订阅", subEnabled != cur.SubscriptionEnabled,
		onOffLabel(cur.SubscriptionEnabled), onOffLabel(subEnabled))
	track("启用", enabled != cur.Enabled, onOffLabel(cur.Enabled), onOffLabel(enabled))

	_, err = s.db.ExecContext(ctx, `
		UPDATE node_inbounds SET display_name = ?, protocol = ?, ss_method = ?,
		       snell_version = ?, snell_obfs_mode = ?, snell_obfs_host = ?,
		       snell_v6_mode = ?, snell_shared_psk = ?,
		       listen_port = ?, public_port = ?, ipv6_public_port = ?,
		       ipv6_enabled = ?, ipv6_display_name = ?, tcp_fast_open = ?,
		       reality_dest = ?, reality_dest_port = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.DisplayName, p.Protocol, p.SSMethod,
		p.SnellVersion, p.SnellObfsMode, p.SnellObfsHost, p.SnellV6Mode,
		p.SnellSharedPSK,
		p.ListenPort, p.PublicPort, p.IPv6PublicPort,
		v6Enabled, p.IPv6DisplayName, p.TCPFastOpen,
		p.RealityDest, p.RealityDestPort,
		tierID, p.SortOrder, boolOr(p.SubscriptionEnabled, cur.SubscriptionEnabled),
		strings.TrimSpace(p.PublicRemark), boolOr(p.Enabled, cur.Enabled),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, InboundEffect{}, fmt.Errorf("%w:该机器上已有入站监听 %d",
				ErrInboundPortConflict, p.ListenPort)
		}
		return nil, InboundEffect{}, err
	}
	updated, err := s.GetInbound(ctx, id)
	return updated, effect, err
}

// DeleteInbound 软删除一个入站。
//
// 软删除而不是物理删除,与节点、用户、转发规则一致。更要紧的是 tag:
// 唯一索引不带 deleted_at 过滤,软删除的行仍然占着它的 tag ——
// 让新入站抢到同一个 tag,两段互不相干的入站级流量历史会接在一条曲线上。
func (s *Store) DeleteInbound(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_inbounds SET deleted_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInboundNotFound
	}
	return nil
}

// SaveInboundDestCheck 写入实测通过的握手目标。
//
// 与节点时代的 ApplyHandshakeDest 一样:不走通用的 UpdateInbound,
// 否则会绕过 8192 字节记录上限的校验。
func (s *Store) SaveInboundDestCheck(
	ctx context.Context, id int64, dest string, destPort, maxRecord int,
) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE node_inbounds SET reality_dest = ?, reality_dest_port = ?,
		       handshake_max_record_size = ?, handshake_checked_at = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		dest, destPort, maxRecord,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// normalizeInboundParams 归一化并校验入站参数。
func normalizeInboundParams(p *InboundParams, fallbackName string) error {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if p.DisplayName == "" {
		p.DisplayName = fallbackName
	}
	if err := validateEntryName(p.DisplayName, "入口名称"); err != nil {
		return err
	}
	// 空串是合法的,它表示「跟随 IPv4 名称」。
	p.IPv6DisplayName = strings.TrimSpace(p.IPv6DisplayName)
	if p.IPv6DisplayName != "" {
		if err := validateEntryName(p.IPv6DisplayName, "IPv6 条目名称"); err != nil {
			return err
		}
		// 两条条目同名的话,用户在客户端里挑不出哪条走 IPv6 —— 而这两条是
		// 同一个入口的两个地址,「一条通、一条不通」正是最需要分辨的时候。
		// 想让它跟 IPv4 同名的唯一合理解释是填错了。
		if p.IPv6DisplayName == p.DisplayName {
			return errors.New("IPv6 条目名称不能与入口名称相同 —— " +
				"订阅里会出现两条完全同名的节点,用户分不出哪条走 IPv6;" +
				"留空即可自动区分")
		}
	}

	if err := singbox.ValidatePort(p.ListenPort, "主机监听"); err != nil {
		return err
	}
	for _, port := range []struct {
		v    int
		what string
	}{{p.PublicPort, "公网"}, {p.IPv6PublicPort, "IPv6 公网"}} {
		if port.v == 0 {
			continue
		}
		if err := singbox.ValidatePort(port.v, port.what); err != nil {
			return err
		}
	}

	if err := normalizeProtocol(&p.Protocol, &p.SSMethod); err != nil {
		return err
	}
	if err := normalizeSnellParams(p); err != nil {
		return err
	}
	// Shadowsocks 与 Snell 都不用 REALITY,握手目标一并留空。
	//
	// 不给它们填一个默认候选:那个域名从来没在这台机器上实测过,
	// 而详情里显示一个未经检测的握手目标,会让人以为这一步已经做过了。
	if singbox.Protocol(p.Protocol) != singbox.ProtocolVLESSReality {
		p.RealityDest = ""
		p.RealityDestPort = 0
		return nil
	}
	if p.RealityDest == "" {
		p.RealityDest = DefaultDestCandidates[0]
	}
	if err := singbox.ValidateHandshakeServer(p.RealityDest); err != nil {
		return err
	}
	if p.RealityDestPort == 0 {
		p.RealityDestPort = 443
	}
	return singbox.ValidatePort(p.RealityDestPort, "握手目标")
}

// normalizeSnellParams 归一化 Snell 的四项参数。
//
// 协议不是 Snell 时全部清空,与 normalizeProtocol 清 ss_method 一样。
// 代价是"切走再切回来"会回到默认版本 —— 那与 Shadowsocks 切回来
// 回到默认加密方法是同一种代价,而反过来(留着旧值)更糟:
// 库里存着一个 v5 的混淆模式,而这个入口现在是 v6,渲染时要靠版本
// 二选一才不至于把它写进配置。
func normalizeSnellParams(p *InboundParams) error {
	if singbox.Protocol(p.Protocol) != singbox.ProtocolSnell {
		p.SnellVersion = 0
		p.SnellObfsMode = ""
		p.SnellObfsHost = ""
		p.SnellV6Mode = ""
		p.SnellSharedPSK = false
		return nil
	}

	version, err := singbox.ParseSnellVersion(p.SnellVersion)
	if err != nil {
		return err
	}
	// **共享模式强制版本 5,而且是拒绝而不是悄悄改。**
	//
	// 悄悄把 v6 改成 v5 的话,管理员在表单里选的是 v6、保存成功、
	// 详情里显示 v5 —— 他会以为面板坏了。而这个组合本身没有任何好处:
	// 共享模式唯一的理由是让 mihomo 能用,而 mihomo 对 v6 是整份配置拒绝。
	if p.SnellSharedPSK && version != singbox.SharedPSKVersion {
		return fmt.Errorf("%w —— 共享凭据唯一的理由是让 Clash / mihomo 能用,"+
			"而 mihomo 不支持 v6(它会拒绝整份配置,那个用户订阅里的全部节点"+
			"会一起消失)。要么把版本改成 5,要么关掉共享凭据",
			singbox.ErrSnellSharedVersion)
	}
	p.SnellVersion = version

	// 两项按版本二选一保留。留着不属于本版本的那一项没有好处:
	// 它会出现在编辑表单里,而管理员改它一次都不会生效。
	if version == singbox.SnellVersion5 {
		mode, err := singbox.ParseSnellObfsMode(p.SnellObfsMode)
		if err != nil {
			return err
		}
		p.SnellObfsMode = string(mode)
		p.SnellV6Mode = ""
		p.SnellObfsHost = strings.TrimSpace(p.SnellObfsHost)
		if mode == singbox.SnellObfsNone {
			// 不混淆就没有伪装 Host。留着它只会让客户端配置里多一个
			// 什么都不做的字段,而读配置的人得先去查它为什么不生效。
			p.SnellObfsHost = ""
		} else if p.SnellObfsHost != "" {
			if err := singbox.ValidateHandshakeServer(p.SnellObfsHost); err != nil {
				return fmt.Errorf("Snell 混淆 Host: %w", err)
			}
		}
		return nil
	}

	mode, err := singbox.ParseSnellV6Mode(p.SnellV6Mode)
	if err != nil {
		return err
	}
	p.SnellV6Mode = string(mode)
	p.SnellObfsMode = ""
	p.SnellObfsHost = ""
	return nil
}

// clearIPv6PortWithoutAddress 在机器没有 IPv6 地址时把 IPv6 公网端口归零。
//
// 留着它,下次给这台机器填上 IPv6 会静默套用一个几个月前的端口,
// 而那个端口未必还转发着 —— 用户拿到的是一条连不上的条目,面板一个错都不报。
// 这是 V2.1 就定下的规矩,只是主体从节点变成了入站。
func clearIPv6PortWithoutAddress(ipv6Address string, port *int) {
	if strings.TrimSpace(ipv6Address) == "" {
		*port = 0
	}
}

// checkInboundProtocolSwitch 拦住"没实测过握手目标就切到 VLESS"。
func checkInboundProtocolSwitch(cur *Inbound, next singbox.Protocol, nextDest string) error {
	// 判据是"切【到】VLESS 而它原来不是 VLESS" —— 从 Snell 切过来
	// 与从 Shadowsocks 切过来是同一件事,所以这里不列举来源协议。
	if next != singbox.ProtocolVLESSReality || cur.Protocol == singbox.ProtocolVLESSReality {
		return nil
	}
	if nextDest == "" || cur.HandshakeCheckedAt == nil {
		return errors.New("切换到 VLESS + REALITY 之前必须先实测握手目标 —— " +
			"REALITY 要求目标返回的每个 TLS 记录不超过 8192 字节,超限时握手会静默失败:" +
			"客户端连不上,而节点上一切正常")
	}
	return nil
}

// queryer 让端口冲突检查在事务内外都能用 —— 新建节点时它跑在事务里
// (节点与第一个入站必须一起成功),单独加入站时跑在事务外。
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkInboundPortFree 确认这个监听端口在这台机器上没被占用。
//
// 实现整个搬去了 nodeport 包:一台机器上抢端口的东西现在有四类,
// 其中 Mieru 入口占的是**一整段**。原来这里逐张表写 `listen_port = ?` 的写法
// 对区间是无效的 —— 它查不出"这个端口落在某个 Mieru 段里",
// 而漏查的表现是 sing-box 或 mita 其中一个 bind 失败、整个服务起不来,
// 要到部署的健康检查才暴露。
//
// apiPort 参数留着不用:节点角色与 API 端口由 nodeport 自己去 nodes 表读,
// 那样三个调用点不必各自记得传对。签名保持不变是为了不动两个调用点。
func checkInboundPortFree(
	ctx context.Context, q queryer, nodeID int64, port, apiPort int, excludeID int64,
) error {
	_ = apiPort
	return nodeport.Free(ctx, q, nodeID,
		mieru.PortRange{Start: port, End: port},
		nodeport.Skip{Kind: nodeport.KindInbound, ID: excludeID})
}

// onOffLabel 用于审计里的开关变更。写「开 → 关」而不是「true → false」——
// 审计日志是给人读的,而管理员在界面上看到的就是一个开关。
func onOffLabel(v bool) string {
	if v {
		return "开"
	}
	return "关"
}

// followLabel 把 0 写成「跟随某某」而不是「0」——
// 审计里出现「公网端口 0 → 8443」没人看得懂 0 是什么意思。
func followLabel(port int, what string) string {
	if port == 0 {
		return "跟随" + what
	}
	return strconv.Itoa(port)
}

// validateEntryName 是订阅里可见的名字共用的一套校验。
//
// 换行与控制字符会把 URI 列表的行数搞乱,客户端解析出一个残缺条目 ——
// 这一条对 IPv4 名字与 IPv6 名字是同一件事,分两处写迟早只补上一处。
func validateEntryName(name, what string) error {
	if len([]rune(name)) > 64 {
		return errors.New(what + "不能超过 64 个字符")
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return errors.New(what + "不能包含换行或制表符")
	}
	return nil
}

// nameFollowLabel 把空的 IPv6 条目名写成「跟随 IPv4 名称」而不是「—」。
//
// 破折号读起来像"没有名字",而空串的实际含义是"有名字,只是跟着另一个字段走"。
// 审计要回答的是他把它改成了什么,不是那一格里存着什么。
func nameFollowLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "跟随 IPv4 名称"
	}
	return name
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// SharedInbound 是一台机器上**没有逐用户凭据**的入站。
//
// 目前只有共享模式的 Snell 入口。它对流量采集是一个特例:
// sing-box 不会为它建任何 user 计数器(服务端跑在单用户模式),
// 所以那个入口的流量在 user>>> 那一族里完全不存在 ——
// 不额外读一份 inbound>>> 的话,这台机器的节点用量会静默少算,
// 而节点额度正是拿它去对商家账单的。
type SharedInbound struct {
	Tag  string
	Code string
}

// SharedInboundsForNode 返回这台机器上没有逐用户凭据的入站。
//
// 判据用 **deployed_** 而不是期望值:采集读的是节点上**此刻**那个进程的
// 计数器,而进程跑的是上一次部署下发的那份配置。按期望值判的话,
// 管理员刚把一个入口从共享改成多用户、还没下发时,这里会以为它已经
// 有用户计数器了 —— 于是那段时间的流量既不在 user>>> 里(节点上还是
// 共享模式)、也不被这里采走,静默丢失。反过来也一样坏:多用户入口
// 被误当成共享,它的 inbound>>> 与各用户的 user>>> 会被同时记进去,
// 那台机器的用量凭空翻一倍。
func (s *Store) SharedInboundsForNode(ctx context.Context, nodeID int64) ([]SharedInbound, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tag FROM node_inbounds
		 WHERE node_id = ? AND deleted_at IS NULL AND enabled = 1
		   AND deployed_protocol = ? AND deployed_snell_shared_psk = 1`,
		nodeID, string(singbox.ProtocolSnell))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]SharedInbound, 0)
	for rows.Next() {
		var id int64
		var tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		list = append(list, SharedInbound{Tag: tag, Code: singbox.SharedInboundCode(id)})
	}
	return list, rows.Err()
}
