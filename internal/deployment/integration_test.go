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
			ProxyPort:         e.proxyPort,
			APIPort:           28080,
			RealityDest:       e.dest,
			RealityPort:       443,
			RealityPrivateKey: e.keys.privateKey,
			ShortID:           e.shortID,
			Users:             users,
		},
		RealityPublicKey: e.keys.publicKey,
		SSHPort:          e.sshPort,
		Revision:         revision,
	}
}

func testUsers(t *testing.T, n int) []singbox.User {
	t.Helper()
	users := make([]singbox.User, 0, n)
	for i := 1; i <= n; i++ {
		users = append(users, singbox.User{
			Code: fmt.Sprintf("user_%06d", i),
			UUID: genUUID(t),
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
	badReq.Params.RealityPrivateKey = mismatched.privateKey
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
