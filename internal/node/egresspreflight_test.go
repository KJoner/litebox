package node

import (
	"context"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/externalproxy"
)

// 把一条外部代理设成出口(sing-box 入口的链式出站,或 Mieru 入口的出口)
// 之前,必须先按渲染期那条路把它拼成 sing-box 出站。这几个用例盯的是
// 生产上撞到的那一种失败:一条带 simple-obfs 插件的机场 SS 线路,登记、
// 连通性检查、订阅三处都放行,保存出口也成功,而本机 sing-box 的下发在
// check 那一步 FATAL(plugin not found)—— 报错落在另一个页面上。

type egressFixture struct {
	store   *Store
	proxies *externalproxy.Store
	inbound *Inbound
	mieru   *MieruInbound
}

func newEgressFixture(t *testing.T) egressFixture {
	t.Helper()
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMieruInbound(t.Context(), n.ID, mieruParams())
	if err != nil {
		t.Fatal(err)
	}
	return egressFixture{
		store:   store,
		proxies: externalproxy.NewStore(db, store.cipher),
		inbound: only(t, n),
		mieru:   m,
	}
}

func (f egressFixture) ssProxy(t *testing.T, name string, p externalproxy.Params) int64 {
	t.Helper()
	x, err := f.proxies.Create(t.Context(), externalproxy.CreateParams{
		Name: name, DisplayName: name,
		Protocol: externalproxy.ProtocolShadowsocks,
		Server:   "198.51.100.7", Port: 8388,
		Params: p, Origin: externalproxy.OriginManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	return x.ID
}

func TestSetChainRejectsExternalProxySingBoxCannotDial(t *testing.T) {
	f := newEgressFixture(t)
	ctx := context.Background()

	bad := f.ssProxy(t, "bad-plugin", externalproxy.Params{
		Method: "aes-256-gcm", Password: "pw", Plugin: "shadow-tls", PluginOpts: "version=3",
	})
	err := f.store.SetChain(ctx, f.inbound.ID, ChainTargetExternal, bad)
	if err == nil {
		t.Fatal("插件名 sing-box 不认的线路应在保存出口时被拒")
	}
	for _, want := range []string{"shadow-tls", "不能当出口", "外部代理"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应含 %q:%v", want, err)
		}
	}
	// 被拒之后库里一个字都不该变:出口仍是直连。
	in, err := f.store.GetInbound(ctx, f.inbound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if in.ChainTargetKind.Enabled() {
		t.Errorf("被拒的 SetChain 不该写库,现在是 %q", in.ChainTargetKind)
	}

	badKey := f.ssProxy(t, "bad-key", externalproxy.Params{
		Method: "2022-blake3-aes-256-gcm", Password: "6BTQNvCD0Wq2orfxED9hwg==",
	})
	if err := f.store.SetChain(ctx, f.inbound.ID, ChainTargetExternal, badKey); err == nil ||
		!strings.Contains(err.Error(), "密钥长度") {
		t.Errorf("SS2022 密钥长度不对的线路应被拒并说明原因:%v", err)
	}
}

// simple-obfs 是同一个插件的另一个名字,要能设成出口,而且渲染出来的
// 出站用的是 sing-box 认的 obfs-local。
func TestSetMieruChainAcceptsObfsAliasAndTranslatesIt(t *testing.T) {
	f := newEgressFixture(t)
	ctx := context.Background()

	ok := f.ssProxy(t, "airport-obfs", externalproxy.Params{
		Method: "aes-256-gcm", Password: "pw",
		Plugin: "simple-obfs", PluginOpts: "obfs=http;obfs-host=www.bing.com",
	})
	if err := f.store.SetMieruChain(ctx, f.mieru.ID, ChainTargetExternal, ok, 11081); err != nil {
		t.Fatalf("simple-obfs 的线路应能当出口:%v", err)
	}
	m, err := f.store.GetMieruInbound(ctx, f.mieru.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.store.ResolveMieruChain(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := chainOutboundFor(target.Target, target.ChainCode, target.UUID, target.SSPassword)
	if err != nil {
		t.Fatal(err)
	}
	if chain.Prebuilt == nil || chain.Prebuilt.Plugin != "obfs-local" {
		t.Fatalf("渲染出的出站应把插件名翻成 obfs-local,得到 %+v", chain.Prebuilt)
	}

	bad := f.ssProxy(t, "bad-plugin", externalproxy.Params{
		Method: "aes-256-gcm", Password: "pw", Plugin: "shadow-tls",
	})
	err = f.store.SetMieruChain(ctx, f.mieru.ID, ChainTargetExternal, bad, 11082)
	if err == nil || !strings.Contains(err.Error(), "shadow-tls") {
		t.Fatalf("Mieru 出口同样要在保存时拒掉拼不出出站的线路:%v", err)
	}
	// 被拒之后原来的出口(airport-obfs)要原样留着,回环端口也不能被改成 11082。
	m, err = f.store.GetMieruInbound(ctx, f.mieru.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ChainTargetExternalID != ok || m.EgressSocksPort != 11081 {
		t.Errorf("被拒的 SetMieruChain 不该写库:target=%d port=%d", m.ChainTargetExternalID, m.EgressSocksPort)
	}
}
