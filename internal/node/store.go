package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/singbox"
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
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	SSHPort int    `json:"ssh_port"`
	SSHUser string `json:"ssh_user"`
	SSHKey  string `json:"-"` // PEM 私钥,永不出现在 API 响应中
	HostKey string `json:"-"`
	// ProxyPort 是客户端连接的公网端口,只写进订阅;
	// ListenPort 是 sing-box 在节点上实际监听的端口。
	// NAT 主机或自建 nginx 转发时两者不同(公网 443 -> 主机 20443)。
	ProxyPort  int `json:"proxy_port"`
	ListenPort int `json:"listen_port"`
	APIPort    int `json:"api_port"`

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

const nodeColumns = `id, name, host, ssh_port, ssh_user, ssh_key_encrypted, ssh_host_key,
	proxy_port, listen_port, api_port, arch, singbox_version, singbox_build_tags,
	reality_dest, reality_dest_port, reality_privkey_encrypted, reality_pubkey, reality_short_id,
	handshake_max_record_size, handshake_checked_at,
	status, last_heartbeat_at, config_revision, deployed_config_sha256,
	created_at, updated_at`

func (s *Store) scanNode(scan func(dest ...any) error) (*Node, error) {
	var n Node
	var sshKeyEnc, realityKeyEnc string
	err := scan(
		&n.ID, &n.Name, &n.Host, &n.SSHPort, &n.SSHUser, &sshKeyEnc, &n.HostKey,
		&n.ProxyPort, &n.ListenPort, &n.APIPort, &n.Arch, &n.SingBoxVersion, &n.BuildTags,
		&n.RealityDest, &n.RealityDestPort, &realityKeyEnc, &n.RealityPublicKey, &n.RealityShortID,
		&n.HandshakeMaxRecordSize, &n.HandshakeCheckedAt,
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
	Name    string
	Host    string
	SSHPort int
	SSHUser string
	SSHKey  string
	// ProxyPort 是客户端连接的公网端口,必填。
	// ListenPort 是 sing-box 在主机上监听的端口,留空表示不做端口转发,与 ProxyPort 相同。
	ProxyPort  int
	ListenPort int
	APIPort    int
	// RealityDest 为空时使用默认候选目标的第一个。
	RealityDest     string
	RealityDestPort int
}

// Create 新增节点,同时生成 REALITY 密钥对与 short_id。
func (s *Store) Create(ctx context.Context, p CreateParams) (*Node, error) {
	if err := validateCreate(&p); err != nil {
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
		INSERT INTO nodes (name, host, ssh_port, ssh_user, ssh_key_encrypted, ssh_host_key,
			proxy_port, listen_port, api_port, reality_dest, reality_dest_port,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, created_at, updated_at)
		VALUES (?,?,?,?,?,'',?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.Host, p.SSHPort, p.SSHUser, sshKeyEnc,
		p.ProxyPort, p.ListenPort, p.APIPort, p.RealityDest, p.RealityDestPort,
		realityKeyEnc, keys.PublicKey, shortID,
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
	if err := validateIdentity(p.Name, p.Host); err != nil {
		return err
	}
	if err := normalizeSSH(&p.SSHPort, &p.SSHUser); err != nil {
		return err
	}
	// SSH 私钥可以留空:留空表示这个节点用面板专用密钥,
	// 由 Service.Bootstrap 在创建后把面板公钥装进节点。
	if err := normalizePorts(&p.ProxyPort, &p.ListenPort, &p.APIPort); err != nil {
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

func validateIdentity(name, host string) error {
	if name == "" {
		return errors.New("节点名称不能为空")
	}
	if len(name) > 64 {
		return errors.New("节点名称不能超过 64 个字符")
	}
	if host == "" {
		return errors.New("主机地址不能为空")
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

func (s *Store) Get(ctx context.Context, id int64) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = ? AND deleted_at IS NULL`, id)
	n, err := s.scanNode(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

func (s *Store) List(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE deleted_at IS NULL ORDER BY id`)
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
	Name       string
	Host       string
	SSHPort    int
	SSHUser    string
	SSHKey     string
	ProxyPort  int
	ListenPort int
	APIPort    int
}

// UpdateEffect 描述一次修改带来的后果,调用方据此决定后续动作。
type UpdateEffect struct {
	// SSHChanged 为真时必须丢弃连接池中该节点的长连接,否则后续操作仍走旧地址。
	SSHChanged bool `json:"ssh_changed"`
	// NeedsDeploy 为真时节点上正在运行的配置已与期望状态不一致,需要重新部署。
	// 面板不自动部署:部署会重启 sing-box 踢掉全部在线连接,
	// 何时切换由管理员配合 NAT/nginx 的改动决定。
	NeedsDeploy bool     `json:"needs_deploy"`
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
	if err := validateIdentity(p.Name, p.Host); err != nil {
		return nil, effect, err
	}
	if err := normalizeSSH(&p.SSHPort, &p.SSHUser); err != nil {
		return nil, effect, err
	}
	if err := normalizePorts(&p.ProxyPort, &p.ListenPort, &p.APIPort); err != nil {
		return nil, effect, err
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
	track("主机地址", old.Host != p.Host, old.Host, p.Host)
	track("SSH 端口", old.SSHPort != p.SSHPort, old.SSHPort, p.SSHPort)
	track("SSH 用户", old.SSHUser != p.SSHUser, old.SSHUser, p.SSHUser)
	track("公网代理端口", old.ProxyPort != p.ProxyPort, old.ProxyPort, p.ProxyPort)
	track("主机代理端口", old.ListenPort != p.ListenPort, old.ListenPort, p.ListenPort)
	track("API 端口", old.APIPort != p.APIPort, old.APIPort, p.APIPort)
	if sshKeyEnc != "" {
		effect.Changes = append(effect.Changes, "已更换 SSH 私钥")
	}

	effect.SSHChanged = old.Host != p.Host || old.SSHPort != p.SSHPort ||
		old.SSHUser != p.SSHUser || sshKeyEnc != ""
	// 只有进入节点配置的字段才需要重新部署。公网端口与节点名只影响订阅内容,
	// 主机地址只影响 SSH 与订阅,改了它们重启 sing-box 没有意义。
	effect.NeedsDeploy = old.ListenPort != p.ListenPort || old.APIPort != p.APIPort

	// SSH 私钥留空时保持原值:COALESCE 会因空串仍然是非 NULL 而失效,只能用 CASE。
	_, err = s.db.ExecContext(ctx, `
		UPDATE nodes
		   SET name = ?, host = ?, ssh_port = ?, ssh_user = ?,
		       ssh_key_encrypted = CASE WHEN ? = '' THEN ssh_key_encrypted ELSE ? END,
		       proxy_port = ?, listen_port = ?, api_port = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, p.Host, p.SSHPort, p.SSHUser,
		sshKeyEnc, sshKeyEnc,
		p.ProxyPort, p.ListenPort, p.APIPort,
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

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
