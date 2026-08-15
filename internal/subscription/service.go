package subscription

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/user"
)

var (
	ErrNotFound = errors.New("订阅不存在")
	// ErrNotServiceable 表示用户存在但当前不可用(停用、过期或超额)。
	// 与 ErrNotFound 区分开,以便给出可读的原因 —— 静默返回空列表
	// 会让客户端把已有节点全部清空,用户完全不知道发生了什么。
	ErrNotServiceable = errors.New("用户当前不可用")
)

// Format 是订阅输出格式。
type Format string

const (
	// FormatBase64 是 v2rayN、Shadowrocket 等客户端的通用格式。
	FormatBase64 Format = "base64"
	// FormatURI 输出未编码的 URI,便于人工核对。
	FormatURI Format = "uri"
	// FormatSingBox 输出完整的 sing-box 客户端配置。
	FormatSingBox Format = "sing-box"
)

// ParseFormat 解析格式参数,未知值回落到 base64。
func ParseFormat(raw string) Format {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "uri", "plain", "text":
		return FormatURI
	case "sing-box", "singbox", "json":
		return FormatSingBox
	default:
		return FormatBase64
	}
}

// Result 是一次订阅请求的产物。
type Result struct {
	Body        []byte
	ContentType string
	// UserInfo 是 Subscription-Userinfo 响应头的值,
	// 客户端用它显示已用流量与到期时间。
	UserInfo  string
	NodeCount int
	UserCode  string
	Filename  string
}

// Service 组装订阅内容。
type Service struct {
	db     *sql.DB
	users  *user.Store
	cipher *crypto.Cipher
	// mixedPort 是 sing-box 客户端配置里本地混合入站的端口。
	mixedPort int
	logger    *slog.Logger
}

func NewService(
	db *sql.DB, users *user.Store, cipher *crypto.Cipher, mixedPort int, logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{db: db, users: users, cipher: cipher, mixedPort: mixedPort, logger: logger}
}

// Build 按订阅 Token 生成内容。
//
// token 是明文;查找走 SHA-256 哈希,这条路径不解密任何字段。
func (s *Service) Build(ctx context.Context, token string, format Format) (Result, error) {
	u, err := s.users.GetBySubTokenHash(ctx, crypto.HashToken(token))
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return Result{}, ErrNotFound
		}
		return Result{}, err
	}

	if !u.Serviceable(time.Now().UTC()) {
		return Result{}, fmt.Errorf("%w:%s", ErrNotServiceable, statusReason(u))
	}

	nodes, err := s.nodesFor(ctx, u.ID)
	if err != nil {
		return Result{}, err
	}

	entries := s.entriesFor(Credentials{UUID: u.UUID, SSPassword: u.SSPassword}, nodes)

	result := Result{
		NodeCount: len(entries),
		UserCode:  u.UserCode,
		UserInfo:  userInfoHeader(u),
		Filename:  u.UserCode,
	}

	switch format {
	case FormatSingBox:
		body, err := SingBoxClientConfig(entries, s.mixedPort)
		if err != nil {
			return Result{}, err
		}
		result.Body = body
		result.ContentType = "application/json; charset=utf-8"
		result.Filename = u.UserCode + ".json"
	case FormatURI:
		result.Body = []byte(strings.Join(uriList(entries), "\n"))
		result.ContentType = "text/plain; charset=utf-8"
	default:
		joined := strings.Join(uriList(entries), "\n")
		encoded := base64.StdEncoding.EncodeToString([]byte(joined))
		result.Body = []byte(encoded)
		result.ContentType = "text/plain; charset=utf-8"
	}
	return result, nil
}

// entriesFor 把节点列表转成订阅条目。
//
// 单个节点转换失败时跳过它并记一条错误日志,而不是让整份订阅失败:
// 订阅失败会让客户端把【已有节点全部清空】,用户完全不知道发生了什么,
// 而问题可能只出在一个刚加进来的节点上。这与 ErrNotServiceable
// 不静默返回空列表是同一条道理。
//
// 正常路径上转换不会失败 —— 凭据在建用户/建节点时生成,存量行由启动
// backfill 补齐。真的走到这里说明数据有问题,那需要管理员去看日志。
func (s *Service) entriesFor(cred Credentials, nodes []Node) []Entry {
	entries := make([]Entry, 0, len(nodes))
	for _, node := range nodes {
		entry, err := EntryFor(cred, node)
		if err != nil {
			s.logger.Error("生成订阅条目失败,已跳过该节点",
				"node", node.DisplayName, "protocol", node.Protocol, "error", err)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func uriList(entries []Entry) []string {
	uris := make([]string, 0, len(entries))
	for _, entry := range entries {
		uris = append(uris, entry.URI)
	}
	return uris
}

// nodesFor 返回该用户订阅中应当出现的节点。
//
// 归属关系走 access 的有效节点视图(等级继承 + 额外授权),与节点配置生成
// 用的是同一份定义 —— 两处一旦分叉,用户会拿到一个节点上并没有他凭据的条目。
//
// 附加过滤条件:
//   - 节点未被软删除;
//   - 节点未被管理员禁用;
//   - subscription_enabled 为真(节点进维护时管理员可临时下架);
//   - 节点至少成功部署过一次(deployed_config_sha256 非空)。
//
// 最后一条是关键:未部署过的节点上根本没有该用户的凭据,
// 下发给客户端只会得到一个连不上的条目。
//
// 刻意不排除 OFFLINE 状态的节点 —— 那多半是一次同步失败造成的瞬时状态,
// 把它从订阅里摘掉会让客户端在节点恢复后仍然缺少该节点。
//
// 只取 display_name:内部名称往往写着机房、供应商与到期日,
// 那是运维信息,不该随订阅发到用户设备上。
//
// IPv6 在读出物理节点之后由 ExpandAll 展开,不在 SQL 里用 UNION 造虚拟行:
// 上面这一串过滤条件只要写两遍就会分叉,而分叉的表现是节点已经进维护、
// IPv4 条目消失了,IPv6 条目却还留在订阅里继续被使用。
//
// 【协议取 deployed_protocol 而不是 protocol】。管理员改协议到部署成功之间
// 存在一个窗口 —— 可能二十秒,可能是他改完就去忙别的的两小时,
// 也可能是部署失败自动回滚之后的永远。按期望值渲染的话,这个窗口里
// 用户拉到 ss:// 而节点上跑的还是 VLESS,客户端握手失败,
// 而数据库、节点、面板三方都是"对的",只有订阅站在中间说了假话。
//
// 与「至少成功部署过一次」那条过滤是同一个思想:
// 订阅只描述节点上真实存在的东西。
func (s *Service) nodesFor(ctx context.Context, userID int64) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.display_name, n.host, n.ipv6_address, n.proxy_port, n.ipv6_proxy_port,
		       n.deployed_protocol, n.deployed_ss_method, n.ss_password_encrypted,
		       n.reality_dest, n.reality_pubkey, n.reality_short_id
		  FROM nodes n
		  JOIN `+access.EffectiveNodesView+` en ON en.node_id = n.id
		 WHERE en.proxy_user_id = ?
		   AND n.deleted_at IS NULL
		   AND n.status != 'DISABLED'
		   AND n.subscription_enabled = 1
		   AND n.deployed_config_sha256 != ''
		 ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	physical := make([]PhysicalNode, 0)
	for rows.Next() {
		var p PhysicalNode
		var protocol, ssMethod, ssKeyEnc string
		if err := rows.Scan(&p.DisplayName, &p.Host, &p.IPv6Address, &p.Port, &p.IPv6Port,
			&protocol, &ssMethod, &ssKeyEnc,
			&p.RealityDest, &p.RealityPublicKey, &p.RealityShortID); err != nil {
			return nil, err
		}
		// 解析失败回落到 VLESS:这一列的值只由 MarkDeployed 写入,
		// 出现未知值说明库被人手工改过。回落而不是报错,是因为报错会让
		// 整份订阅失败,把用户客户端里的节点全部清空。
		p.Protocol, _ = singbox.ParseProtocol(protocol)
		p.SSMethod = singbox.SSMethod(ssMethod)
		if p.Protocol == singbox.ProtocolShadowsocks && ssKeyEnc != "" {
			if p.SSServerKey, err = s.cipher.Decrypt(ssKeyEnc); err != nil {
				return nil, fmt.Errorf("解密节点 %s 的 Shadowsocks 密钥: %w", p.DisplayName, err)
			}
		}
		physical = append(physical, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ExpandAll(physical), nil
}

// userInfoHeader 生成 Subscription-Userinfo 头。
//
// 这是机场客户端的事实标准:客户端解析后在界面上显示流量与到期。
// total=0 表示不限量,expire 省略表示不过期。
func userInfoHeader(u *user.User) string {
	parts := []string{
		fmt.Sprintf("upload=%d", u.UsedUplink),
		fmt.Sprintf("download=%d", u.UsedDownlink),
		fmt.Sprintf("total=%d", u.QuotaBytes),
	}
	if u.ExpiresAt != nil && *u.ExpiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, *u.ExpiresAt); err == nil {
			parts = append(parts, fmt.Sprintf("expire=%d", exp.Unix()))
		}
	}
	return strings.Join(parts, "; ")
}

func statusReason(u *user.User) string {
	switch u.Status {
	case user.StatusDisabled:
		return "账号已被停用"
	case user.StatusExpired:
		return "账号已过期"
	case user.StatusQuotaExceeded:
		return "流量已用尽"
	}
	if u.Expired(time.Now().UTC()) {
		return "账号已过期"
	}
	if u.QuotaExceeded() {
		return "流量已用尽"
	}
	return "账号当前不可用"
}

// RecordAccess 记录一次订阅拉取。
//
// 只保留最近一次:客户端会周期性拉取,逐次入库会让表无节制增长。
// 失败不影响订阅返回 —— 记录访问是运维便利,不是业务必需。
func (s *Service) RecordAccess(ctx context.Context, userCode, clientIP, userAgent string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_users
		   SET sub_last_access_at = ?, sub_last_access_ip = ?,
		       sub_last_user_agent = ?, sub_access_count = sub_access_count + 1
		 WHERE user_code = ?`,
		time.Now().UTC().Format(time.RFC3339), clientIP, truncate(userAgent, 200), userCode)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
