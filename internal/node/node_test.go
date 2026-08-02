package node

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/singbox"
)

const testSSHKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake-key-for-tests\n-----END OPENSSH PRIVATE KEY-----"

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "node.db"), 5*time.Second)
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

func defaultCreateParams() CreateParams {
	return CreateParams{
		Name:      "node-la",
		Host:      "192.0.2.10",
		SSHPort:   22,
		SSHUser:   "root",
		SSHKey:    testSSHKey,
		ProxyPort: 24443,
	}
}

func TestCreateNodeGeneratesRealityMaterial(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	if err := singbox.ValidateRealityPrivateKey(n.RealityPrivateKey); err != nil {
		t.Errorf("生成的 REALITY 私钥格式非法: %v", err)
	}
	if err := singbox.ValidateRealityPublicKey(n.RealityPublicKey); err != nil {
		t.Errorf("生成的 REALITY 公钥格式非法: %v", err)
	}
	if err := singbox.ValidateShortID(n.RealityShortID); err != nil {
		t.Errorf("生成的 short_id 非法: %v", err)
	}
	// 公钥必须与私钥配套,否则客户端永远握手不上。
	derived, err := DerivePublicKey(n.RealityPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if derived != n.RealityPublicKey {
		t.Errorf("公钥与私钥不配套:%s != %s", derived, n.RealityPublicKey)
	}
	if n.Status != StatusPending {
		t.Errorf("新节点状态 = %s", n.Status)
	}
	if n.APIPort != 28080 {
		t.Errorf("API 端口默认值 = %d", n.APIPort)
	}
	if n.RealityDest != DefaultDestCandidates[0] {
		t.Errorf("握手目标默认值 = %s", n.RealityDest)
	}
}

// SSH 私钥与 REALITY 私钥必须以密文入库。
func TestSensitiveFieldsAreEncryptedAtRest(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	var sshEnc, realityEnc string
	err = db.QueryRow(`SELECT ssh_key_encrypted, reality_privkey_encrypted FROM nodes WHERE id = ?`, n.ID).
		Scan(&sshEnc, &realityEnc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sshEnc, "OPENSSH PRIVATE KEY") {
		t.Error("SSH 私钥以明文存入了数据库")
	}
	if realityEnc == n.RealityPrivateKey {
		t.Error("REALITY 私钥以明文存入了数据库")
	}
	// 密文必须能还原回明文,否则节点配置无从生成。
	if _, err := base64.StdEncoding.DecodeString(sshEnc); err != nil {
		t.Errorf("密文不是合法 base64: %v", err)
	}
	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SSHKey != testSSHKey {
		t.Error("读取时未能还原 SSH 私钥")
	}
}

func TestCreateNodeRejectsInvalidParams(t *testing.T) {
	store, _ := newTestStore(t)
	cases := map[string]func(*CreateParams){
		"名称为空":       func(p *CreateParams) { p.Name = "" },
		"主机为空":       func(p *CreateParams) { p.Host = "" },
		"私钥为空":       func(p *CreateParams) { p.SSHKey = "" },
		"代理端口为零":     func(p *CreateParams) { p.ProxyPort = 0 },
		"代理端口超范围":    func(p *CreateParams) { p.ProxyPort = 99999 },
		"代理与API端口相同": func(p *CreateParams) { p.APIPort = p.ProxyPort },
		"握手目标是IP":    func(p *CreateParams) { p.RealityDest = "8.8.8.8" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := defaultCreateParams()
			mutate(&p)
			if _, err := store.Create(t.Context(), p); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}
}

func TestCreateNodeRejectsDuplicateName(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), defaultCreateParams()); err != nil {
		t.Fatal(err)
	}
	p := defaultCreateParams()
	p.Host = "192.0.2.11"
	if _, err := store.Create(t.Context(), p); !errors.Is(err, ErrNameConflict) {
		t.Errorf("重名节点应当被拒绝,得到 %v", err)
	}
}

// 主机密钥固定后不允许被静默覆盖 —— 那正是中间人攻击要做的事。
func TestPinHostKeyIsStickyAndDetectsChange(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.PinHostKey(t.Context(), n.ID, "key-aaa"); err != nil {
		t.Fatalf("首次固定失败: %v", err)
	}
	// 重复固定相同密钥是幂等的。
	if err := store.PinHostKey(t.Context(), n.ID, "key-aaa"); err != nil {
		t.Errorf("固定相同密钥应当成功: %v", err)
	}
	if err := store.PinHostKey(t.Context(), n.ID, "key-bbb"); !errors.Is(err, ErrHostKeyPinned) {
		t.Errorf("固定不同密钥应当被拒绝,得到 %v", err)
	}

	// 节点重装后由管理员显式重置。
	if err := store.ResetHostKey(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.PinHostKey(t.Context(), n.ID, "key-bbb"); err != nil {
		t.Errorf("重置后应当可以重新固定: %v", err)
	}
}

func TestNextRevisionIncrementsMonotonically(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	var prev int64
	for i := 0; i < 5; i++ {
		rev, err := store.NextRevision(t.Context(), n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rev <= prev {
			t.Fatalf("revision 未递增:%d 之后是 %d", prev, rev)
		}
		prev = rev
	}
}

func TestDeleteIsSoftAndHidesNode(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), n.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后 Get 应返回 ErrNotFound,得到 %v", err)
	}
	// 软删除:行仍在,只是被过滤掉了。
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, n.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("软删除不应真的删除数据行")
	}
	if err := store.Delete(t.Context(), n.ID); !errors.Is(err, ErrNotFound) {
		t.Error("重复删除应返回 ErrNotFound")
	}
}

// 删除节点后应当能用回同一个名字。
// name 列有 UNIQUE 约束且不区分是否已删除,不处理的话名字会被永久占住。
func TestDeleteFreesNodeName(t *testing.T) {
	store, _ := newTestStore(t)

	first, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}

	second, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatalf("删除后应当能复用节点名称: %v", err)
	}
	if second.Name != defaultCreateParams().Name {
		t.Errorf("新节点名称 = %q", second.Name)
	}
}

func TestSetEnabledAndDeployStatus(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetEnabled(t.Context(), n.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(t.Context(), n.ID)
	if got.Status != StatusDisabled {
		t.Errorf("禁用后状态 = %s", got.Status)
	}

	// 已禁用的节点不应被部署流程改回在线。
	if err := store.MarkDeployed(t.Context(), n.ID, "abc123"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(t.Context(), n.ID)
	if got.Status != StatusDisabled {
		t.Errorf("禁用状态被部署流程覆盖为 %s", got.Status)
	}
}

func TestGenerateUUIDMatchesValidator(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := GenerateUUID()
		if err != nil {
			t.Fatal(err)
		}
		// 生成器与校验器必须完全对齐,否则自己生成的 UUID 会被自己拒绝。
		if err := singbox.ValidateUUID(id); err != nil {
			t.Fatalf("生成的 UUID %q 未通过校验: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("生成了重复的 UUID: %s", id)
		}
		seen[id] = true
	}
}

func TestGenerateShortIDMatchesValidator(t *testing.T) {
	for i := 0; i < 20; i++ {
		id, err := GenerateShortID(8)
		if err != nil {
			t.Fatal(err)
		}
		if err := singbox.ValidateShortID(id); err != nil {
			t.Errorf("生成的 short_id %q 未通过校验: %v", id, err)
		}
	}
}

func TestParseVersionOutput(t *testing.T) {
	const out = `sing-box version v1.13.15-litebox

Environment: go1.26.3 linux/amd64
Tags: with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0
Revision: 3708fa18766cda1f11b77f6ed9c7bd61688f17df
CGO: disabled
`
	version, tags := parseVersionOutput(out)
	if version != "v1.13.15-litebox" {
		t.Errorf("版本 = %q", version)
	}
	if len(tags) != 4 {
		t.Fatalf("标签数量 = %d: %v", len(tags), tags)
	}
	var hasAPI bool
	for _, tag := range tags {
		if tag == RequiredBuildTag {
			hasAPI = true
		}
	}
	if !hasAPI {
		t.Error("未解析出 with_v2ray_api")
	}
}

// 缺少 with_v2ray_api 的节点必须被判为不可用 —— 否则流量统计会静默失效。
func TestProbeResultRequiresV2RayAPI(t *testing.T) {
	usable := ProbeResult{HasV2RayAPI: true, SystemdVersion: "systemd 252"}
	if !usable.Usable() {
		t.Error("满足条件的探测结果应当可用")
	}

	missing := ProbeResult{
		HasV2RayAPI: false,
		Problems:    []string{"sing-box 构建标签中缺少 with_v2ray_api,流量统计无法工作"},
	}
	if missing.Usable() {
		t.Error("缺少 with_v2ray_api 时应判为不可用")
	}
}

func TestNormalizeArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
	}
	for in, want := range cases {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q,期望 %q", in, got, want)
		}
	}
}

func TestSplitRecordSizes(t *testing.T) {
	// 构造两条 TLS 记录:长度 5+3 与 5+2。
	data := []byte{
		0x16, 0x03, 0x03, 0x00, 0x03, 'a', 'b', 'c',
		0x17, 0x03, 0x03, 0x00, 0x02, 'd', 'e',
	}
	sizes := splitRecordSizes(data)
	if len(sizes) != 2 || sizes[0] != 8 || sizes[1] != 7 {
		t.Errorf("记录长度解析错误:%v", sizes)
	}

	// 截断的记录不应被计入,否则会低估最大记录长度。
	truncated := []byte{0x16, 0x03, 0x03, 0x20, 0x00, 'a'}
	if got := splitRecordSizes(truncated); len(got) != 0 {
		t.Errorf("截断记录不应被计入:%v", got)
	}
}

func TestBinaryProviderRejectsUnknownArch(t *testing.T) {
	p := NewDirBinaryProvider(t.TempDir())
	if _, err := p.Load("mips64"); err == nil {
		t.Error("不支持的架构应当报错")
	}
	// 文件缺失时的报错要能指引用户去构建。
	_, err := p.Load("amd64")
	if err == nil || !strings.Contains(err.Error(), "build-singbox.sh") {
		t.Errorf("缺少二进制时的错误信息应指引构建方式:%v", err)
	}
}
