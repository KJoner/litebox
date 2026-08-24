package node

import "testing"

// 「这台机器上 sing-box 那份配置有没有主语」的四种边界。
//
// **生产上撞到了**:一台只有 Mieru 入口的机器显示「未部署 rev 0」并催着
// 去部署,而那份配置里一个入站都没有 —— 部署下去什么也不会发生,
// 那台机器正靠 mita 服务用户。NEVER_DEPLOYED 会带着 needs_deploy,
// 而那是一个假的待办。
func TestHasNoSingBox(t *testing.T) {
	mieru := []*MieruInbound{{ID: 1, DisplayName: "JP-1"}}
	inbound := []*Inbound{{ID: 1, DisplayName: "in-1"}}

	for _, tc := range []struct {
		name string
		node *Node
		want bool
	}{
		{"只有 Mieru 入口", &Node{MieruInbounds: mieru}, true},
		// 全新机器:两种入口都没有。「去装 sing-box」正是下一步,照旧催他。
		{"什么入口都没有的新机器", &Node{}, false},
		// 有 sing-box 入口就按正常流程走 —— 哪怕它同时有 Mieru。
		{"两种入口都有", &Node{Inbounds: inbound, MieruInbounds: mieru}, false},
		// **入口被删光但服务还在跑**:那时恰恰需要下发一次去撤掉那些入站,
		// 报「不适用」会把一个真实的待办藏起来,而被移出的用户凭据还在节点上。
		{
			"下发过、入口被删光",
			&Node{DeployedConfigSHA256: "abc", MieruInbounds: mieru},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasNoSingBox(tc.node); got != tc.want {
				t.Errorf("hasNoSingBox = %v,期望 %v", got, tc.want)
			}
		})
	}
}

// 中转角色永远不适用,与有没有 Mieru 入口无关。
func TestRelayConfigStateIsNotApplicable(t *testing.T) {
	s := &Service{}
	got, need := s.ConfigStatus(t.Context(), &Node{Role: RoleRelay})
	if got != ConfigNotApplicable || need {
		t.Errorf("中转机的配置状态 = %s(needs_deploy=%v)", got, need)
	}
}
