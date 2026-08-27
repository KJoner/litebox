package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/traffic"
)

var (
	ErrNotFound      = errors.New("节点不存在")
	ErrNameConflict  = errors.New("节点名称已被占用")
	ErrHostKeyPinned = errors.New("节点主机密钥已固定")
)

// Status 是节点状态。
type Status string

const (
	StatusPending      Status = "PENDING"
	StatusOnline       Status = "ONLINE"
	StatusOffline      Status = "OFFLINE"
	StatusDisabled     Status = "DISABLED"
	StatusDeployFailed Status = "DEPLOY_FAILED"
)

// Node 是一个节点的完整记录。敏感字段在这里已是明文,
// 加解密由 Store 在读写边界处理。
type Node struct {
	ID int64 `json:"id"`
	// Name 是内部名称,只在管理后台出现;DisplayName 才是发给用户与订阅的名字。
	// 内部名称上通常写着机房、供应商、到期日甚至 IP 段,属于运维信息。
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Host 是这台机器【唯一】的管理地址:SSH、探测、安装、部署、重启、
	// 流量同步与资源采集一律走它 —— 只留一条管理通道才不会出现
	// "两条路两种结论"。
	//
	// SubIPv4Address 与 IPv6Address 只进订阅,面板一次都不解析它们。
	// SubIPv4Address 为空表示 IPv4 条目跟随 Host(回落只有
	// subscription.SubscriptionIPv4 一处实现),填了它之后管理地址与
	// 用户连的地址就此分开 —— 前面挂了一层 IP 转发、或者管理口上根本
	// 没开代理端口时,那是唯一填得下去的写法。
	Host           string `json:"host"`
	SubIPv4Address string `json:"sub_ipv4_address"`
	IPv6Address    string `json:"ipv6_address"`
	SSHPort        int    `json:"ssh_port"`
	SSHUser        string `json:"ssh_user"`
	SSHKey         string `json:"-"` // PEM 私钥,永不出现在 API 响应中
	HostKey        string `json:"-"`
	// APIPort 是节点上 V2Ray API 的回环端口。
	//
	// 它留在节点级而不是跟着入站走:一台机器上只有一个 sing-box 进程,
	// 也就只有一个 API 端点。全部入站的统计都从这一个端口读出来。
	APIPort int `json:"api_port"`

	// ConfigInRAM 为真时,这台机器上的 sing-box 配置与备份放在内存文件系统
	// (/run/litebox)里,磁盘上一个字节都不留。
	//
	// 机器重启后配置就没了,sing-box 起不来 —— 靠服务巡检重新下发救回来。
	// 所以它与「自动恢复」是配套的,单独打开反而危险。
	ConfigInRAM bool `json:"config_in_ram"`
	// Role 是节点角色:LANDING 落地(默认),RELAY 纯中转机(不跑 sing-box 服务)。
	// RELAY 上没有任何 node_inbounds 行。
	Role Role `json:"role"`

	// Inbounds 是这台机器上的 sing-box 入站,由 Get/List 一并带出。
	//
	// V8 之前这些参数(协议、端口、REALITY、SS、TFO、链式)直接是 nodes 上的列,
	// 因为那时一台机器只有一个入站。搬进独立表之后,nodes 上的原列已冻结,
	// 任何代码路径都不再读写它们 —— 详见迁移 0019。
	//
	// 绝不返回 nil:Go 的 nil 切片序列化成 JSON null 而不是 [],
	// 而前端把它当数组用(inbounds.length、inbounds[0])。
	Inbounds []*Inbound `json:"inbounds"`

	// MieruInbounds 是这台机器上的 Mieru 入口,同样由 Get/List 一并带出。
	//
	// 与 Inbounds 分成两个字段而不是合成一个:两类入口的服务端是两个进程,
	// 参数、部署方式与流量采集路径都不一样,合成一个数组会让每一处消费者
	// 都要先判断"这一项是哪一类" —— 而判断写漏的表现是渲染器把一个
	// Mieru 入口当成 sing-box 入站写进 config.json。
	// 界面上它们仍然并成一张列表,那一层的合并由前端做。
	//
	// 绝不返回 nil,理由同上。
	MieruInbounds []*MieruInbound `json:"mieru_inbounds"`

	// mieruEgress 是渲染 sing-box 配置时用的那一跳(不序列化,也不入库)。
	//
	// 挂在 Node 上而不是当参数层层传:renderInputs 是收齐渲染输入的
	// 唯一一处,而 nodeParams 只此一份 —— 多一个参数意味着两个调用点
	// 都要记得传,而漏传的表现是那台机器的 Mieru 出口从配置里消失,
	// 流量从本机直接出去,界面上却写着"出口:某某落地"。
	mieruEgress []singbox.MieruEgressParams

	Arch           string `json:"arch"`
	SingBoxVersion string `json:"singbox_version"`
	BuildTags      string `json:"singbox_build_tags"`
	// SingBoxChannel 是这台机器上装的 sing-box 属于哪一支(V14)。
	//
	// **它由安装动作写入,描述的是事实而不是期望** —— 见 SingBoxChannel
	// 的注释。Snell 入口只在 PREVIEW 上能建,而反过来:有 Snell 入口时
	// 装回正式版会被拦住,不然那台机器的整份配置从下一次部署起就渲染不出来
	// (sing-box check 报 unknown inbound type,部署失败并回滚)。
	SingBoxChannel SingBoxChannel `json:"singbox_channel"`
	// MemTotalMB 由探测写入,0 表示还没探测过。它只用来算入站的 udp_timeout ——
	// 不读 node_metrics 的最新采样:那个值每五分钟变一次、还能整个关掉,
	// 配置哈希会跟着抖,「已同步」与「待部署」两个状态来回跳。
	MemTotalMB int `json:"mem_total_mb"`

	// **机器没有访问等级**(V8.1,迁移 0020)。等级是【入口】的属性:
	// 机器本身不接受任何连接,入口才接受。两层同时存在的时候,
	// 「机器 VIP、其中一个入口对所有人开放」这件事做不到,而管理员会以为
	// 把入口调成普通组就行 —— 结果那个入口谁都看不见,面板一个字都不说。
	//
	// nodes.access_tier_id 那一列已冻结。user_nodes 的额外授权仍然是
	// 机器级的:它的意思就是「这台机器给他用」,穿透入口等级。
	SortOrder int `json:"sort_order"`
	// SubscriptionEnabled 为假时不再下发到新生成的订阅,
	// 但节点、历史流量与部署记录全部保留 —— 这是"进维护"而不是"删节点"。
	SubscriptionEnabled bool   `json:"subscription_enabled"`
	PublicRemark        string `json:"public_remark"`
	MaintenanceMessage  string `json:"maintenance_message"`

	// TrafficQuotaBytes 为 0 表示不限量。节点额度只用于统计与预警,
	// 超额不会自动停服 —— 同步有间隔、各家 VPS 的口径也不同,
	// 自动关掉一个共享节点会同时影响全部用户。
	TrafficQuotaBytes int64  `json:"traffic_quota_bytes"`
	TrafficResetCycle string `json:"traffic_reset_cycle"`
	TrafficResetDay   int    `json:"traffic_reset_day"`
	// TrafficBillingMode 是 VPS 商计量这台机器流量的口径:
	// EGRESS 只计出站(与 sing-box 计数 1:1),BOTH 进出合计(约两倍)。
	// 它只影响额度比较与展示,不动 traffic_ledger 一个字节。
	TrafficBillingMode string `json:"traffic_billing_mode"`

	Status               Status  `json:"status"`
	LastHeartbeatAt      *string `json:"last_heartbeat_at"`
	ConfigRevision       int64   `json:"config_revision"`
	DeployedConfigSHA256 string  `json:"deployed_config_sha256"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Store 负责节点的持久化,并在读写边界完成敏感字段的加解密。
type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

// nodeColumns 刻意【不含】迁移 0019 冻结的那十几列(协议、三个端口、
// REALITY、SS、TFO、链式)。它们的数据已经搬进 node_inbounds,留在 nodes 上
// 只是为了不重建这张全库被引用最多的表 —— 谁把它们加回这里,就等于
// 让同一件事有两个来源,而两个来源迟早分叉。
const nodeColumns = `n.id, n.name, n.display_name, n.host, n.sub_ipv4_address, n.ipv6_address,
	n.ssh_port, n.ssh_user,
	n.ssh_key_encrypted, n.ssh_host_key,
	n.api_port, n.role, n.config_in_ram,
	n.arch, n.singbox_version, n.singbox_build_tags, n.singbox_channel, n.mem_total_mb,
	n.sort_order, n.subscription_enabled, n.public_remark, n.maintenance_message,
	n.traffic_quota_bytes, n.traffic_reset_cycle, n.traffic_reset_day,
	n.traffic_billing_mode,
	n.status, n.last_heartbeat_at, n.config_revision, n.deployed_config_sha256,
	n.created_at, n.updated_at`

// nodeFrom 不再 JOIN 等级表:等级是入口的属性(迁移 0020),
// 而入口在 node_inbounds 上有自己的那一份。
const nodeFrom = ` FROM nodes n `

func (s *Store) scanNode(scan func(dest ...any) error) (*Node, error) {
	var n Node
	var sshKeyEnc string
	err := scan(
		&n.ID, &n.Name, &n.DisplayName, &n.Host, &n.SubIPv4Address, &n.IPv6Address,
		&n.SSHPort, &n.SSHUser, &sshKeyEnc, &n.HostKey,
		&n.APIPort, &n.Role, &n.ConfigInRAM,
		&n.Arch, &n.SingBoxVersion, &n.BuildTags, &n.SingBoxChannel, &n.MemTotalMB,
		&n.SortOrder, &n.SubscriptionEnabled, &n.PublicRemark, &n.MaintenanceMessage,
		&n.TrafficQuotaBytes, &n.TrafficResetCycle, &n.TrafficResetDay,
		&n.TrafficBillingMode,
		&n.Status, &n.LastHeartbeatAt, &n.ConfigRevision, &n.DeployedConfigSHA256,
		&n.CreatedAt, &n.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if sshKeyEnc != "" {
		if n.SSHKey, err = s.cipher.Decrypt(sshKeyEnc); err != nil {
			return nil, fmt.Errorf("解密节点 %d 的 SSH 私钥: %w", n.ID, err)
		}
	}
	n.Inbounds = make([]*Inbound, 0)
	n.MieruInbounds = make([]*MieruInbound, 0)
	return &n, nil
}

// CreateParams 是新增节点所需的参数。
type CreateParams struct {
	// Role 留空按 LANDING。**一经创建不可更改** —— LANDING 改 RELAY 等于
	// 卸掉 sing-box 并丢掉这台机器上全部用户凭据,RELAY 改 LANDING 则可能与
	// 已有的转发规则抢端口。两个方向都属于"删了重建"的范畴,而重建会丢掉
	// 这台机器的全部历史数据,所以要让管理员显式地那么做,而不是点一下开关。
	Role string

	Name string
	// DisplayName 留空时复制 Name。订阅里的节点名不能为空:
	// 客户端拿它识别条目,空名字会让用户面对一列无法区分的节点。
	DisplayName string
	// Host 是管理地址,必须填。SubIPv4Address 与 IPv6Address 选填,
	// 只进订阅:前者留空表示 IPv4 条目跟随 Host,后者留空表示该节点没有 IPv6。
	Host           string
	SubIPv4Address string
	IPv6Address    string
	SSHPort        int
	SSHUser        string
	SSHKey         string
	SortOrder      int
	// TrafficQuotaBytes 留 0 表示不限量;TrafficResetCycle 留空按 NONE;
	// TrafficBillingMode 留空按 EGRESS(与升级前的行为一致)。
	TrafficQuotaBytes  int64
	TrafficResetCycle  string
	TrafficResetDay    int
	TrafficBillingMode string
	APIPort            int

	// 以下是【第一个入站】的参数,由 Create 一并建出来。
	//
	// 新建节点顺带建一个入站,而不是建完再让管理员去「入口」里加一条:
	// 一台落地机器没有任何入站等于谁都连不上,那不是任何人想要的中间状态。
	// 再加的入口走 CreateInbound,两条路收同一个 InboundParams。
	//
	// ProxyPort 是客户端连接的公网端口,必填;ListenPort 留空表示不做端口转发,
	// 与 ProxyPort 相同;IPv6ProxyPort 留 0 表示跟随 ProxyPort。
	ProxyPort     int
	ListenPort    int
	IPv6ProxyPort int
	// Protocol 留空按 VLESS_REALITY 处理;SSMethod 只在 SHADOWSOCKS 下有意义,
	// 留空取默认方法。
	Protocol string
	SSMethod string
	// RealityDest 为空时使用默认候选目标的第一个。
	// SHADOWSOCKS 不要求握手目标,这两项会被强制清空。
	RealityDest     string
	RealityDestPort int
	// TCPFastOpen 默认关。新建时就能填,第一次部署即生效 ——
	// 只支持编辑的话,建完节点还要再进一次表单,而那次编辑与新建之间
	// 通常已经部署过一回了。
	TCPFastOpen bool
}

// firstInbound 把建节点表单里那几项翻译成入站参数。
//
// 只此一处:新建与「入口」里再加一条如果各拼一遍,某天加了一项只改到一处,
// 表现是"这个值在新建时填不进去"这种谁都解释不了的怪事。
func (p CreateParams) firstInbound() InboundParams {
	return InboundParams{
		// 展示名称留空,由 normalizeInboundParams 回落到节点的展示名 ——
		// 一台机器上只有一个入口时,两个名字长得一样才是对的。
		Protocol:        p.Protocol,
		SSMethod:        p.SSMethod,
		ListenPort:      p.ListenPort,
		PublicPort:      p.ProxyPort,
		IPv6PublicPort:  p.IPv6ProxyPort,
		TCPFastOpen:     p.TCPFastOpen,
		RealityDest:     p.RealityDest,
		RealityDestPort: p.RealityDestPort,
	}
}

// Create 新增节点。落地角色同时建出它的第一个入站,两者在同一个事务里 ——
// 分两步的话,中途失败会留下一台没有任何入站的落地机器,
// 而那台机器渲染出的配置里一个入站都没有,谁都连不上。
func (s *Store) Create(ctx context.Context, p CreateParams) (*Node, error) {
	if err := validateCreate(&p); err != nil {
		return nil, err
	}

	// 空私钥直接存空串而不是加密后的空串:读取侧用"是否为空"判断
	// 该节点是否用面板密钥,加密后的空串不为空,会被当成一把解不开的私钥。
	sshKeyEnc := ""
	var err error
	if p.SSHKey != "" {
		if sshKeyEnc, err = s.cipher.Encrypt(p.SSHKey); err != nil {
			return nil, fmt.Errorf("加密 SSH 私钥: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	// 迁移 0019 冻结的那几列这里一律写零值。它们仍然 NOT NULL,
	// 但已经没有任何代码路径读它们 —— 写一个"看起来合理"的值反而更糟:
	// 半年后有人翻库看到 nodes.proxy_port = 443,会以为那是真的。
	res, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (name, display_name, host, sub_ipv4_address, ipv6_address,
			ssh_port, ssh_user,
			ssh_key_encrypted, ssh_host_key, api_port,
			proxy_port, listen_port, ipv6_proxy_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			sort_order,
			traffic_quota_bytes, traffic_reset_cycle, traffic_reset_day, traffic_billing_mode,
			role, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,'',?,0,0,0,'','','','',?,?,?,?,?,?,?,?,?)`,
		p.Name, p.DisplayName, p.Host, p.SubIPv4Address, p.IPv6Address,
		p.SSHPort, p.SSHUser, sshKeyEnc,
		p.APIPort,
		p.SortOrder,
		p.TrafficQuotaBytes, p.TrafficResetCycle, p.TrafficResetDay, p.TrafficBillingMode,
		p.Role, StatusPending, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	// 中转机上不跑 sing-box,没有入站。
	if !Role(p.Role).IsRelay() {
		if _, err := s.createInboundTx(ctx, tx, id, p.firstInbound()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func validateCreate(p *CreateParams) error {
	var err error
	role, err := ParseRole(p.Role)
	if err != nil {
		return err
	}
	p.Role = string(role)
	if err = validateName(p.Name); err != nil {
		return err
	}
	if p.Host, err = normalizeIPv4(p.Host); err != nil {
		return err
	}
	if p.SubIPv4Address, err = normalizeSubIPv4(p.SubIPv4Address); err != nil {
		return err
	}
	if p.IPv6Address, err = normalizeIPv6(p.IPv6Address); err != nil {
		return err
	}
	if err = normalizeTrafficQuota(&p.TrafficQuotaBytes, &p.TrafficResetCycle,
		&p.TrafficResetDay, &p.TrafficBillingMode); err != nil {
		return err
	}
	if err := normalizeDisplayName(p.Name, &p.DisplayName); err != nil {
		return err
	}
	if err := normalizeSSH(&p.SSHPort, &p.SSHUser); err != nil {
		return err
	}
	// SSH 私钥可以留空:留空表示这个节点用面板专用密钥,
	// 由 Service.Bootstrap 在创建后把面板公钥装进节点。

	// 中转机到此为止:它上面不跑 sing-box,一个入站都不建,端口与握手目标
	// 都没有意义。端口全部归零而不是填一个看起来合理的默认值 ——
	// 详情页显示一个从来没有人监听过的端口,会让排查的人以为服务没起来。
	// 客户端连的端口在 node_relays 里,一条规则一个。
	if Role(p.Role).IsRelay() {
		p.ProxyPort, p.ListenPort, p.APIPort, p.IPv6ProxyPort = 0, 0, 0, 0
		p.RealityDest, p.RealityDestPort = "", 0
		p.TCPFastOpen = false
		p.Protocol, p.SSMethod = "", ""
		return nil
	}

	if err := normalizePorts(&p.ProxyPort, &p.ListenPort, &p.APIPort); err != nil {
		return err
	}
	// 协议、握手目标与 IPv6 端口的归一化不在这里做:它们已经是【入站】的属性,
	// 由 normalizeInboundParams 统一处理。在这里再写一遍的话,
	// 新建节点与「入口」里加一条会走两套校验,而两套迟早分叉。
	return normalizeIPv6Port(p.IPv6Address, &p.IPv6ProxyPort)
}

// normalizeProtocol 归一化落地协议与加密方法。
//
// 非 Shadowsocks 的节点把 ss_method 清成空串:留着一个用不到的方法名,
// 会让节点详情看起来像是"两种协议都配好了",而实际上只有一种在跑。
func normalizeProtocol(protocol, ssMethod *string) error {
	parsed, err := singbox.ParseProtocol(*protocol)
	if err != nil {
		return err
	}
	*protocol = string(parsed)

	if parsed != singbox.ProtocolShadowsocks {
		*ssMethod = ""
		return nil
	}
	method, err := singbox.ParseSSMethod(*ssMethod)
	if err != nil {
		return err
	}
	*ssMethod = string(method)
	return nil
}

func validateName(name string) error {
	if name == "" {
		return errors.New("节点名称不能为空")
	}
	if len(name) > 64 {
		return errors.New("节点名称不能超过 64 个字符")
	}
	return nil
}

// normalizeTrafficQuota 归一化节点流量额度与重置周期。
func normalizeTrafficQuota(quota *int64, cycle *string, day *int, billing *string) error {
	if *quota < 0 {
		return errors.New("节点流量限额不能为负数")
	}
	parsed, err := traffic.ParseResetCycle(*cycle)
	if err != nil {
		return err
	}
	*cycle = string(parsed)
	mode, err := traffic.ParseBillingMode(*billing)
	if err != nil {
		return err
	}
	*billing = string(mode)
	if *day == 0 {
		*day = 1
	}
	if *day < 1 || *day > 31 {
		return errors.New("每月重置日必须在 1~31 之间")
	}
	return nil
}

// normalizeDisplayName 让展示名称留空时回落到内部名称。
func normalizeDisplayName(name string, displayName *string) error {
	*displayName = strings.TrimSpace(*displayName)
	if *displayName == "" {
		*displayName = name
	}
	if len([]rune(*displayName)) > 64 {
		return errors.New("展示名称不能超过 64 个字符")
	}
	return nil
}

func normalizeSSH(port *int, user *string) error {
	if *port == 0 {
		*port = 22
	}
	if err := singbox.ValidatePort(*port, "SSH"); err != nil {
		return err
	}
	if *user == "" {
		*user = "root"
	}
	return nil
}

// normalizePorts 归一化公网端口、主机监听端口与 API 端口。
//
// 主机端口留空表示"不做端口转发",此时它等于公网端口 —— 这是绝大多数节点的形态。
// 只有 NAT 主机或自建 nginx 转发时两者才不同。
//
// 冲突检查只针对主机端口:公网端口是转发链路另一端的号码,节点上没有任何东西监听它,
// 拿它和 API 端口比会误伤合法配置(例如公网 28080 -> 主机 443)。
func normalizePorts(proxyPort, listenPort, apiPort *int) error {
	if err := singbox.ValidatePort(*proxyPort, "公网代理"); err != nil {
		return err
	}
	if *listenPort == 0 {
		*listenPort = *proxyPort
	}
	if err := singbox.ValidatePort(*listenPort, "主机代理监听"); err != nil {
		return err
	}
	if *apiPort == 0 {
		*apiPort = 28080
	}
	if err := singbox.ValidatePort(*apiPort, "V2Ray API"); err != nil {
		return err
	}
	if *listenPort == *apiPort {
		return errors.New("主机代理端口与 API 端口不能相同")
	}
	return nil
}

// normalizeIPv6Port 校验 IPv6 条目的公网端口。
//
// **0 保持 0,不在这里解析成 proxyPort。** 解析放在订阅生成时:
// 写死的话,以后改 IPv4 公网端口,IPv6 条目会继续停在旧端口上 ——
// 而管理员当初看到的是一个空输入框,不会想到那里固化了一个值。
//
// 没有 IPv6 地址时端口一并归零:留着它,下次重新填上 IPv6 会静默套用
// 一个几个月前的端口,而那个端口未必还转发着。清空是显式的,重填是显式的。
func normalizeIPv6Port(ipv6 string, port *int) error {
	if ipv6 == "" {
		*port = 0
		return nil
	}
	if *port == 0 {
		return nil
	}
	return singbox.ValidatePort(*port, "IPv6 公网代理")
}

// Get 读取一个节点,连同它的全部入站。
//
// 入站一并带出而不是让调用方各自再查一次:分两次查会出现"列表里有、
// 详情里没有"这种前端只能靠猜的差异,而入站是落地节点的必备内容 ——
// 没有它,节点详情页上「这台机器提供什么」这个问题答不了。
func (s *Store) Get(ctx context.Context, id int64) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+nodeFrom+`WHERE n.id = ? AND n.deleted_at IS NULL`, id)
	n, err := s.scanNode(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if n.MieruInbounds, err = s.MieruInboundsForNode(ctx, id); err != nil {
		return nil, err
	}
	if n.Inbounds, err = s.InboundsForNode(ctx, id); err != nil {
		return nil, err
	}
	return n, nil
}

// List 列出全部节点,入站一次性取回后按机器分组挂上去。
//
// 逐节点再查一遍的话,10 台机器就是 10 次往返 —— 与节点列表的周期流量
// 走批量接口是同一条道理。
func (s *Store) List(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeColumns+nodeFrom+`WHERE n.deleted_at IS NULL ORDER BY n.sort_order, n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*Node
	byID := make(map[int64]*Node)
	for rows.Next() {
		n, err := s.scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
		byID[n.ID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	inbounds, err := s.AllInbounds(ctx)
	if err != nil {
		return nil, err
	}
	for _, in := range inbounds {
		if n, ok := byID[in.NodeID]; ok {
			n.Inbounds = append(n.Inbounds, in)
		}
	}
	// 与 sing-box 入站一样一次取全,不逐节点查 ——
	// 10 台机器逐个查就是 10 次往返。
	mierus, err := s.AllMieruInbounds(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mierus {
		if n, ok := byID[m.NodeID]; ok {
			n.MieruInbounds = append(n.MieruInbounds, m)
		}
	}
	return nodes, nil
}

// UpdateParams 是修改节点配置的参数。
//
// SSHKey 为空表示保持原私钥不变 —— 私钥从不回显给前端,前端也就无法把原值提交回来。
//
// 刻意不含 REALITY 握手目标与 short_id:握手目标必须经节点本机实测才能写入
// (CDN 按地域下发不同证书链,记录大小超过 8192 字节就静默握手失败),
// 修改它请走 ApplyHandshakeDest;从这里直接改会绕过那道实测。
type UpdateParams struct {
	Name        string
	DisplayName string
	Host        string
	// SubIPv4Address 与 IPv6Address 都是【留空表示清空,不是"保持原值"】。
	// 与下面几个字段的约定相反,因为把订阅地址改回跟随管理地址、
	// 把 IPv6 条目从订阅里撤下来都是管理员的显式动作,必须有办法表达。
	//
	// 代价是每一处编辑都必须回填这两栏,漏了就是静默清空 ——
	// 而清空 SubIPv4Address 的表现是全部用户的 IPv4 条目改指管理地址,
	// 那台机器如果管理口上没开代理端口,所有人当场断线。
	SubIPv4Address string
	IPv6Address    string
	SSHPort        int
	SSHUser        string
	SSHKey         string
	// APIPort 是这台机器上 V2Ray API 的回环端口,全部入站共用一个。
	APIPort int

	// TrafficQuotaBytes 为 nil 表示保持原额度。用指针是因为 0 本身有含义
	// (不限量),零值区分不出"没传"和"改成不限量"。
	TrafficQuotaBytes *int64
	// TrafficResetCycle 留空、TrafficResetDay 为 0 表示保持原值。
	TrafficResetCycle string
	TrafficResetDay   int
	// TrafficBillingMode 留空表示保持原值。
	//
	// 与 IPv6Address 那种"留空即清空"相反 —— 这一项没有"清空"的语义,
	// 它只有两个取值。漏传时若回落到 EGRESS,一台双向计费的机器会悄悄
	// 把用量显示成一半,而管理员看到的是额度绰绰有余。
	TrafficBillingMode string

	SortOrder int
	// SubscriptionEnabled 为 nil 表示保持原值,理由同上 ——
	// 漏传会把节点从所有人的订阅里摘掉,只有用户来反馈才会发现。
	SubscriptionEnabled *bool
	PublicRemark        string
	MaintenanceMessage  string
}

// UpdateEffect 描述一次修改带来的后果,调用方据此决定后续动作。
type UpdateEffect struct {
	// SSHChanged 为真时必须丢弃连接池中该节点的长连接,否则后续操作仍走旧地址。
	SSHChanged bool `json:"ssh_changed"`
	// NeedsDeploy 为真时节点上正在运行的配置已与期望状态不一致,需要重新部署。
	// 端口类变更不自动部署:部署会重启 sing-box 踢掉全部在线连接,
	// 何时切换由管理员配合 NAT/nginx 的改动决定。
	NeedsDeploy bool `json:"needs_deploy"`
	// **没有 TierChanged**:访问等级已经是入口的属性(迁移 0020),
	// 它由 InboundEffect 带出来。在这里留一个恒为 false 的字段,
	// 会让下一个人以为节点这一层还能改等级。
	//
	// RelayTargetChanged 为真时,以这个节点为落地的中转主机全部过时了。
	//
	// 依赖有两条:nginx 的 proxy_pass 指着这个节点的【地址与公网端口】,
	// 中转条目的协议参数取自这个节点。任何一项变了而不往下游传播,
	// 表现是中转机把流量转到一个没人监听的端口、或者用户拿到一套
	// 对不上的协议参数 —— **而面板上两台机器都显示正常**。
	//
	// 不细分是哪一项变了:判断"这次够不够格触发传播"本身就是会写漏的地方,
	// 而多一次 nginx reload 不打断任何人。
	RelayTargetChanged bool     `json:"relay_target_changed"`
	Changes            []string `json:"changes"`
}

// Update 修改节点配置。
func (s *Store) Update(ctx context.Context, id int64, p UpdateParams) (*Node, UpdateEffect, error) {
	// Changes 初始化成空切片而不是留 nil:nil 切片会序列化成 JSON 的 null,
	// 前端拿到 null 再取 .length 就直接抛异常。
	effect := UpdateEffect{Changes: []string{}}

	old, err := s.Get(ctx, id)
	if err != nil {
		return nil, effect, err
	}
	if err := validateName(p.Name); err != nil {
		return nil, effect, err
	}
	// 只有确实改动了这一栏才按 IPv4 字面量的新规矩校验。存量节点可能用域名接入,
	// 不然管理员改个端口都会被一条与本次操作无关的规则拦住。
	if p.Host, err = normalizeIPv4(p.Host); err != nil {
		return nil, effect, err
	}
	if p.SubIPv4Address, err = normalizeSubIPv4(p.SubIPv4Address); err != nil {
		return nil, effect, err
	}
	if p.IPv6Address, err = normalizeIPv6(p.IPv6Address); err != nil {
		return nil, effect, err
	}
	if p.TrafficQuotaBytes == nil {
		p.TrafficQuotaBytes = &old.TrafficQuotaBytes
	}
	if p.TrafficResetCycle == "" {
		p.TrafficResetCycle = old.TrafficResetCycle
	}
	if p.TrafficResetDay == 0 {
		p.TrafficResetDay = old.TrafficResetDay
	}
	if strings.TrimSpace(p.TrafficBillingMode) == "" {
		p.TrafficBillingMode = old.TrafficBillingMode
	}
	if err := normalizeTrafficQuota(p.TrafficQuotaBytes, &p.TrafficResetCycle,
		&p.TrafficResetDay, &p.TrafficBillingMode); err != nil {
		return nil, effect, err
	}
	if err := normalizeDisplayName(p.Name, &p.DisplayName); err != nil {
		return nil, effect, err
	}
	if err := normalizeSSH(&p.SSHPort, &p.SSHUser); err != nil {
		return nil, effect, err
	}
	if err := s.normalizeAPIPort(ctx, id, old, &p.APIPort); err != nil {
		return nil, effect, err
	}
	subEnabled := old.SubscriptionEnabled
	if p.SubscriptionEnabled != nil {
		subEnabled = *p.SubscriptionEnabled
	}
	if len([]rune(p.PublicRemark)) > 128 {
		return nil, effect, errors.New("公开备注不能超过 128 个字符")
	}
	if len([]rune(p.MaintenanceMessage)) > 128 {
		return nil, effect, errors.New("维护说明不能超过 128 个字符")
	}

	sshKeyEnc := ""
	if p.SSHKey != "" {
		if sshKeyEnc, err = s.cipher.Encrypt(p.SSHKey); err != nil {
			return nil, effect, fmt.Errorf("加密 SSH 私钥: %w", err)
		}
	}

	track := func(label string, changed bool, from, to any) {
		if changed {
			effect.Changes = append(effect.Changes, fmt.Sprintf("%s %v → %v", label, from, to))
		}
	}
	track("节点名称", old.Name != p.Name, old.Name, p.Name)
	track("展示名称", old.DisplayName != p.DisplayName, old.DisplayName, p.DisplayName)
	track("IPv4 地址", old.Host != p.Host, old.Host, p.Host)
	track("订阅 IPv4 地址", old.SubIPv4Address != p.SubIPv4Address,
		orFollowHost(old.SubIPv4Address), orFollowHost(p.SubIPv4Address))
	track("IPv6 地址", old.IPv6Address != p.IPv6Address,
		orNone(old.IPv6Address), orNone(p.IPv6Address))
	track("SSH 端口", old.SSHPort != p.SSHPort, old.SSHPort, p.SSHPort)
	track("SSH 用户", old.SSHUser != p.SSHUser, old.SSHUser, p.SSHUser)
	track("API 端口", old.APIPort != p.APIPort, old.APIPort, p.APIPort)
	track("排序", old.SortOrder != p.SortOrder, old.SortOrder, p.SortOrder)
	track("下发订阅", old.SubscriptionEnabled != subEnabled, old.SubscriptionEnabled, subEnabled)
	track("公开备注", old.PublicRemark != p.PublicRemark, old.PublicRemark, p.PublicRemark)
	track("维护说明", old.MaintenanceMessage != p.MaintenanceMessage,
		old.MaintenanceMessage, p.MaintenanceMessage)
	track("流量限额", old.TrafficQuotaBytes != *p.TrafficQuotaBytes,
		quotaLabel(old.TrafficQuotaBytes), quotaLabel(*p.TrafficQuotaBytes))
	track("重置周期", old.TrafficResetCycle != p.TrafficResetCycle,
		old.TrafficResetCycle, p.TrafficResetCycle)
	track("计费口径", old.TrafficBillingMode != p.TrafficBillingMode,
		billingLabel(old.TrafficBillingMode), billingLabel(p.TrafficBillingMode))
	// 重置日只在按月重置时有意义,不重置时改它没有任何效果,写进审计只会造成误解。
	track("每月重置日", p.TrafficResetCycle == string(traffic.CycleMonthly) &&
		old.TrafficResetDay != p.TrafficResetDay, old.TrafficResetDay, p.TrafficResetDay)
	if sshKeyEnc != "" {
		effect.Changes = append(effect.Changes, "已更换 SSH 私钥")
	}

	// IPv6 不在这里:它不参与 SSH,连接池里的连接仍然有效。
	// 把它算进来会在每次改 IPv6 时白白断掉一条已建立的长连接(建连约 1.3 秒)。
	effect.SSHChanged = old.Host != p.Host || old.SSHPort != p.SSHPort ||
		old.SSHUser != p.SSHUser || sshKeyEnc != ""
	// 只有进入节点配置的字段才需要重新部署。公网端口与节点名只影响订阅内容,
	// 主机地址只影响 SSH 与订阅,改了它们重启 sing-box 没有意义。
	//
	// 访问等级是例外:它不写进配置文件,却决定哪些用户会出现在这个节点上。
	// 等级调低会有一批用户凭空获得访问权(但节点上还没有他们的凭据),
	// 调高则会有一批用户的凭据滞留在节点上继续可用 —— 后者是安全问题。
	//
	// 订阅 IPv4、IPv6 与节点流量额度同样不进配置文件:前两者只改订阅内容,
	// 后者只用于统计与预警。为它们重启 sing-box 会把全部在线连接踢掉一次,
	// 换不来任何东西。订阅 IPv4 也不置 SSHChanged —— 管理通道仍然走 host,
	// 连接池里那条长连接照样有效,断掉它只是白白多一次 1.3 秒的重连。
	//
	// 入站那一侧的变更(协议、端口、TFO、入站等级)不在这里判 ——
	// 它们由 UpdateInbound 返回 InboundEffect,由上层各自处理。
	// 在两处都判一遍的话,判据迟早分叉,而分叉的表现是"改了却没标脏"。
	//
	// 访问等级也不在这里:它已经是入口的属性(迁移 0020)。
	effect.NeedsDeploy = old.APIPort != p.APIPort

	// 下游传播的判据与 NeedsDeploy 是两套:主机地址不进本机配置
	// (改了它重启 sing-box 没有意义),但它正是中转主机 proxy_pass 的目标
	// —— 那边必须跟着改。入站的公网端口与协议同理,由 UpdateInbound 负责传播。
	// 订阅 IPv4 必须算进来:中转的 proxy_pass 与链式出站指向的是落地的
	// 【对外落脚点】,而这一栏一填,那个落脚点就换了地址。不传播的话,
	// 中转机上的 nginx 会继续把流量送到旧地址 —— 而管理员刚刚才在
	// 落地那一页上改完并看到"已保存"。
	effect.RelayTargetChanged = old.Host != p.Host ||
		old.SubIPv4Address != p.SubIPv4Address ||
		old.IPv6Address != p.IPv6Address

	// SSH 私钥留空时保持原值:COALESCE 会因空串仍然是非 NULL 而失效,只能用 CASE。
	_, err = s.db.ExecContext(ctx, `
		UPDATE nodes
		   SET name = ?, display_name = ?, host = ?,
		       sub_ipv4_address = ?, ipv6_address = ?,
		       ssh_port = ?, ssh_user = ?,
		       ssh_key_encrypted = CASE WHEN ? = '' THEN ssh_key_encrypted ELSE ? END,
		       api_port = ?,
		       sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, maintenance_message = ?,
		       traffic_quota_bytes = ?, traffic_reset_cycle = ?, traffic_reset_day = ?,
		       traffic_billing_mode = ?,
		       updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, p.DisplayName, p.Host, p.SubIPv4Address, p.IPv6Address,
		p.SSHPort, p.SSHUser,
		sshKeyEnc, sshKeyEnc,
		p.APIPort,
		p.SortOrder, subEnabled,
		p.PublicRemark, p.MaintenanceMessage,
		*p.TrafficQuotaBytes, p.TrafficResetCycle, p.TrafficResetDay,
		p.TrafficBillingMode,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, effect, ErrNameConflict
		}
		return nil, effect, err
	}

	// 清空【订阅 IPv4】时刻意什么都不做,与下面 IPv6 那一段正好相反。
	//
	// ipv6_public_port 只为 IPv6 条目而存在,地址没了它就没有任何意义;
	// 而 public_port 在 NAT 机器上本来就独立于订阅 IP 存在(服务商映射的
	// 外部端口 ≠ 监听端口),跟着归零会把一台正常 NAT 机的订阅端口悄悄
	// 改成监听端口 —— 用户拿到一条连不上的条目,而面板一个错都不报。
	//
	// 清空 IPv6 地址时,这台机器上全部入站的 IPv6 公网端口一并归零。
	//
	// 留着它们,下次重新填上 IPv6 会静默套用几个月前的端口,而那些端口
	// 未必还转发着 —— 用户拿到的是连不上的条目,面板一个错都不报。
	// 这一条 V2.1 就定下了,多入站之后跨了两张表,所以要显式写出来。
	if p.IPv6Address == "" && old.IPv6Address != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE node_inbounds SET ipv6_public_port = 0, updated_at = ?
			  WHERE node_id = ? AND deleted_at IS NULL AND ipv6_public_port != 0`,
			time.Now().UTC().Format(time.RFC3339), id); err != nil {
			return nil, effect, err
		}
	}

	updated, err := s.Get(ctx, id)
	return updated, effect, err
}

// PinHostKey 固定节点的 SSH 主机公钥。已固定且不一致时拒绝覆盖。
func (s *Store) PinHostKey(ctx context.Context, id int64, hostKey string) error {
	var existing string
	if err := s.db.QueryRowContext(ctx,
		`SELECT ssh_host_key FROM nodes WHERE id = ?`, id).Scan(&existing); err != nil {
		return err
	}
	if existing != "" {
		if existing == hostKey {
			return nil
		}
		return fmt.Errorf("%w,新旧密钥不一致,需人工确认后重置", ErrHostKeyPinned)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET ssh_host_key = ?, updated_at = ? WHERE id = ?`,
		hostKey, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// ResetHostKey 清空已固定的主机密钥,用于节点重装后由管理员显式确认。
func (s *Store) ResetHostKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET ssh_host_key = '', updated_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SetConfigInRAM 只改这一列。
//
// 不走通用的 Update:那条路会跑一遍全量校验并算出「哪些字段变了」,
// 而这一项的切换是一个自带顺序与回滚的复合操作(见 Service.SetConfigInRAM),
// 它需要能把这一列单独改回去。
func (s *Store) SetConfigInRAM(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET config_in_ram = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		enabled, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SaveProbe 保存节点探测结果。
// SaveProbe 保存节点探测结果。
//
// memTotalMB 为 0 时不覆盖已有值:探测偶尔读不到 /proc/meminfo(极少见),
// 把它写成 0 会让 udp_timeout 那一项从配置里消失,于是节点凭空变成「待部署」,
// 部署下去又把它加回来 —— 一次读取抖动换来两次全节点重启。
func (s *Store) SaveProbe(
	ctx context.Context, id int64, arch, version, buildTags string, memTotalMB int, usable bool,
) error {
	status := StatusOnline
	if !usable {
		status = StatusOffline
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET arch = ?, singbox_version = ?, singbox_build_tags = ?,
			mem_total_mb = CASE WHEN ? > 0 THEN ? ELSE mem_total_mb END,
			status = CASE WHEN status = 'DISABLED' THEN status ELSE ? END,
			last_heartbeat_at = ?, updated_at = ?
		WHERE id = ?`,
		arch, version, buildTags, memTotalMB, memTotalMB, status, now, now, id)
	return err
}

// normalizeAPIPort 归一化 V2Ray API 端口,并确认它没被这台机器上的入站占用。
//
// 冲突的后果是 sing-box 起不来,而它要到部署的健康检查才暴露 ——
// 那时配置已经换过去了,只能靠回滚救回来。
func (s *Store) normalizeAPIPort(ctx context.Context, id int64, old *Node, port *int) error {
	if *port == 0 {
		*port = old.APIPort
	}
	// 中转机上没有 sing-box,API 端口一直是 0,不必也不能按端口校验。
	if old.Role.IsRelay() {
		*port = 0
		return nil
	}
	if err := singbox.ValidatePort(*port, "V2Ray API"); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_inbounds
		  WHERE node_id = ? AND listen_port = ? AND deleted_at IS NULL`,
		id, *port).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("V2Ray API 端口 %d 已被这台机器上的一个入站占用", *port)
	}
	return nil
}

// NextRevision 原子地递增并返回节点的配置版本号。
func (s *Store) NextRevision(ctx context.Context, id int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var revision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT config_revision FROM nodes WHERE id = ?`, id).Scan(&revision); err != nil {
		return 0, err
	}
	revision++
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET config_revision = ?, updated_at = ? WHERE id = ?`,
		revision, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return 0, err
	}
	return revision, tx.Commit()
}

// DeployedInbound 是一个入站在这次部署里真正下发到节点上的样子。
type DeployedInbound struct {
	ID          int64
	Protocol    singbox.Protocol
	SSMethod    string
	TCPFastOpen bool
	// Snell 的两项。psk 没有 deployed_ 镜像 —— 它建入口时生成一次,
	// 之后没有任何路径会改它,与 ss_password / reality_privkey 同。
	SnellVersion   int
	SnellObfsMode  string
	SnellV6Mode    string
	SnellSharedPSK bool
	// Unmetered 是这次下发时这个入口有没有计量(V15),采集按它判。
	Unmetered bool
}

// MarkDeployed 记录部署成功后的配置哈希、各入站的生效参数与节点状态。
//
// deployed_* 只在这里写入 —— 它们回答的是"节点上现在跑的是什么",
// 而订阅与门户只信这个答案。部署失败回滚后这些列保持原值,
// 于是订阅继续下发那份仍然能连的旧条目。
//
// **不在这次配置里的入站要把 deployed_* 清空。** 一个被停用或删掉的入站,
// 部署成功之后节点上就没有它了;不清的话它会继续留在所有人的订阅里,
// 而客户端连过去无人应答 —— 订阅、数据库、节点三方各说各话。
func (s *Store) MarkDeployed(
	ctx context.Context, id int64, sha256 string, inbounds []DeployedInbound,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		UPDATE nodes SET deployed_config_sha256 = ?,
		       status = ?, last_heartbeat_at = ?, updated_at = ?
		WHERE id = ? AND status != 'DISABLED'`,
		sha256, StatusOnline, now, now, id); err != nil {
		return err
	}

	live := make(map[int64]bool, len(inbounds))
	for _, in := range inbounds {
		live[in.ID] = true
		if _, err := tx.ExecContext(ctx, `
			UPDATE node_inbounds SET deployed_protocol = ?, deployed_ss_method = ?,
			       deployed_tcp_fast_open = ?,
			       deployed_snell_version = ?, deployed_snell_obfs_mode = ?,
			       deployed_snell_v6_mode = ?, deployed_snell_shared_psk = ?,
			       deployed_unmetered = ?,
			       updated_at = ?
			 WHERE id = ? AND node_id = ?`,
			string(in.Protocol), in.SSMethod, in.TCPFastOpen,
			in.SnellVersion, in.SnellObfsMode, in.SnellV6Mode, in.SnellSharedPSK,
			in.Unmetered,
			now, in.ID, id); err != nil {
			return err
		}
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM node_inbounds WHERE node_id = ? AND deployed_protocol != ''`, id)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var inboundID int64
		if err := rows.Scan(&inboundID); err != nil {
			rows.Close()
			return err
		}
		if !live[inboundID] {
			stale = append(stale, inboundID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, inboundID := range stale {
		if _, err := tx.ExecContext(ctx, `
			UPDATE node_inbounds SET deployed_protocol = '', deployed_ss_method = '',
			       deployed_tcp_fast_open = 0, deployed_snell_version = 0,
			       deployed_snell_obfs_mode = '', deployed_snell_v6_mode = '',
			       deployed_snell_shared_psk = 0, deployed_unmetered = 0, updated_at = ?
			 WHERE id = ?`, now, inboundID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// MarkDeployFailed 把节点标记为部署失败。
func (s *Store) MarkDeployFailed(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET status = ?, updated_at = ? WHERE id = ? AND status != 'DISABLED'`,
		StatusDeployFailed, now, id)
	return err
}

// SetEnabled 启用或禁用节点。
func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	status := StatusDisabled
	if enabled {
		status = StatusPending
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// Delete 软删除节点。
//
// 同时给名称加上删除标记:name 列有 UNIQUE 约束且不区分是否已删除,
// 不改名的话被删节点会永久占住这个名字,管理员再也无法用回它。
// 加后缀而不是清空,是为了让残留在数据库里的行仍然可读。
func (s *Store) Delete(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE nodes
		   SET deleted_at = ?, updated_at = ?,
		       name = name || ' (已删除#' || id || ')'
		 WHERE id = ? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// orNone 让审计日志里的空值读起来像句话,而不是 " → 2602:...:1"。
// orFollowHost 把空串写成「跟随管理地址」而不是「(未配置)」——
// 这一栏留空不是"没配",它有明确的含义:IPv4 条目就用 nodes.host。
// 审计里写成"未配置"会让人以为那台机器的订阅里没有 IPv4 条目。
func orFollowHost(v string) string {
	if v == "" {
		return "(跟随管理地址)"
	}
	return v
}

func orNone(v string) string {
	if v == "" {
		return "(未配置)"
	}
	return v
}

// ipv6PortLabel 把 0 写成「跟随 IPv4」而不是「0」——
// 审计里出现「IPv6 公网端口 0 → 8443」没人看得懂 0 是什么意思。
func ipv6PortLabel(port int) string {
	if port == 0 {
		return "跟随 IPv4"
	}
	return strconv.Itoa(port)
}

func quotaLabel(bytes int64) string {
	if bytes <= 0 {
		return "不限量"
	}
	return fmt.Sprintf("%d 字节", bytes)
}

// billingLabel 把计费口径译成审计日志里能直接读懂的话。
// 记 EGRESS/BOTH 的话,几个月后翻审计日志的人还要再去查一遍这两个词的含义。
func billingLabel(mode string) string {
	if traffic.BillingMode(mode) == traffic.BillingBoth {
		return "双向计费(进出合计,×2)"
	}
	return "出站计费(×1)"
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// SaveSingBoxChannel 记录这台机器上装的是哪一支 sing-box(V14)。
//
// **只由 InstallBinary 与 Uninstall 调用。** 它描述的是"机器上那个文件
// 是哪一版",不是一个可以单独编辑的设置 —— 让它变成表单里的一栏,
// 就会出现"库里写着预览版、机器上是正式版"的状态,而那个状态下
// Snell 入口保存得进去、部署到一半失败并回滚。
func (s *Store) SaveSingBoxChannel(ctx context.Context, id int64, channel SingBoxChannel) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET singbox_channel = ?, updated_at = ? WHERE id = ?`,
		string(channel), time.Now().UTC().Format(time.RFC3339), id)
	return err
}
