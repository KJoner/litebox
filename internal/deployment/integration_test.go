package deployment

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// 密钥生成在这里重新实现而不是复用 internal/node:
// node 依赖 deployment,测试若反向引入 node 会形成导入环。

type realityKeyPair struct {
	privateKey string
	publicKey  string
}

func genRealityKeyPair(t *testing.T) realityKeyPair {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return realityKeyPair{
		privateKey: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
		publicKey:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
	}
}

func genShortID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func genUUID(t *testing.T) string {
	t.Helper()
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// 针对真实节点的集成测试。默认跳过,设置以下环境变量后运行:
//
//	LITEBOX_TEST_NODE_HOST      节点地址
//	LITEBOX_TEST_NODE_PORT      SSH 端口(默认 22)
//	LITEBOX_TEST_NODE_USER      SSH 用户(默认 root)
//	LITEBOX_TEST_NODE_KEY       SSH 私钥路径
//	LITEBOX_TEST_NODE_PROXYPORT VLESS 端口(默认 24443)
//
// 前置条件:节点上已通过面板完成 install(有 sing-box 与 systemd 单元)。
// 这些测试会重启节点上的 litebox-singbox 服务,不要指向生产节点。

type integrationEnv struct {
	pool      *sshx.Pool
	deployer  *Deployer
	proxyPort int
	sshPort   int
	dest      string
	keys      realityKeyPair
	shortID   string
}

func setupIntegration(t *testing.T) *integrationEnv {
	t.Helper()

	host := os.Getenv("LITEBOX_TEST_NODE_HOST")
	if host == "" {
		t.Skip("未设置 LITEBOX_TEST_NODE_HOST,跳过真实节点集成测试")
	}
	keyPath := os.Getenv("LITEBOX_TEST_NODE_KEY")
	if keyPath == "" {
		t.Fatal("设置了 LITEBOX_TEST_NODE_HOST 就必须同时设置 LITEBOX_TEST_NODE_KEY")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读取 SSH 私钥: %v", err)
	}

	sshPort := envInt(t, "LITEBOX_TEST_NODE_PORT", 22)
	proxyPort := envInt(t, "LITEBOX_TEST_NODE_PROXYPORT", 24443)
	user := os.Getenv("LITEBOX_TEST_NODE_USER")
	if user == "" {
		user = "root"
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := sshx.NewPool(func(ctx context.Context, nodeID int64) (sshx.Target, error) {
		return sshx.Target{
			Host:          host,
			Port:          sshPort,
			User:          user,
			PrivateKeyPEM: string(keyPEM),
		}, nil
	}, logger)
	t.Cleanup(pool.CloseAll)

	return &integrationEnv{
		pool: pool,
		deployer: NewDeployer(Options{
			Pool:        pool,
			Layout:      DefaultLayout(),
			Logger:      logger,
			KeepBackups: 5,
		}),
		proxyPort: proxyPort,
		sshPort:   sshPort,
		dest:      "www.cloudflare.com",
		keys:      genRealityKeyPair(t),
		shortID:   genShortID(t),
	}
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("环境变量 %s 不是整数: %v", key, raw)
	}
	return v
}

func (e *integrationEnv) request(revision int64, users []singbox.User) Request {
	return Request{
		NodeID: 1,
		Params: singbox.NodeParams{
			APIPort: 28080,
			Inbounds: []singbox.InboundParams{{
				Tag:               singbox.LegacyVLESSInboundTag,
				ListenPort:        e.proxyPort,
				RealityDest:       e.dest,
				RealityPort:       443,
				RealityPrivateKey: e.keys.privateKey,
				ShortID:           e.shortID,
				Users:             users,
			}},
		},
		Probes: []ProbeTarget{{
			Tag:              singbox.LegacyVLESSInboundTag,
			RealityPublicKey: e.keys.publicKey,
		}},
		SSHPort:  e.sshPort,
		Revision: revision,
	}
}

// ssRequest 把同一台节点改成 Shadowsocks 2022 的部署请求。
//
// 节点二进制不变 —— Shadowsocks 在 sing-box 里是核心协议,
// 不在任何构建标签后面,现有的 assets/singbox 直接就支持。
func (e *integrationEnv) ssRequest(t *testing.T, revision int64, users []singbox.User) Request {
	t.Helper()
	req := e.request(revision, users)
	req.Params.Inbounds[0].Protocol = singbox.ProtocolShadowsocks
	req.Params.Inbounds[0].SSMethod = singbox.SSMethodAES128GCM
	key, err := singbox.GenerateSSKey()
	if err != nil {
		t.Fatal(err)
	}
	req.Params.Inbounds[0].SSPassword = key
	return req
}

// snellRequest 把同一台节点改成 Snell 的部署请求。
//
// **前置条件比另外两种多一条:节点上装的必须是预览版 sing-box。**
// 装着正式版时这次部署会在 check 那一步失败,报
// `unknown inbound type: snell` —— 那正是 node.checkChannelSupportsProtocol
// 要拦在保存入口时的东西,而这里跑的是它下面那一层。
func (e *integrationEnv) snellRequest(
	t *testing.T, revision int64, users []singbox.User, version int, obfs singbox.SnellObfsMode,
) Request {
	t.Helper()
	req := e.request(revision, users)
	psk, err := singbox.GenerateSnellKey()
	if err != nil {
		t.Fatal(err)
	}
	in := &req.Params.Inbounds[0]
	in.Protocol = singbox.ProtocolSnell
	in.SnellVersion = version
	in.SnellPSK = psk
	in.SnellObfsMode = obfs
	// REALITY 那几项留着不清:生产上它们同样留在库里(切协议不清空),
	// 而渲染必须靠协议二选一 —— 清掉之后这个测试就量不到那件事了。
	return req
}

// testUsers 生成带两套凭据的用户 —— 与生产一致:
// 一份凭据对应一种协议,渲染时按节点协议取用其中一份。
func testUsers(t *testing.T, n int) []singbox.User {
	t.Helper()
	users := make([]singbox.User, 0, n)
	for i := 1; i <= n; i++ {
		key, err := singbox.GenerateSSKey()
		if err != nil {
			t.Fatal(err)
		}
		snellKey, err := singbox.GenerateSnellKey()
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, singbox.User{
			Code:         fmt.Sprintf("user_%06d", i),
			UUID:         genUUID(t),
			SSPassword:   key,
			SnellUserKey: snellKey,
		})
	}
	return users
}

func stepByName(result Result, name string) (Step, bool) {
	for _, s := range result.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

func logSteps(t *testing.T, result Result) {
	t.Helper()
	for _, s := range result.Steps {
		t.Logf("  [%-7s] %-26s %5dms  %s", s.Status, s.Name, s.DurationMS, s.Detail)
	}
}

// 带用户的配置部署后,三步健康检查都应通过,其中拨测必须真的建立了 VLESS 连接。
func TestIntegrationDeployWithUsersPassesDialCheck(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := env.deployer.Deploy(ctx, env.request(101, testUsers(t, 2)))
	logSteps(t, result)
	if err != nil {
		t.Fatalf("部署失败: %v", err)
	}
	if result.Status != StatusSuccess {
		t.Fatalf("部署状态 = %s", result.Status)
	}

	dial, ok := stepByName(result, "健康检查:VLESS 拨测")
	if !ok {
		t.Fatal("步骤记录中没有拨测")
	}
	if dial.Status != StepSuccess {
		t.Fatalf("拨测未通过:%s", dial.Detail)
	}
	if !strings.Contains(dial.Detail, "SSH-") {
		t.Errorf("拨测详情应包含经代理读到的 SSH 横幅:%s", dial.Detail)
	}
}

// 核心回滚验收:配置能通过 sing-box check、服务能启动、端口能监听,
// 但用户实际连不上。只有第三步拨测能发现,并且必须自动回滚。
//
// 构造方式是给节点下发一份 REALITY 私钥格式合法、但与探测客户端所用公钥
// 不配套的配置。这与 Phase 0 用非法 flow 复现的是同一条失效路径:
// 前两步健康检查全绿,而所有用户握手失败。
func TestIntegrationBadConfigTriggersRollback(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	users := testUsers(t, 2)

	// 先部署一份正常配置,作为回滚的目标版本。
	good, err := env.deployer.Deploy(ctx, env.request(201, users))
	if err != nil {
		logSteps(t, good)
		t.Fatalf("基线部署失败: %v", err)
	}
	t.Logf("基线配置哈希: %s", good.ConfigSHA256)

	// 换一把不配套的 REALITY 私钥:格式合法,check 会通过,
	// 但探测客户端持有的仍是旧公钥,握手必然失败。
	mismatched := genRealityKeyPair(t)
	badReq := env.request(202, users)
	badReq.Params.Inbounds[0].RealityPrivateKey = mismatched.privateKey
	// RealityPublicKey 保持为原来的公钥,模拟"节点配置与已下发凭据不一致"。

	bad, deployErr := env.deployer.Deploy(ctx, badReq)
	logSteps(t, bad)

	if deployErr == nil {
		t.Fatal("坏配置竟然部署成功了,拨测健康检查失效")
	}
	if bad.Status != StatusRolledBack {
		t.Fatalf("部署状态 = %s,期望 ROLLED_BACK", bad.Status)
	}

	// 前两步必须是通过的 —— 这正是"只靠它们会误判"的证据。
	for _, name := range []string{"健康检查:systemd 状态", "健康检查:端口监听"} {
		step, ok := stepByName(bad, name)
		if !ok {
			t.Fatalf("缺少步骤 %s", name)
		}
		if step.Status != StepSuccess {
			t.Errorf("步骤 %s = %s,本应通过(它们发现不了这类故障)", name, step.Status)
		}
	}
	dial, ok := stepByName(bad, "健康检查:VLESS 拨测")
	if !ok || dial.Status != StepFailed {
		t.Fatal("拨测本应失败并触发回滚")
	}
	if !strings.Contains(bad.RollbackResult, "回滚成功") {
		t.Fatalf("回滚未成功:%s", bad.RollbackResult)
	}

	// 回滚后节点必须重新可用:再部署一次正常配置应当直接成功。
	verify, err := env.deployer.Deploy(ctx, env.request(203, users))
	logSteps(t, verify)
	if err != nil {
		t.Fatalf("回滚后节点未恢复正常:%v", err)
	}
	if verify.ConfigSHA256 != good.ConfigSHA256 {
		t.Errorf("同样的参数应渲染出相同配置:%s != %s", verify.ConfigSHA256, good.ConfigSHA256)
	}
}

// 部署事务必须先 check 再重启,不得反过来。
//
// 说明:无法通过 Deploy 构造出 check 失败的用例 —— 面板自己的校验严格强于
// sing-box check(见 singbox 包注释),凡是能通过 Render 的配置 sing-box 都收。
// 这本身是期望的性质,因此这里改为验证步骤顺序不变量,
// 由下面的 TestIntegrationSingBoxCheckRejectsMalformedConfig 单独验证
// 节点侧 check 确实会拒绝坏配置。
func TestIntegrationCheckPrecedesRestart(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := env.deployer.Deploy(ctx, env.request(301, testUsers(t, 1)))
	logSteps(t, result)
	if err != nil {
		t.Fatalf("部署失败: %v", err)
	}

	var checkIdx, restartIdx = -1, -1
	for i, s := range result.Steps {
		switch s.Name {
		case "sing-box check":
			checkIdx = i
		case "重启服务":
			restartIdx = i
		}
	}
	if checkIdx < 0 || restartIdx < 0 {
		t.Fatal("步骤记录中缺少 check 或重启")
	}
	if checkIdx > restartIdx {
		t.Errorf("check 出现在重启之后(位置 %d > %d),坏配置会先把服务弄挂", checkIdx, restartIdx)
	}
	// 备份也必须在替换之前,否则回滚时没有可恢复的版本。
	backupIdx, replaceIdx := -1, -1
	for i, s := range result.Steps {
		switch s.Name {
		case "备份当前配置":
			backupIdx = i
		case "原子替换配置":
			replaceIdx = i
		}
	}
	if backupIdx > replaceIdx {
		t.Errorf("备份出现在替换之后(位置 %d > %d),回滚将无版本可用", backupIdx, replaceIdx)
	}
}

// 节点侧的 sing-box check 确实会拒绝格式损坏的配置。
// 直接上传坏配置并调用 check,不经过部署事务,因此不会影响正在运行的服务。
func TestIntegrationSingBoxCheckRejectsMalformedConfig(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	layout := DefaultLayout()
	const probePath = "/opt/litebox/check-probe.json"

	// REALITY 私钥长度不足 32 字节,Phase 0 已确认 check 会报 invalid private key。
	badConfig := []byte(`{
  "log": { "level": "info" },
  "inbounds": [{
    "type": "vless", "tag": "vless-in", "listen": "::", "listen_port": 24443,
    "users": [{ "name": "user_000001", "uuid": "00000000-0000-4000-8000-000000000001", "flow": "xtls-rprx-vision" }],
    "tls": { "enabled": true, "server_name": "www.cloudflare.com",
      "reality": { "enabled": true, "handshake": { "server": "www.cloudflare.com", "server_port": 443 },
        "private_key": "dG9vLXNob3J0", "short_id": ["abcd"] } }
  }],
  "outbounds": [{ "type": "direct", "tag": "direct" }]
}`)

	err := env.pool.Do(ctx, 1, func(client *sshx.Client) error {
		if err := client.Upload(ctx, probePath, badConfig, 0o600); err != nil {
			return err
		}
		defer client.Run(ctx, sshx.NewCommand("rm", "-f", probePath))

		result, err := client.Run(ctx, sshx.NewCommand(layout.BinaryPath, "check", "-c", probePath))
		if err != nil {
			return err
		}
		if result.ExitCode == 0 {
			t.Error("sing-box check 接受了私钥长度非法的配置")
			return nil
		}
		t.Logf("check 已按预期拒绝,退出码 %d:%s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
		return nil
	})
	if err != nil {
		t.Fatalf("执行远端 check: %v", err)
	}
}

// Shadowsocks 2022 的完整部署:配置渲染 → check → 重启 → 三步健康检查,
// 其中拨测必须真的经 Shadowsocks 链路建立连接。
//
// 这是第一块功能里唯一无法在单元测试中覆盖的一环 ——
// serverPSK:userPSK 的拼法、method 与密钥长度的匹配、sing-box 对
// 多用户 EIH 的支持,都只能由节点上真实的 sing-box 来回答。
func TestIntegrationDeployShadowsocksPassesDialCheck(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := env.deployer.Deploy(ctx, env.ssRequest(t, 201, testUsers(t, 2)))
	logSteps(t, result)
	if err != nil {
		t.Fatalf("Shadowsocks 部署失败: %v", err)
	}
	if result.Status != StatusSuccess {
		t.Fatalf("部署状态 = %s", result.Status)
	}

	dial, ok := stepByName(result, "健康检查:Shadowsocks 拨测")
	if !ok {
		t.Fatal("步骤记录中没有 Shadowsocks 拨测")
	}
	if dial.Status != StepSuccess {
		t.Fatalf("拨测未通过:%s", dial.Detail)
	}
	if !strings.Contains(dial.Detail, "SSH-") {
		t.Errorf("拨测详情应包含经代理读到的 SSH 横幅:%s", dial.Detail)
	}

	// 时钟检查必须真的跑过。它是 Shadowsocks 独有的失效模式,
	// 而后面三步检查【结构性地】测不出它 —— 拨测客户端与服务端
	// 跑在同一台机器上,共用同一个时钟。
	skew, ok := stepByName(result, clockSkewStep)
	if !ok {
		t.Fatal("步骤记录中没有时钟检查")
	}
	if skew.Status == StepFailed {
		t.Fatalf("时钟检查失败:%s", skew.Detail)
	}
	t.Logf("节点时钟:%s", skew.Detail)
}

// 两种协议来回切换都要能部署成功,且切回去之后节点仍然可用。
//
// 单向测通不够:切走时留在节点上的旧配置字段(REALITY 块、
// 用户列表的形状)如果没被干净替换,表现是切回来之后 sing-box
// 起得来、端口也在听,但没有人连得上。
func TestIntegrationProtocolSwitchRoundTrip(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	users := testUsers(t, 2)

	for _, step := range []struct {
		name     string
		req      Request
		dialStep string
	}{
		{"VLESS", env.request(301, users), "健康检查:VLESS 拨测"},
		{"切到 Shadowsocks", env.ssRequest(t, 302, users), "健康检查:Shadowsocks 拨测"},
		{"切回 VLESS", env.request(303, users), "健康检查:VLESS 拨测"},
	} {
		result, err := env.deployer.Deploy(ctx, step.req)
		t.Logf("--- %s ---", step.name)
		logSteps(t, result)
		if err != nil {
			t.Fatalf("%s 部署失败: %v", step.name, err)
		}
		dial, ok := stepByName(result, step.dialStep)
		if !ok || dial.Status != StepSuccess {
			t.Fatalf("%s 的拨测没通过:%+v", step.name, dial)
		}
	}
}

// Snell 入站部署后,四步健康检查都要通过,其中拨测必须真的建立了 Snell 连接
// 并在隧道里完成一次完整的 SSH 公钥认证。
//
// **这一步不可省略。** 与 VLESS 那一条同一个理由,而 Snell 还多一处会
// 静默出事的地方:版本写错(服务端 5 要对应客户端 4)时,探测客户端会在
// decode 阶段拒掉整份配置,而报出来的是"探测客户端未能监听端口" ——
// 那句话完全看不出真正的原因。
func TestIntegrationDeploySnellPassesDialCheck(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := env.deployer.Deploy(ctx, env.snellRequest(t, 610,
		testUsers(t, 2), singbox.SnellVersion6, ""))
	logSteps(t, result)
	if err != nil {
		t.Fatalf("Snell v6 部署失败: %v(回滚:%s)", err, result.RollbackResult)
	}
	if result.Status != StatusSuccess {
		t.Fatalf("部署状态是 %s", result.Status)
	}
	// 步骤名里必须出现 Snell —— 拨测按协议分派,而分派写错的表现是
	// 用 VLESS 的参数去拨一个 Snell 入站,那会失败并回滚一个健康的节点。
	var dial Step
	for _, st := range result.Steps {
		if strings.Contains(st.Name, "拨测") {
			dial = st
		}
	}
	if !strings.Contains(dial.Name, "Snell") {
		t.Fatalf("拨测这一步的名字是 %q,没认出协议", dial.Name)
	}
	if dial.Status != StepSuccess {
		t.Fatalf("Snell 拨测未通过:%s", dial.Detail)
	}
	if !strings.Contains(dial.Detail, "拨测成功") {
		t.Errorf("拨测详情不像真的连上了:%s", dial.Detail)
	}
}

// 版本 5 + HTTP 混淆同样要能拨通。
//
// 两个版本走的是两条不同的线路协议(v5 用 obfs,v6 用流量整形),
// 而客户端版本还要经 SnellClientVersion 翻译一次 —— 只验其中一个的话,
// 另一个的翻译写错了没有人会发现,直到管理员真的建了那种入口。
func TestIntegrationDeploySnellV5WithObfs(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := env.deployer.Deploy(ctx, env.snellRequest(t, 611,
		testUsers(t, 2), singbox.SnellVersion5, singbox.SnellObfsHTTP))
	logSteps(t, result)
	if err != nil {
		t.Fatalf("Snell v5 + http 混淆部署失败: %v(回滚:%s)", err, result.RollbackResult)
	}
	if result.Status != StatusSuccess {
		t.Fatalf("部署状态是 %s", result.Status)
	}
}

// 三种协议来回切都要能落地。
//
// 与 TestIntegrationProtocolSwitchRoundTrip 同一件事,只是把 Snell 也串进去:
// 切协议时库里另外两种的参数都留着(不清空),而渲染必须靠协议二选一 ——
// 写错的表现是 sing-box 拒绝启动,部署失败并回滚。
func TestIntegrationSnellProtocolRoundTrip(t *testing.T) {
	env := setupIntegration(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	users := testUsers(t, 2)
	steps := []struct {
		name string
		req  Request
	}{
		{"VLESS", env.request(620, users)},
		{"Snell v6", env.snellRequest(t, 621, users, singbox.SnellVersion6, "")},
		{"Shadowsocks", env.ssRequest(t, 622, users)},
		{"Snell v5", env.snellRequest(t, 623, users, singbox.SnellVersion5, singbox.SnellObfsNone)},
		{"VLESS", env.request(624, users)},
	}
	for _, s := range steps {
		result, err := env.deployer.Deploy(ctx, s.req)
		if err != nil {
			logSteps(t, result)
			t.Fatalf("切到 %s 失败: %v(回滚:%s)", s.name, err, result.RollbackResult)
		}
		t.Logf("切到 %-12s 成功,配置 %s", s.name, result.ConfigSHA256[:12])
	}
}
