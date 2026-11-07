package node

import (
	"io"
	"log/slog"
	"testing"

	"github.com/litebox/litebox/internal/mieru"
)

func deployedMieru() *MieruInbound {
	ports := mieru.PortRange{Start: 31000, End: 31004}
	return &MieruInbound{
		ID: 7, Enabled: true,
		ListenPorts: ports, Transport: mieru.Transport("TCP"), Multiplexing: mieru.Multiplexing("MULTIPLEXING_LOW"),
		DeployedTransport: mieru.Transport("TCP"), DeployedMultiplexing: mieru.Multiplexing("MULTIPLEXING_LOW"),
		DeployedListenPortStart: ports.Start, DeployedListenPortEnd: ports.End,
		DeployedConfigSHA256: "full-old", DeployedShapeSHA256: "shape",
	}
}

// R05:自动下发只做「只动了用户」的入口;已一致、没上过节点、停用的跳过;形状变了的拦下等人手动下发。
func TestPlanMieruAuto(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(m *MieruInbound)
		full, shape string
		want        mieruAutoPlan
	}{
		{"只动了用户", nil, "full-new", "shape", mieruAutoReload},
		{"已一致", nil, "full-old", "shape", mieruAutoSkip},
		{"端口或出口也改了", nil, "full-new", "shape-new", mieruAutoBlocked},
		{"没上过节点", func(m *MieruInbound) { m.DeployedTransport = "" }, "full-new", "shape", mieruAutoSkip},
		{"已停用", func(m *MieruInbound) { m.Enabled = false }, "full-new", "shape", mieruAutoSkip},
		{"存量行没有哈希、几列一致", func(m *MieruInbound) { m.DeployedConfigSHA256, m.DeployedShapeSHA256 = "", "" },
			"full-new", "shape", mieruAutoReload},
		{"存量行没有哈希、端口改了", func(m *MieruInbound) {
			m.DeployedConfigSHA256, m.DeployedShapeSHA256 = "", ""
			m.ListenPorts = mieru.PortRange{Start: 32000, End: 32004}
		}, "full-new", "shape", mieruAutoBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := deployedMieru()
			if c.mutate != nil {
				c.mutate(m)
			}
			if got := planMieruAuto(m, c.full, c.shape); got != c.want {
				t.Fatalf("得到 %v,期望 %v", got, c.want)
			}
		})
	}
}

// 形状哈希不看用户:同一个入口换了用户,形状不变、整份变。
func TestMieruHashesIgnoreUsersInShape(t *testing.T) {
	base := mieru.ServerConfig{PortBindings: []mieru.PortBinding{{PortRange: "31000-31004", Protocol: "TCP"}}}
	a, b := base, base
	a.Users = []mieru.User{{Name: "user_000001", Password: "x"}}
	b.Users = []mieru.User{{Name: "user_000002", Password: "y"}}
	ra, _ := a.MarshalIndent()
	rb, _ := b.MarshalIndent()
	fa, sa, err := mieruHashes(a, ra)
	if err != nil {
		t.Fatal(err)
	}
	fb, sb, _ := mieruHashes(b, rb)
	if fa == fb || sa != sb {
		t.Fatalf("整份 %v / 形状 %v", fa == fb, sa == sb)
	}
}

// 占位用户:同一个入口每次一样(配置哈希不抖),不同入口不同,也不是任何用户代码的形状。
func TestMieruIdleUserDeterministic(t *testing.T) {
	store, _ := newTestStore(t)
	svc := NewService(ServiceOptions{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a1, a2, b := svc.mieruIdleUser(1), svc.mieruIdleUser(1), svc.mieruIdleUser(2)
	if a1 != a2 {
		t.Fatal("同一个入口的占位用户每次不一样 —— 配置哈希会一直抖")
	}
	if a1.Password == b.Password {
		t.Fatal("不同入口的占位口令相同")
	}
	if a1.Name != mieruIdleUserName || len(a1.Password) < 32 {
		t.Fatalf("占位用户 = %+v", a1)
	}
}
