package singbox

import (
	"strings"
	"testing"
)

func testSSKey(t *testing.T) string {
	t.Helper()
	k, err := GenerateSSKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustSnellKey(t *testing.T) string {
	t.Helper()
	k, err := GenerateSnellKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// 不计流量的入口:凭据一个不少,name 一个都没有,白名单里也没有它们。
//
// 这是 V14 验证 Snell 时当成"坑"记下来的那条规矩(漏了 name 会静默不计量)
// 反过来用:现在它是一个显式的开关,而三种协议都要走同一处。
func TestUnmeteredInboundKeepsCredentialsDropsNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(p *InboundParams)
	}{
		{"VLESS", func(p *InboundParams) {}},
		{"Shadowsocks", func(p *InboundParams) {
			p.Protocol = ProtocolShadowsocks
			p.SSMethod = SSMethodAES128GCM
			p.SSPassword = testSSKey(t)
			for i := range p.Users {
				p.Users[i].SSPassword = testSSKey(t)
			}
		}},
		{"Snell", func(p *InboundParams) {
			p.Protocol = ProtocolSnell
			p.SnellVersion = SnellVersion6
			p.SnellPSK = mustSnellKey(t)
			for i := range p.Users {
				p.Users[i].SnellUserKey = mustSnellKey(t)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := v3Params()
			tc.setup(&params.Inbounds[0])
			params.Inbounds[0].Unmetered = true

			cfg, err := Render(params)
			if err != nil {
				t.Fatal(err)
			}
			in := cfg.Inbounds[0]
			if len(in.Users) != 2 {
				t.Fatalf("凭据必须一个不少,实际 %d 个", len(in.Users))
			}
			for _, u := range in.Users {
				if u.Name != "" {
					t.Errorf("不计流量的入口渲染出了 name %q —— 有 name 就有计数器", u.Name)
				}
				if u.Credential() == "" {
					t.Error("凭据被丢了:用户手上那份配置会连不上")
				}
			}
			if len(cfg.Experimental.V2RayAPI.Stats.Users) != 0 {
				t.Errorf("没有名字的用户不该进统计白名单:%v", cfg.Experimental.V2RayAPI.Stats.Users)
			}
			if err := AssertStatsConsistent(cfg); err != nil {
				t.Errorf("白名单断言应该放过没有名字的用户:%v", err)
			}
			if !UnmeteredOf(in) {
				t.Error("渲染结果认不出自己是不计流量的")
			}
			raw, _ := cfg.MarshalIndent()
			if strings.Contains(string(raw), `"name"`) {
				t.Errorf("配置里不该出现 name 字段(omitempty):\n%s", raw)
			}
		})
	}
}

// 同一个人同时在一个计量入口与一个不计流量入口上:白名单里有他一份
// (由计量那个带进去),不计流量那个入口上他没有名字。
func TestUserOnMeteredAndUnmeteredInboundsCountedOnce(t *testing.T) {
	params := v3Params()
	second := params.Inbounds[0]
	second.ID = 2
	second.Tag = "in-2"
	second.ListenPort = 24444
	second.Unmetered = true
	params.Inbounds = append(params.Inbounds, second)

	cfg, err := Render(params)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Experimental.V2RayAPI.Stats.Users; len(got) != 2 {
		t.Errorf("白名单应该只有计量入口上的两个用户,实际 %v", got)
	}
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Error(err)
	}
	if UnmeteredOf(cfg.Inbounds[0]) || !UnmeteredOf(cfg.Inbounds[1]) {
		t.Error("两个入口的计量属性被搞反了")
	}
}

// 空用户列表不算不计流量 —— 那是共享 Snell,另有判据。
func TestUnmeteredOfIgnoresEmptyUserList(t *testing.T) {
	if UnmeteredOf(Inbound{Type: "snell"}) {
		t.Error("空用户列表被判成了不计流量")
	}
	if !UnmeteredOf(Inbound{Type: "vless", Users: []InboundUser{{UUID: "x"}}}) {
		t.Error("全部没名字却没被判成不计流量")
	}
	if UnmeteredOf(Inbound{Type: "vless", Users: []InboundUser{{UUID: "x"}, {Name: "user_000001", UUID: "y"}}}) {
		t.Error("有一个带名字的就是计量入口")
	}
}
