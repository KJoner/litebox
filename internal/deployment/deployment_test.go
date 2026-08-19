package deployment

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

func TestLayoutPaths(t *testing.T) {
	l := DefaultLayout()

	// 临时文件必须与正式配置同目录,否则 mv 会跨文件系统而失去原子性。
	if filepath.ToSlash(filepath.Dir(l.tempConfigPath())) != filepath.ToSlash(filepath.Dir(l.ConfigPath)) {
		t.Errorf("临时配置 %s 与正式配置 %s 不同目录", l.tempConfigPath(), l.ConfigPath)
	}
	if !strings.HasPrefix(l.backupPath(7), l.BackupDir) {
		t.Errorf("备份路径不在备份目录下:%s", l.backupPath(7))
	}
	if l.backupPath(7) == l.backupPath(8) {
		t.Error("不同 revision 应生成不同备份路径")
	}
	// 服务名带前缀,避免与机器上已有的 sing-box 服务冲突。
	if !strings.HasPrefix(l.ServiceName, "litebox-") {
		t.Errorf("服务名应带 litebox- 前缀:%s", l.ServiceName)
	}
}

func TestStepRecorderCapturesOutcomes(t *testing.T) {
	rec := &stepRecorder{}

	if err := rec.run("成功步骤", func() (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("校验未通过")
	if err := rec.run("失败步骤", func() (string, error) { return "", wantErr }); !errors.Is(err, wantErr) {
		t.Errorf("失败步骤应原样返回错误,得到 %v", err)
	}
	rec.skip("跳过步骤", "没有用户可拨测")

	if len(rec.steps) != 3 {
		t.Fatalf("步骤数 = %d", len(rec.steps))
	}
	if rec.steps[0].Status != StepSuccess || rec.steps[0].Detail != "ok" {
		t.Errorf("成功步骤记录不符:%+v", rec.steps[0])
	}
	if rec.steps[1].Status != StepFailed || !strings.Contains(rec.steps[1].Detail, "校验未通过") {
		t.Errorf("失败步骤应记录错误详情:%+v", rec.steps[1])
	}
	if rec.steps[2].Status != StepSkipped {
		t.Errorf("跳过步骤状态 = %s", rec.steps[2].Status)
	}
}

func TestStoreSaveAndList(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "deploy.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO nodes (id, name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES (1,'n1','127.0.0.1',24443,'www.apple.com','e','p','abcd','t','t')`); err != nil {
		t.Fatal(err)
	}

	store := NewStore(db)
	result := Result{
		NodeID:       1,
		Revision:     3,
		ConfigSHA256: "deadbeef",
		Status:       StatusRolledBack,
		Steps: []Step{
			{Name: "sing-box check", Status: StepSuccess, DurationMS: 120},
			{Name: "健康检查:VLESS 拨测", Status: StepFailed, Detail: "拨测失败"},
		},
		ErrorMessage:   "拨测失败",
		RollbackResult: "回滚成功,节点已恢复服务",
		StartedAt:      time.Now().UTC(),
		FinishedAt:     time.Now().UTC(),
	}
	if _, err := store.Save(t.Context(), result); err != nil {
		t.Fatalf("保存部署记录: %v", err)
	}

	records, err := store.ListByNode(t.Context(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("记录数 = %d", len(records))
	}
	got := records[0]
	if got.Status != string(StatusRolledBack) {
		t.Errorf("状态 = %s", got.Status)
	}
	// 步骤明细是排查失败部署最有用的信息,必须能完整取回。
	if len(got.Steps) != 2 {
		t.Fatalf("步骤数 = %d", len(got.Steps))
	}
	if got.Steps[1].Name != "健康检查:VLESS 拨测" || got.Steps[1].Status != StepFailed {
		t.Errorf("步骤明细不符:%+v", got.Steps[1])
	}
	if got.RollbackResult == "" {
		t.Error("回滚结果丢失")
	}
}

func TestStoreListRecentReturnsEmptySlice(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "deploy2.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	// 没有记录时返回空切片而非 nil,前端拿到的才是 [] 而不是 null。
	records, err := NewStore(db).ListRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if records == nil {
		t.Error("应返回空切片而非 nil")
	}
	if len(records) != 0 {
		t.Errorf("记录数 = %d", len(records))
	}
}

// 参数校验必须全部发生在任何 I/O 之前 —— 握手一旦开始就无法干净地中止。
//
// 这里刻意传 nil 连接:凡是能走到写字节那一步的输入都会当场 panic,
// 所以这个测试同时钉住了"拒绝"与"在写之前拒绝"两件事。
func TestSocks5ConnectValidatesBeforeAnyIO(t *testing.T) {
	cases := map[string]struct {
		host string
		port int
	}{
		"空目标":   {"", 22},
		"端口为 0": {"127.0.0.1", 0},
		"端口超范围": {"127.0.0.1", 70000},
		"端口为负":  {"127.0.0.1", -1},
		"域名过长":  {strings.Repeat("a", 256), 22},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := socks5Connect(nil, c.host, c.port); err == nil {
				t.Error("非法参数应当在任何 I/O 之前被拒绝")
			}
		})
	}
}

// 域名目标**必须被接受**:中转与链式的拨测要 CONNECT 到中转主机自己的
// 公网地址,而那一栏允许填域名(动态 DNS)。
//
// 主控这边不解析它 —— 解析结果与节点看到的可能不是同一个,
// 而那正是这次拨测要验证的东西。域名走 SOCKS5 的 ATYP=3 交给探测客户端。
//
// 拿 nil 连接跑到写字节那一步会 panic,正好用来证明它没有在校验阶段被拒。
func TestSocks5ConnectAcceptsDomainTarget(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("域名目标在写入之前就被拒绝了 —— 中转与链式的拨测会因此失败")
		}
	}()
	_ = socks5Connect(nil, "relay.example.com", 58739)
}
