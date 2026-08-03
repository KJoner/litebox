package settings

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"golang.org/x/crypto/ssh"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "settings.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db, cipher), db
}

// 生成的私钥必须能被标准 SSH 库解析,公钥必须是 authorized_keys 的合法一行。
// 自制格式在这里出错的话,要到连节点时才会暴露。
func TestGenerateKeyPairProducesUsableOpenSSHMaterial(t *testing.T) {
	key, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.ParsePrivateKey([]byte(key.PrivateKeyPEM))
	if err != nil {
		t.Fatalf("私钥无法解析: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("密钥类型 = %s", signer.PublicKey().Type())
	}
	if !strings.HasPrefix(key.PrivateKeyPEM, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("私钥不是 OpenSSH 格式:%.40s", key.PrivateKeyPEM)
	}

	parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
	if err != nil {
		t.Fatalf("公钥不是合法的 authorized_keys 行: %v", err)
	}
	if comment != "litebox-panel" {
		t.Errorf("公钥注释 = %q", comment)
	}
	// 公私钥必须配套,否则装上去也连不上。
	if string(parsed.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("公钥与私钥不配套")
	}
	if strings.Contains(key.PublicKey, "\n") {
		t.Error("公钥含换行,追加进 authorized_keys 会破坏文件")
	}
}

// 面板密钥一旦生成就不能再变:它已经装到节点的 authorized_keys 里了,
// 重新生成等于把面板自己关在所有节点门外。
func TestEnsurePanelKeyIsStable(t *testing.T) {
	store, _ := newTestStore(t)
	mgr := NewKeyManager(store)

	first, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKey != second.PublicKey {
		t.Fatal("重复调用 Ensure 换掉了密钥")
	}

	// 换一个未预热缓存的实例,必须从数据库读回同一把。
	reloaded, err := NewKeyManager(store).Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PrivateKeyPEM != first.PrivateKeyPEM {
		t.Error("重启后读回的私钥与原来不同")
	}
}

func TestPanelPrivateKeyIsEncryptedAtRest(t *testing.T) {
	store, db := newTestStore(t)
	key, err := NewKeyManager(store).Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := db.QueryRow(
		`SELECT value FROM system_settings WHERE key = ?`, KeyPanelSSHPrivateKey).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "OPENSSH PRIVATE KEY") {
		t.Error("面板私钥以明文存进了数据库")
	}
	if stored == key.PrivateKeyPEM {
		t.Error("面板私钥未加密")
	}
}

func TestRotateReplacesKey(t *testing.T) {
	store, _ := newTestStore(t)
	mgr := NewKeyManager(store)
	old, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := mgr.Rotate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rotated.PublicKey == old.PublicKey {
		t.Fatal("轮换后公钥没变")
	}
	current, err := mgr.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if current.PublicKey != rotated.PublicKey {
		t.Error("轮换后再取密钥拿到的不是新的")
	}
}

func TestBaseURLPrefersDatabaseValue(t *testing.T) {
	store, _ := newTestStore(t)

	if got := store.BaseURL(t.Context(), "http://127.0.0.1:8080/"); got != "http://127.0.0.1:8080" {
		t.Errorf("未设置时应回落到配置值并去掉结尾斜杠,得到 %q", got)
	}
	if err := store.Set(t.Context(), KeySubscriptionBaseURL, "https://panel.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := store.BaseURL(t.Context(), "http://127.0.0.1:8080"); got != "https://panel.example.com" {
		t.Errorf("设置后应以数据库为准,得到 %q", got)
	}
}

func TestValidateBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://panel.example.com":  "https://panel.example.com",
		"https://panel.example.com/": "https://panel.example.com",
		"http://192.0.2.10:8080":     "http://192.0.2.10:8080",
	}
	for in, want := range ok {
		got, err := ValidateBaseURL(in)
		if err != nil {
			t.Errorf("%q 应当通过: %v", in, err)
		}
		if got != want {
			t.Errorf("%q 归一化为 %q,期望 %q", in, got, want)
		}
	}

	// 少了 scheme 是最容易犯也最难发现的错:客户端拿到订阅地址解析不了,
	// 而面板这边看起来一切正常。
	for _, bad := range []string{"", "   ", "panel.example.com", "ftp://panel.example.com",
		"https://", "https://panel.example.com?x=1", "https://panel.example.com#a"} {
		if _, err := ValidateBaseURL(bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}
