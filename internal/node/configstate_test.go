package node

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/singbox"
)

// stubUsers 让配置状态的测试能控制"库里该有哪些用户"。
type stubUsers struct {
	users      []singbox.User
	mieruUsers []mieru.User
}

func (s *stubUsers) UsersForInbound(context.Context, int64) ([]singbox.User, error) {
	return s.users, nil
}

func (s *stubUsers) MieruUsersForInbound(context.Context, int64) ([]mieru.User, error) {
	return s.mieruUsers, nil
}

func newConfigStateFixture(t *testing.T, users ...singbox.User) (*Service, *Node) {
	t.Helper()
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	svc := NewService(ServiceOptions{
		Store:  store,
		Users:  &stubUsers{users: users},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return svc, n
}

// markDeployedWithCurrent 把节点标成"已部署当前这份配置"。
func markDeployedWithCurrent(t *testing.T, svc *Service, id int64) {
	t.Helper()
	desired, err := svc.desiredConfig(t.Context(), id)
	if err != nil {
		t.Fatalf("渲染期望配置: %v", err)
	}
	n, err := svc.store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	deployed := make([]DeployedInbound, 0, len(n.Inbounds))
	for _, in := range n.Inbounds {
		deployed = append(deployed, DeployedInbound{
			ID: in.ID, Protocol: in.Protocol, SSMethod: in.SSMethod, TCPFastOpen: in.TCPFastOpen})
	}
	if err := svc.store.MarkDeployed(t.Context(), id, desired.SHA256, deployed); err != nil {
		t.Fatalf("记录已部署哈希: %v", err)
	}
}

func reload(t *testing.T, svc *Service, id int64) *Node {
	t.Helper()
	n, err := svc.store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("重新读取节点: %v", err)
	}
	return n
}

// 从未部署过的节点必须报 NEVER_DEPLOYED 并催部署 ——
// 这是新加一台机器之后管理员唯一该做的事。
func TestConfigStateNeverDeployed(t *testing.T) {
	svc, n := newConfigStateFixture(t)

	state, needs := svc.ConfigStatus(t.Context(), n)
	if state != ConfigNeverDeployed {
		t.Errorf("配置状态 = %s,期望 NEVER_DEPLOYED", state)
	}
	if !needs {
		t.Error("从未部署过的节点应当提示部署")
	}
}

func TestConfigStateInSyncAfterDeploy(t *testing.T) {
	svc, n := newConfigStateFixture(t, singbox.User{Code: "user_000001", UUID: "5f8d1b2e-1c3a-4f6b-9d0e-2a7c4b8e1f30"})
	markDeployedWithCurrent(t, svc, n.ID)

	state, needs := svc.ConfigStatus(t.Context(), reload(t, svc, n.ID))
	if state != ConfigInSync {
		t.Errorf("配置状态 = %s,期望 IN_SYNC", state)
	}
	if needs {
		t.Error("配置一致时不该提示部署")
	}
}

// 用户变了但还没部署 —— 这正是「数据库改了而节点没改」的那个窗口,
// 也是这个字段存在的理由:运行状态仍是 ONLINE,看不出凭据还没下发。
func TestConfigStatePendingAfterUserChange(t *testing.T) {
	stub := &stubUsers{users: []singbox.User{
		{Code: "user_000001", UUID: "5f8d1b2e-1c3a-4f6b-9d0e-2a7c4b8e1f30"},
	}}
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{
		Store: store, Users: stub,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	markDeployedWithCurrent(t, svc, n.ID)

	stub.users = append(stub.users, singbox.User{
		Code: "user_000002", UUID: "9b1e77c4-3d52-4a80-8f16-6c0d5e2b7a94",
	})

	state, needs := svc.ConfigStatus(t.Context(), reload(t, svc, n.ID))
	if state != ConfigPending {
		t.Errorf("新增用户后配置状态 = %s,期望 PENDING", state)
	}
	if !needs {
		t.Error("库里已变更时应当提示部署")
	}
}

func TestConfigStateDeployFailedWhenStillDivergent(t *testing.T) {
	stub := &stubUsers{}
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServiceOptions{
		Store: store, Users: stub,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	markDeployedWithCurrent(t, svc, n.ID)

	stub.users = []singbox.User{{Code: "user_000001", UUID: "5f8d1b2e-1c3a-4f6b-9d0e-2a7c4b8e1f30"}}
	if err := store.MarkDeployFailed(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}

	state, needs := svc.ConfigStatus(t.Context(), reload(t, svc, n.ID))
	if state != ConfigDeployFailed {
		t.Errorf("配置状态 = %s,期望 DEPLOY_FAILED", state)
	}
	if !needs {
		t.Error("部署失败且配置仍有差异时应当提示重新部署")
	}
}

// 部署失败之后管理员把改动撤了回去:节点上跑的就是库里现在这份,
// 没有任何东西需要下发。此时若仍报 DEPLOY_FAILED + 催部署,
// 管理员会去重启一台其实完全正常的机器 —— 而重启会踢掉它上面全部在线连接。
func TestConfigStateDeployFailedButRevertedReadsInSync(t *testing.T) {
	svc, n := newConfigStateFixture(t)
	markDeployedWithCurrent(t, svc, n.ID)
	if err := svc.store.MarkDeployFailed(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}

	state, needs := svc.ConfigStatus(t.Context(), reload(t, svc, n.ID))
	if state != ConfigInSync {
		t.Errorf("改动撤回后配置状态 = %s,期望 IN_SYNC", state)
	}
	if needs {
		t.Error("没有任何待下发的改动时不该提示部署")
	}
}

// 已禁用的节点部署不了(Deploy 会直接拒绝),所以不管配置差多少都不催。
func TestConfigStateDisabledNeverNags(t *testing.T) {
	svc, n := newConfigStateFixture(t)

	if err := svc.store.SetEnabled(t.Context(), n.ID, false); err != nil {
		t.Fatal(err)
	}
	state, needs := svc.ConfigStatus(t.Context(), reload(t, svc, n.ID))
	if state != ConfigNeverDeployed {
		t.Errorf("配置状态 = %s,期望仍如实报 NEVER_DEPLOYED", state)
	}
	if needs {
		t.Error("已禁用的节点不应当提示部署 —— 部署接口本来就会拒绝")
	}
}

// 批量与逐个必须给出一致的结果:节点列表走批量、详情页走逐个,
// 两处显示不同的配置状态会让人以为刷新一下就变了。
func TestConfigStatusesMatchesSingle(t *testing.T) {
	svc, n := newConfigStateFixture(t)
	nodes, err := svc.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	batch := svc.ConfigStatuses(t.Context(), nodes)
	state, needs := svc.ConfigStatus(t.Context(), n)
	got, ok := batch[n.ID]
	if !ok {
		t.Fatalf("批量结果里没有节点 %d", n.ID)
	}
	if got.State != state || got.NeedsDeploy != needs {
		t.Errorf("批量 = %+v,逐个 = %s/%v", got, state, needs)
	}
}
