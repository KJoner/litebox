package settings

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// PanelKey 是面板专用的节点访问密钥。
//
// 为什么要一把专用密钥:面板需要节点的 root 权限(装 systemd 单元、重启服务),
// 这个权限收不掉,能收的是爆炸半径 —— 用一把只给面板用的密钥,
// 轮换或吊销时不必动管理员自己的日常密钥。
//
// 私钥不设 passphrase:面板要无人值守地连节点,带口令的密钥用不了。
// 安全性靠"只存在数据库里且被主密钥加密"来保证,而不是靠口令。
type PanelKey struct {
	PrivateKeyPEM string
	// PublicKey 是 authorized_keys 一行的内容,形如 "ssh-ed25519 AAAA... litebox-panel"。
	PublicKey string
}

// KeyManager 负责面板密钥的懒生成与缓存。
type KeyManager struct {
	store *Store

	mu     sync.Mutex
	cached *PanelKey
}

func NewKeyManager(store *Store) *KeyManager {
	return &KeyManager{store: store}
}

// Ensure 返回面板密钥,不存在时生成一把并落库。
//
// 生成是幂等的:并发调用被互斥锁串行化,已存在则直接读出,
// 绝不会因为两个请求同时进来而把已经装到节点上的公钥换掉。
func (m *KeyManager) Ensure(ctx context.Context) (PanelKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached != nil {
		return *m.cached, nil
	}

	priv, err := m.store.GetSecret(ctx, KeyPanelSSHPrivateKey)
	if err != nil {
		return PanelKey{}, err
	}
	pub, err := m.store.Get(ctx, KeyPanelSSHPublicKey)
	if err != nil {
		return PanelKey{}, err
	}
	if priv != "" && pub != "" {
		key := PanelKey{PrivateKeyPEM: priv, PublicKey: pub}
		m.cached = &key
		return key, nil
	}

	key, err := GenerateKeyPair()
	if err != nil {
		return PanelKey{}, err
	}
	if err := m.store.SetSecret(ctx, KeyPanelSSHPrivateKey, key.PrivateKeyPEM); err != nil {
		return PanelKey{}, err
	}
	if err := m.store.Set(ctx, KeyPanelSSHPublicKey, key.PublicKey); err != nil {
		return PanelKey{}, err
	}
	m.cached = &key
	return key, nil
}

// Rotate 生成新的面板密钥并覆盖旧的。
//
// 轮换后所有节点上的旧公钥都会失效,必须逐个节点重新装公钥,
// 否则面板将连不上任何节点 —— 调用方必须把这一点告诉管理员。
func (m *KeyManager) Rotate(ctx context.Context) (PanelKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, err := GenerateKeyPair()
	if err != nil {
		return PanelKey{}, err
	}
	if err := m.store.SetSecret(ctx, KeyPanelSSHPrivateKey, key.PrivateKeyPEM); err != nil {
		return PanelKey{}, err
	}
	if err := m.store.Set(ctx, KeyPanelSSHPublicKey, key.PublicKey); err != nil {
		return PanelKey{}, err
	}
	m.cached = &key
	return key, nil
}

// GenerateKeyPair 生成一对 ed25519 SSH 密钥。
func GenerateKeyPair() (PanelKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PanelKey{}, fmt.Errorf("生成 ed25519 密钥: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "litebox-panel")
	if err != nil {
		return PanelKey{}, fmt.Errorf("编码 OpenSSH 私钥: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return PanelKey{}, fmt.Errorf("编码 SSH 公钥: %w", err)
	}

	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " litebox-panel"
	return PanelKey{
		PrivateKeyPEM: string(pem.EncodeToMemory(block)),
		PublicKey:     authorized,
	}, nil
}
