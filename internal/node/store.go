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
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	SSHPort   int    `json:"ssh_port"`
	SSHUser   string `json:"ssh_user"`
	SSHKey    string `json:"-"` // PEM 私钥,永不出现在 API 响应中
	HostKey   string `json:"-"`
	ProxyPort int    `json:"proxy_port"`
	APIPort   int    `json:"api_port"`

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
	proxy_port, api_port, arch, singbox_version, singbox_build_tags,
	reality_dest, reality_dest_port, reality_privkey_encrypted, reality_pubkey, reality_short_id,
	handshake_max_record_size, handshake_checked_at,
	status, last_heartbeat_at, config_revision, deployed_config_sha256,
	created_at, updated_at`

func (s *Store) scanNode(scan func(dest ...any) error) (*Node, error) {
	var n Node
	var sshKeyEnc, realityKeyEnc string
	err := scan(
		&n.ID, &n.Name, &n.Host, &n.SSHPort, &n.SSHUser, &sshKeyEnc, &n.HostKey,
		&n.ProxyPort, &n.APIPort, &n.Arch, &n.SingBoxVersion, &n.BuildTags,
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
	Name      string
	Host      string
	SSHPort   int
	SSHUser   string
	SSHKey    string
	ProxyPort int
	APIPort   int
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

	sshKeyEnc, err := s.cipher.Encrypt(p.SSHKey)
	if err != nil {
		return nil, fmt.Errorf("加密 SSH 私钥: %w", err)
	}
	realityKeyEnc, err := s.cipher.Encrypt(keys.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("加密 REALITY 私钥: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (name, host, ssh_port, ssh_user, ssh_key_encrypted, ssh_host_key,
			proxy_port, api_port, reality_dest, reality_dest_port,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, created_at, updated_at)
		VALUES (?,?,?,?,?,'',?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.Host, p.SSHPort, p.SSHUser, sshKeyEnc,
		p.ProxyPort, p.APIPort, p.RealityDest, p.RealityDestPort,
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
	if p.Name == "" {
		return errors.New("节点名称不能为空")
	}
	if len(p.Name) > 64 {
		return errors.New("节点名称不能超过 64 个字符")
	}
	if p.Host == "" {
		return errors.New("主机地址不能为空")
	}
	if p.SSHPort == 0 {
		p.SSHPort = 22
	}
	if err := singbox.ValidatePort(p.SSHPort, "SSH"); err != nil {
		return err
	}
	if p.SSHUser == "" {
		p.SSHUser = "root"
	}
	if p.SSHKey == "" {
		return errors.New("SSH 私钥不能为空")
	}
	if err := singbox.ValidatePort(p.ProxyPort, "代理监听"); err != nil {
		return err
	}
	if p.APIPort == 0 {
		p.APIPort = 28080
	}
	if err := singbox.ValidatePort(p.APIPort, "V2Ray API"); err != nil {
		return err
	}
	if p.ProxyPort == p.APIPort {
		return errors.New("代理端口与 API 端口不能相同")
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
