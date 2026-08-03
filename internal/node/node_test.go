package node

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
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
		"代理端口为零":     func(p *CreateParams) { p.ProxyPort = 0 },
		"代理端口超范围":    func(p *CreateParams) { p.ProxyPort = 99999 },
		"代理与API端口相同": func(p *CreateParams) { p.APIPort = p.ProxyPort },
		"主机端口超范围":    func(p *CreateParams) { p.ListenPort = 99999 },
		"主机与API端口相同": func(p *CreateParams) { p.APIPort, p.ListenPort = 28080, 28080 },
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
	usable := ProbeResult{HasV2RayAPI: true, InitSystem: "systemd", InitVersion: "systemd 252"}
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

// 不填主机端口意味着"没有端口转发",此时它必须与公网端口一致 ——
// 这是绝大多数节点的形态,也是本列加入之前全部存量节点的形态。
func TestListenPortDefaultsToProxyPort(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.ListenPort = 0
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.ListenPort != p.ProxyPort {
		t.Errorf("主机端口 = %d,应回落到公网端口 %d", n.ListenPort, p.ProxyPort)
	}
}

// NAT 主机与自建 nginx 转发的形态:公网 443 -> 主机 20443。
// 两个端口必须各自独立保存,合并成一个值就无法描述转发。
func TestNATPortsStoredIndependently(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.ProxyPort, p.ListenPort = 443, 20443
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.ProxyPort != 443 || n.ListenPort != 20443 {
		t.Fatalf("端口 = 公网 %d / 主机 %d,应为 443 / 20443", n.ProxyPort, n.ListenPort)
	}

	// 节点配置必须监听主机端口。渲染成公网端口会让 sing-box 监听在
	// 转发链路另一端的号码上,NAT 转进来的流量无人接收。
	cfg, err := singbox.Render(singbox.NodeParams{
		ListenPort:        n.ListenPort,
		APIPort:           n.APIPort,
		RealityDest:       n.RealityDest,
		RealityPort:       n.RealityDestPort,
		RealityPrivateKey: n.RealityPrivateKey,
		ShortID:           n.RealityShortID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Inbounds[0].ListenPort != 20443 {
		t.Errorf("inbound.listen_port = %d,应为主机端口 20443", cfg.Inbounds[0].ListenPort)
	}
}

func TestUpdateNodeReportsEffect(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	// 只改公网端口:订阅内容变了,但节点上跑的配置一个字节都没变。
	updated, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: 443, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProxyPort != 443 || updated.ListenPort != n.ListenPort {
		t.Fatalf("端口 = 公网 %d / 主机 %d", updated.ProxyPort, updated.ListenPort)
	}
	if effect.NeedsDeploy {
		t.Error("只改公网端口不该要求重新部署")
	}
	if effect.SSHChanged {
		t.Error("未改连接参数却报告 SSH 变更")
	}

	// 改主机端口:进了节点配置,必须重新部署才生效。
	_, effect, err = store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: 443, ListenPort: 20443, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !effect.NeedsDeploy {
		t.Error("改主机端口后应要求重新部署")
	}

	// 改主机地址:连接池里的长连接指向旧地址,必须失效重连。
	_, effect, err = store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: "192.0.2.99", SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: 443, ListenPort: 20443, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !effect.SSHChanged {
		t.Error("改主机地址后应报告 SSH 变更")
	}
	if effect.NeedsDeploy {
		t.Error("改主机地址不影响节点配置,不该要求重新部署")
	}
}

// 私钥不回显给前端,前端也就无法把原值提交回来:留空必须保持原私钥,
// 否则每次改个端口都会把节点的 SSH 凭据清掉。
func TestUpdateNodeKeepsSSHKeyWhenBlank(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	updated, _, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		SSHKey: "", ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SSHKey != testSSHKey {
		t.Fatal("留空私钥时原私钥被清掉了")
	}

	const newKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nrotated\n-----END OPENSSH PRIVATE KEY-----"
	updated, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		SSHKey: newKey, ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SSHKey != newKey {
		t.Error("换私钥未生效")
	}
	if !effect.SSHChanged {
		t.Error("换私钥后应报告 SSH 变更")
	}
}

func TestUpdateNodeRejectsBadParams(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(t.Context(), func() CreateParams {
		p := defaultCreateParams()
		p.Name = "node-sg"
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}

	base := UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	}
	cases := map[string]func(*UpdateParams){
		"名称为空":       func(p *UpdateParams) { p.Name = "" },
		"主机为空":       func(p *UpdateParams) { p.Host = "" },
		"公网端口为零":     func(p *UpdateParams) { p.ProxyPort = 0 },
		"主机与API端口相同": func(p *UpdateParams) { p.ListenPort = p.APIPort },
		"名称与其他节点重复":  func(p *UpdateParams) { p.Name = other.Name },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, _, err := store.Update(t.Context(), n.ID, p); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}

	if _, _, err := store.Update(t.Context(), 9999, base); !errors.Is(err, ErrNotFound) {
		t.Errorf("修改不存在的节点应返回 ErrNotFound,实际 %v", err)
	}
}

// 无任何改动时 Changes 必须是空数组而不是 nil:nil 会序列化成 JSON 的 null,
// 前端拿到 null 再取 .length 会直接抛异常。
func TestUpdateNodeReturnsEmptyChangesNotNil(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	_, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if effect.Changes == nil {
		t.Fatal("Changes 是 nil,会序列化成 null")
	}
	if len(effect.Changes) != 0 {
		t.Errorf("没有改动却报告了变更:%v", effect.Changes)
	}
	body, err := json.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"changes":[]`) {
		t.Errorf("序列化结果 = %s", body)
	}
}

// 私钥留空是新节点的常态:它表示"用面板专用密钥",
// 由 Bootstrap 把面板公钥装进节点。留空必须真的存空串 ——
// 存成"加密后的空串"就不为空了,读取侧会把它当成一把解不开的私钥。
func TestCreateNodeAllowsEmptySSHKey(t *testing.T) {
	store, db := newTestStore(t)
	p := defaultCreateParams()
	p.SSHKey = ""

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatalf("私钥留空应当允许: %v", err)
	}
	if n.SSHKey != "" {
		t.Errorf("读回的私钥应为空,得到 %q", n.SSHKey)
	}

	var stored string
	if err := db.QueryRow(
		`SELECT ssh_key_encrypted FROM nodes WHERE id = ?`, n.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Errorf("空私钥应当存空串而不是密文,得到 %q", stored)
	}
}

func TestParseMetrics(t *testing.T) {
	// 用真机量级的计数器:一台跑了几个月的机器,CPU jiffies 与网卡累计字节数
	// 都远超 6 位有效数字。这正是本地全新容器上测不出来的那一档。
	out := `cpu_a 11031991837 9814835801
net_a 9814712345 4501234567
cpu_b 11031992037 9814835951
net_b 9814714393 4501235591
mem 2048000 1024000
load1 0.42
uptime 86400
disk 20971520 5242880
`
	m, err := parseMetrics(out)
	if err != nil {
		t.Fatal(err)
	}
	// 200 jiffies 里 150 空闲 -> 忙 25%。
	if m.CPUPercent != 25 {
		t.Errorf("CPU = %v,应为 25", m.CPUPercent)
	}
	// 用 MemAvailable 而不是 MemTotal-MemFree:后者把页缓存算成已用,
	// 小内存机器上会常年显示接近满载。
	if m.MemUsedKB != 1024000 {
		t.Errorf("已用内存 = %d", m.MemUsedKB)
	}
	if m.MemPercent() != 50 {
		t.Errorf("内存占比 = %v", m.MemPercent())
	}
	if m.NetRxBps != 2048 || m.NetTxBps != 1024 {
		t.Errorf("网速 = %d/%d", m.NetRxBps, m.NetTxBps)
	}
	if m.Load1 != 0.42 || m.UptimeSeconds != 86400 {
		t.Errorf("负载/uptime = %v/%d", m.Load1, m.UptimeSeconds)
	}
	if m.DiskTotalKB != 20971520 || m.DiskUsedKB != 5242880 {
		t.Errorf("磁盘 = %d/%d", m.DiskTotalKB, m.DiskUsedKB)
	}
}

// 大计数器必须逐字节精确,不能被浮点或格式化削掉精度。
// 网卡累计值差 1KB 在这里就是 1KB/s 的误差,趋势图上一眼可见。
func TestParseMetricsKeepsLargeCountersExact(t *testing.T) {
	m, err := parseMetrics(`cpu_a 11031991837 9814835801
cpu_b 11031991937 9814835901
net_a 9007199254730000 1
net_b 9007199254740000 2
mem 1024 512
`)
	if err != nil {
		t.Fatal(err)
	}
	if m.NetRxBps != 10000 {
		t.Errorf("大计数器差值 = %d,应为 10000", m.NetRxBps)
	}
	// 100 jiffies 全部空闲 -> 0%。
	if m.CPUPercent != 0 {
		t.Errorf("CPU = %v", m.CPUPercent)
	}
}

// 计数器回绕、网卡重置、采样间隔为零时不能产出荒谬的数值:
// 一个假的 GB/s 尖峰会把整张趋势图压扁,真实波动全看不见了。
func TestParseMetricsClampsAbnormalDeltas(t *testing.T) {
	m, err := parseMetrics(`cpu_a 100 50
cpu_b 100 50
net_a 999999 5
net_b 1 1
mem 1024 512
`)
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUPercent != 0 {
		t.Errorf("采样间隔为零时 CPU 应为 0,得到 %v", m.CPUPercent)
	}
	if m.NetRxBps != 0 || m.NetTxBps != 0 {
		t.Errorf("负增量应归零,得到 %d/%d", m.NetRxBps, m.NetTxBps)
	}
	// 内存总量为零时不能除零。
	if (Metrics{}).MemPercent() != 0 {
		t.Error("空采样的内存占比应为 0")
	}
}

// idle 增量大于 total 增量(不同 CPU 核数下的舍入)时不能算出负数或超过 100。
func TestParseMetricsClampsCPUToRange(t *testing.T) {
	m, err := parseMetrics(`cpu_a 100 10
cpu_b 200 200
mem 1024 512
`)
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUPercent < 0 || m.CPUPercent > 100 {
		t.Errorf("CPU = %v,超出 0~100", m.CPUPercent)
	}
}

func TestParseMetricsRejectsGarbage(t *testing.T) {
	// 远端 shell 报错(缺 awk、脚本被截断)时输出不含 mem 行,
	// 必须报错而不是静默返回一条全零采样 —— 后者会在图上画出一条假的"零负载"。
	for _, bad := range []string{
		"sh: awk: not found\n",
		"",
		"cpu_a 1 2\nnet_a 3 4\n",
	} {
		if _, err := parseMetrics(bad); err == nil {
			t.Errorf("输出 %q 应当被拒绝", bad)
		}
	}
}

// 同一把公钥换个注释仍然是同一把,不能重复追加 ——
// 每次引导都追加一行会让 authorized_keys 无限膨胀。
func TestContainsAuthorizedKeyIgnoresComment(t *testing.T) {
	const body = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"
	existing := []byte("ssh-rsa AAAAB3other other@host\n" + body + " some-other-comment\n")

	if !containsAuthorizedKey(existing, body+" litebox-panel") {
		t.Error("同一把密钥换注释后应被识别为已存在")
	}
	if containsAuthorizedKey(existing, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIdifferent litebox-panel") {
		t.Error("不同的密钥被误判为已存在")
	}
	if containsAuthorizedKey(nil, body+" litebox-panel") {
		t.Error("空文件里不应找到任何密钥")
	}
	if containsAuthorizedKey(existing, "格式不对") {
		t.Error("非法公钥不应匹配到任何行")
	}
}

// x/crypto/ssh 的认证失败原文是 "attempted methods [none], no supported methods remain",
// 字面上看像是客户端没带凭据,实际是服务端没开放我们能提供的认证方式。
// 不翻译的话,一台 PasswordAuthentication=no 的机器会让人一直以为是密码打错了。
func TestExplainAuthFailure(t *testing.T) {
	authErr := errors.New(
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none], no supported methods remain")

	pwd := explainAuthFailure(authErr, "password")
	if !strings.Contains(pwd, "PasswordAuthentication") {
		t.Errorf("口令方式的提示没点出 sshd 配置:%q", pwd)
	}
	if !strings.Contains(pwd, "authorized_keys") {
		t.Errorf("口令方式的提示没给出可行的下一步:%q", pwd)
	}

	key := explainAuthFailure(authErr, "local-key")
	if key == "" || key == pwd {
		t.Errorf("私钥方式应当有自己的提示:%q", key)
	}

	// 与认证无关的错误(网络不通、超时)不该被安上"认证方式不对"的解释。
	if got := explainAuthFailure(errors.New("dial tcp 192.0.2.1:22: i/o timeout"), "password"); got != "" {
		t.Errorf("非认证错误不应附加提示:%q", got)
	}
	if got := explainAuthFailure(nil, "password"); got != "" {
		t.Errorf("nil 错误不应附加提示:%q", got)
	}
}
