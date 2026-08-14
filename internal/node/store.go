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
	// Host 是 IPv4 地址,同时是 SSH 管理地址与 IPv4 订阅地址。
	// IPv6Address 只进订阅:SSH、探测、安装、部署、重启、流量同步与资源采集
	// 一律走 Host,双栈里只留一条管理通道才不会出现"两条路两种结论"。
	Host        string `json:"host"`
	IPv6Address string `json:"ipv6_address"`
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SSHKey      string `json:"-"` // PEM 私钥,永不出现在 API 响应中
	HostKey     string `json:"-"`
	// ProxyPort 是客户端连接的公网端口,只写进订阅;
	// ListenPort 是 sing-box 在节点上实际监听的端口。
	// NAT 主机或自建 nginx 转发时两者不同(公网 443 -> 主机 20443)。
	ProxyPort  int `json:"proxy_port"`
	ListenPort int `json:"listen_port"`
	APIPort    int `json:"api_port"`
	// IPv6ProxyPort 是 IPv6 条目在订阅里用的公网端口,0 表示跟随 ProxyPort。
	// 双栈机器的两个协议栈未必映射到同一个外部端口(NAT 小鸡上 IPv4 常是
	// 服务商映射的高位端口,而 IPv6 是直连的 443)。
	//
	// **0 要原样留着,不在这里解析成 ProxyPort** —— 解析放在订阅生成时,
	// 否则以后改 ProxyPort,IPv6 条目会停在旧端口上而管理员毫不知情。
	IPv6ProxyPort int `json:"ipv6_proxy_port"`

	Arch           string `json:"arch"`
	SingBoxVersion string `json:"singbox_version"`
	BuildTags      string `json:"singbox_build_tags"`

	RealityDest       string `json:"reality_dest"`
	RealityDestPort   int    `json:"reality_dest_port"`
	RealityPrivateKey string `json:"-"`
	RealityPublicKey  string `json:"reality_public_key"`
	RealityShortID    string `json:"reality_short_id"`

	HandshakeMaxRecordSize int     `json:"handshake_max_record_size"`
	HandshakeCheckedAt     *string `json:"handshake_checked_at"`

	// AccessTier* 是节点所属访问等级:等级不高于用户等级的节点会被自动继承。
	AccessTierID    int64  `json:"access_tier_id"`
	AccessTierCode  string `json:"access_tier_code"`
	AccessTierName  string `json:"access_tier_name"`
	AccessTierLevel int    `json:"access_tier_level"`

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

const nodeColumns = `n.id, n.name, n.display_name, n.host, n.ipv6_address, n.ssh_port, n.ssh_user,
	n.ssh_key_encrypted, n.ssh_host_key,
	n.proxy_port, n.listen_port, n.api_port, n.ipv6_proxy_port,
	n.arch, n.singbox_version, n.singbox_build_tags,
	n.reality_dest, n.reality_dest_port, n.reality_privkey_encrypted, n.reality_pubkey,
	n.reality_short_id, n.handshake_max_record_size, n.handshake_checked_at,
	n.access_tier_id, t.code, t.name, t.level,
	n.sort_order, n.subscription_enabled, n.public_remark, n.maintenance_message,
	n.traffic_quota_bytes, n.traffic_reset_cycle, n.traffic_reset_day,
	n.traffic_billing_mode,
	n.status, n.last_heartbeat_at, n.config_revision, n.deployed_config_sha256,
	n.created_at, n.updated_at`

// nodeFrom 固定带上等级表。等级是节点的必备属性,分两次查会出现
// "列表里有、详情里没有"这种前端只能靠猜的差异。
const nodeFrom = ` FROM nodes n JOIN access_tiers t ON t.id = n.access_tier_id `

func (s *Store) scanNode(scan func(dest ...any) error) (*Node, error) {
	var n Node
	var sshKeyEnc, realityKeyEnc string
	err := scan(
		&n.ID, &n.Name, &n.DisplayName, &n.Host, &n.IPv6Address,
		&n.SSHPort, &n.SSHUser, &sshKeyEnc, &n.HostKey,
		&n.ProxyPort, &n.ListenPort, &n.APIPort, &n.IPv6ProxyPort,
		&n.Arch, &n.SingBoxVersion, &n.BuildTags,
		&n.RealityDest, &n.RealityDestPort, &realityKeyEnc, &n.RealityPublicKey,
		&n.RealityShortID, &n.HandshakeMaxRecordSize, &n.HandshakeCheckedAt,
		&n.AccessTierID, &n.AccessTierCode, &n.AccessTierName, &n.AccessTierLevel,
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
	if realityKeyEnc != "" {
		if n.RealityPrivateKey, err = s.cipher.Decrypt(realityKeyEnc); err != nil {
			return nil, fmt.Errorf("解密节点 %d 的 REALITY 私钥: %w", n.ID, err)
		}
	}
	return &n, nil
}

// CreateParams 是新增节点所需的参数。
type CreateParams struct {
	Name string
	// DisplayName 留空时复制 Name。订阅里的节点名不能为空:
	// 客户端拿它识别条目,空名字会让用户面对一列无法区分的节点。
	DisplayName string
	// Host 必须是 IPv4;IPv6Address 选填,留空表示该节点只有 IPv4。
	Host        string
	IPv6Address string
	SSHPort     int
	SSHUser     string
	SSHKey      string
	// AccessTierID 留 0 表示普通组。
	AccessTierID int64
	SortOrder    int
	// TrafficQuotaBytes 留 0 表示不限量;TrafficResetCycle 留空按 NONE;
	// TrafficBillingMode 留空按 EGRESS(与升级前的行为一致)。
	TrafficQuotaBytes  int64
	TrafficResetCycle  string
	TrafficResetDay    int
	TrafficBillingMode string
	// ProxyPort 是客户端连接的公网端口,必填。
	// ListenPort 是 sing-box 在主机上监听的端口,留空表示不做端口转发,与 ProxyPort 相同。
	ProxyPort  int
	ListenPort int
	APIPort    int
	// IPv6ProxyPort 留 0 表示 IPv6 条目跟随 ProxyPort。
	IPv6ProxyPort int
	// RealityDest 为空时使用默认候选目标的第一个。
	RealityDest     string
	RealityDestPort int
}

// Create 新增节点,同时生成 REALITY 密钥对与 short_id。
func (s *Store) Create(ctx context.Context, p CreateParams) (*Node, error) {
	if err := validateCreate(&p); err != nil {
		return nil, err
	}
	// 迁移里没给 access_tier_id 写外键(SQLite 的 ADD COLUMN 限制),
	// 这道校验就是唯一的拦截点 —— 指向不存在的等级会让节点从所有
	// 有效节点查询里消失(视图是 INNER JOIN),表现为"节点在,但谁都用不到"。
	if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
		return nil, err
	}

	keys, err := GenerateRealityKeyPair()
	if err != nil {
		return nil, err
	}
	shortID, err := GenerateShortID(8)
	if err != nil {
		return nil, err
	}

	// 空私钥直接存空串而不是加密后的空串:读取侧用"是否为空"判断
	// 该节点是否用面板密钥,加密后的空串不为空,会被当成一把解不开的私钥。
	sshKeyEnc := ""
	if p.SSHKey != "" {
		if sshKeyEnc, err = s.cipher.Encrypt(p.SSHKey); err != nil {
			return nil, fmt.Errorf("加密 SSH 私钥: %w", err)
		}
	}
	realityKeyEnc, err := s.cipher.Encrypt(keys.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("加密 REALITY 私钥: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (name, display_name, host, ipv6_address, ssh_port, ssh_user,
			ssh_key_encrypted, ssh_host_key, proxy_port, listen_port, api_port, ipv6_proxy_port,
			reality_dest, reality_dest_port,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			access_tier_id, sort_order,
			traffic_quota_bytes, traffic_reset_cycle, traffic_reset_day, traffic_billing_mode,
			status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,'',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.DisplayName, p.Host, p.IPv6Address, p.SSHPort, p.SSHUser, sshKeyEnc,
		p.ProxyPort, p.ListenPort, p.APIPort, p.IPv6ProxyPort,
		p.RealityDest, p.RealityDestPort,
		realityKeyEnc, keys.PublicKey, shortID,
		p.AccessTierID, p.SortOrder,
		p.TrafficQuotaBytes, p.TrafficResetCycle, p.TrafficResetDay, p.TrafficBillingMode,
		StatusPending, now, now)
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
	return s.Get(ctx, id)
}

func validateCreate(p *CreateParams) error {
	var err error
	if err = validateName(p.Name); err != nil {
		return err
	}
	// 新建节点一律要求 IPv4 字面量。
	if p.Host, err = normalizeIPv4(p.Host, true); err != nil {
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
	if p.AccessTierID == 0 {
		p.AccessTierID = access.TierNormalID
	}
	if err := normalizeSSH(&p.SSHPort, &p.SSHUser); err != nil {
		return err
	}
	// SSH 私钥可以留空:留空表示这个节点用面板专用密钥,
	// 由 Service.Bootstrap 在创建后把面板公钥装进节点。
	if err := normalizePorts(&p.ProxyPort, &p.ListenPort, &p.APIPort); err != nil {
		return err
	}
	if err := normalizeIPv6Port(p.IPv6Address, &p.IPv6ProxyPort); err != nil {
		return err
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

func (s *Store) Get(ctx context.Context, id int64) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+nodeFrom+`WHERE n.id = ? AND n.deleted_at IS NULL`, id)
	n, err := s.scanNode(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (s *Store) List(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeColumns+nodeFrom+`WHERE n.deleted_at IS NULL ORDER BY n.sort_order, n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []*Node
	for rows.Next() {
		n, err := s.scanNode(rows.Scan)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
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
	// IPv6Address 留空表示清空 IPv6,不是"保持原值"。
	// 与下面几个字段的约定相反,因为清空 IPv6 是管理员的显式动作
	// (把订阅里的 IPv6 条目撤下来),必须有办法表达。
	IPv6Address string
	SSHPort     int
	SSHUser     string
	SSHKey      string
	ProxyPort   int
	ListenPort  int
	APIPort     int
	// IPv6ProxyPort 为 0 表示 IPv6 条目跟随 ProxyPort,与 IPv6Address 一样
	// 是"留空即清空"而不是"保持原值" —— 它只有在 IPv6Address 非空时才有意义,
	// 两个字段总是一起提交,不存在漏传一个的情况。
	IPv6ProxyPort int

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

	// AccessTierID 为 0 表示保持原等级。不回落到普通组:
	// 前端漏传这个字段时把 VIP 节点悄悄降成普通组,等于给全体用户开门,
	// 而且不报任何错。
	AccessTierID int64
	SortOrder    int
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
	// TierChanged 为真时节点上的用户集合已经变了,必须自动标脏重新部署。
	// 这一条不能交给管理员挑时机:等级调高后,被移出的用户凭据还留在节点上,
	// 拖多久就多能用多久 —— 那是权限没有真正收回。
	TierChanged bool     `json:"tier_changed"`
	Changes     []string `json:"changes"`
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
	if p.Host, err = normalizeIPv4(p.Host, strings.TrimSpace(p.Host) != old.Host); err != nil {
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
	if err := normalizePorts(&p.ProxyPort, &p.ListenPort, &p.APIPort); err != nil {
		return nil, effect, err
	}
	if err := normalizeIPv6Port(p.IPv6Address, &p.IPv6ProxyPort); err != nil {
		return nil, effect, err
	}
	if p.AccessTierID == 0 {
		p.AccessTierID = old.AccessTierID
	}
	if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
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
	track("IPv6 地址", old.IPv6Address != p.IPv6Address,
		orNone(old.IPv6Address), orNone(p.IPv6Address))
	track("SSH 端口", old.SSHPort != p.SSHPort, old.SSHPort, p.SSHPort)
	track("SSH 用户", old.SSHUser != p.SSHUser, old.SSHUser, p.SSHUser)
	track("公网代理端口", old.ProxyPort != p.ProxyPort, old.ProxyPort, p.ProxyPort)
	track("主机代理端口", old.ListenPort != p.ListenPort, old.ListenPort, p.ListenPort)
	track("API 端口", old.APIPort != p.APIPort, old.APIPort, p.APIPort)
	track("IPv6 公网端口", old.IPv6ProxyPort != p.IPv6ProxyPort,
		ipv6PortLabel(old.IPv6ProxyPort), ipv6PortLabel(p.IPv6ProxyPort))
	track("访问等级", old.AccessTierID != p.AccessTierID, old.AccessTierID, p.AccessTierID)
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
	// IPv6 与节点流量额度同样不进配置文件:前者只改订阅内容,后者只用于
	// 统计与预警。为它们重启 sing-box 会把全部在线连接踢掉一次,换不来任何东西。
	effect.TierChanged = old.AccessTierID != p.AccessTierID
	effect.NeedsDeploy = old.ListenPort != p.ListenPort || old.APIPort != p.APIPort ||
		effect.TierChanged

	// SSH 私钥留空时保持原值:COALESCE 会因空串仍然是非 NULL 而失效,只能用 CASE。
	_, err = s.db.ExecContext(ctx, `
		UPDATE nodes
		   SET name = ?, display_name = ?, host = ?, ipv6_address = ?,
		       ssh_port = ?, ssh_user = ?,
		       ssh_key_encrypted = CASE WHEN ? = '' THEN ssh_key_encrypted ELSE ? END,
		       proxy_port = ?, listen_port = ?, api_port = ?, ipv6_proxy_port = ?,
		       access_tier_id = ?, sort_order = ?, subscription_enabled = ?,
		       public_remark = ?, maintenance_message = ?,
		       traffic_quota_bytes = ?, traffic_reset_cycle = ?, traffic_reset_day = ?,
		       traffic_billing_mode = ?,
		       updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, p.DisplayName, p.Host, p.IPv6Address,
		p.SSHPort, p.SSHUser,
		sshKeyEnc, sshKeyEnc,
		p.ProxyPort, p.ListenPort, p.APIPort, p.IPv6ProxyPort,
		p.AccessTierID, p.SortOrder, subEnabled,
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

// SaveProbe 保存节点探测结果。
func (s *Store) SaveProbe(ctx context.Context, id int64, arch, version, buildTags string, usable bool) error {
	status := StatusOnline
	if !usable {
		status = StatusOffline
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET arch = ?, singbox_version = ?, singbox_build_tags = ?,
			status = CASE WHEN status = 'DISABLED' THEN status ELSE ? END,
			last_heartbeat_at = ?, updated_at = ?
		WHERE id = ?`,
		arch, version, buildTags, status, now, now, id)
	return err
}

// SaveDestCheck 保存握手目标检测结果。
func (s *Store) SaveDestCheck(ctx context.Context, id int64, dest string, destPort, maxRecord int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET reality_dest = ?, reality_dest_port = ?,
			handshake_max_record_size = ?, handshake_checked_at = ?, updated_at = ?
		WHERE id = ?`,
		dest, destPort, maxRecord, now, now, id)
	return err
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

// MarkDeployed 记录部署成功后的配置哈希与状态。
func (s *Store) MarkDeployed(ctx context.Context, id int64, sha256 string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET deployed_config_sha256 = ?, status = ?, last_heartbeat_at = ?, updated_at = ?
		WHERE id = ? AND status != 'DISABLED'`,
		sha256, StatusOnline, now, now, id)
	return err
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
