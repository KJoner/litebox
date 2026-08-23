package node

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// 每一档的取舍。判据保守:**只有确定同步了才放行** —— 误拦的代价是
// 管理员被要求先部署落地(那本来就该做),漏拦的代价是一次重启服务加一次回滚,
// 外加一句指向错误方向的报错。
func TestChainTargetBlocksIsConservative(t *testing.T) {
	for _, tc := range []struct {
		state ConfigState
		block bool
		why   string
	}{
		{ConfigInSync, false, "落地上跑的就是库里这一份,凭据一定在"},
		{ConfigNeverDeployed, true, "落地上根本没有 sing-box 配置"},
		{ConfigPending, true, "库里有未下发的变更,链路凭据很可能就在里面"},
		{ConfigDeployFailed, true, "落地在跑回滚后的旧配置,新凭据没写上去"},
		{ConfigUnknown, true, "落地的配置渲染不出来,放行只是把失败推迟十几秒还多赔两次重启"},
		{ConfigNotApplicable, false, "中转角色压根不该当落地,交给渲染期报更准确"},
	} {
		if got := chainTargetBlocks(tc.state); got != tc.block {
			t.Errorf("%s 应当 block=%v(%s),得到 %v", tc.state, tc.block, tc.why, got)
		}
	}
}

// 端到端:落地待部署时,Deploy 必须在【碰节点之前】就拒绝。
//
// 这一条钉住的是接线 —— 检查写好了却没接进 Deploy 的话,行为与没做一样,
// 而单测 chainTargetBlocks 照样全绿。
func TestDeployRejectedWhenChainTargetOutOfSync(t *testing.T) {
	svc, host, landing := chainPair(t)

	// 配出口。SetChain 只写库,落地上此刻还没有这份凭据。
	if err := svc.store.SetChain(t.Context(),
		only(t, host).ID, ChainTargetInbound, only(t, landing).ID); err != nil {
		t.Fatalf("配置链式出口: %v", err)
	}

	// deployer 是 nil:检查必须在它之前跑完,走到那里会 panic —— 那本身
	// 就是"闸门没拦住"最直接的证据。
	_, err := svc.Deploy(t.Context(), host.ID)
	if !errors.Is(err, ErrChainTargetOutOfSync) {
		t.Fatalf("落地待部署时应当拒绝,得到:%v", err)
	}
	// 报错要指名道姓,并说清楚该先做什么 —— 只说"配置未同步"的话,
	// 管理员会盯着【这台】机器查,而缺的东西在另一台上。
	for _, want := range []string{landing.DisplayName, "设置出口", "先部署"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q:%v", want, err)
		}
	}
}

// 落地同步之后就该放行 —— 拦得太宽会让链式节点永远部署不了。
func TestDeployAllowedOnceChainTargetInSync(t *testing.T) {
	svc, host, landing := chainPair(t)
	if err := svc.store.SetChain(t.Context(),
		only(t, host).ID, ChainTargetInbound, only(t, landing).ID); err != nil {
		t.Fatal(err)
	}
	// 落地把含链路凭据的那一份部署上去。
	markDeployedWithCurrent(t, svc, landing.ID)

	h, err := svc.store.Get(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.checkChainTargetsReady(t.Context(), h.Inbounds); err != nil {
		t.Errorf("落地已同步却仍被拦下:%v", err)
	}
}

// 外部代理落地不参与这个检查:那是别人的机器,我们只拿凭据去连它,
// 不往上面写任何东西,也就无所谓同步不同步。
func TestChainToExternalProxySkipsTheCheck(t *testing.T) {
	svc, host, _ := chainPair(t)
	h, err := svc.store.Get(t.Context(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := only(t, h)
	in.ChainTargetKind = ChainTargetExternal
	in.ChainTargetExternalID = 7

	if err := svc.checkChainTargetsReady(t.Context(), []*Inbound{in}); err != nil {
		t.Errorf("外部代理落地不该被这个检查拦:%v", err)
	}
}

// chainPair 造一台中转和一台【已经部署过】的落地。
//
// 落地必须先有 deployed_protocol,否则 SetChain 会以
// ErrChainTargetNotDeployed 提前挡下来 —— 那是另一条更早的防线。
func chainPair(t *testing.T) (svc *Service, host, landing *Node) {
	t.Helper()
	store, _ := newTestStore(t)
	svc = NewService(ServiceOptions{
		Store:  store,
		Users:  &stubUsers{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	lp := defaultCreateParams()
	lp.Name, lp.DisplayName, lp.Host, lp.ProxyPort = "node-landing", "落地机", "192.0.2.20", 24444
	var err error
	if landing, err = store.Create(t.Context(), lp); err != nil {
		t.Fatalf("创建落地: %v", err)
	}
	markDeployedWithCurrent(t, svc, landing.ID)

	hp := defaultCreateParams()
	hp.Name, hp.DisplayName = "node-host", "中转机"
	if host, err = store.Create(t.Context(), hp); err != nil {
		t.Fatalf("创建中转: %v", err)
	}
	// 中转自己也标成已部署,免得它自己的状态混进断言里。
	markDeployedWithCurrent(t, svc, host.ID)
	return svc, host, landing
}
