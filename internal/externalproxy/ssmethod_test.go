package externalproxy

import (
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 门口收下的每一种加密方法,渲染器都必须拨得出去。
//
// 这是一条真实故障的回归。曾经这两处各有一张表:这里放行了机场最常见的
// chacha20-ietf-poly1305,于是登记、连通性检查、订阅全部正常;而把这条线路
// 设成某个入站的链式出口时,渲染期用的是【入站】那张只有 SS2022 的表,
// 部署因此在十几秒后失败并回滚 —— 报错落在部署记录里,而管理员刚刚才在
// 外部代理页面看到这条线路是绿的。
//
// 两张表分开是对的(我们自己的机器不跑传统 AEAD),但"客户端能拨什么"
// 只能有一份答案。这个测试钉住的正是这一点:哪天有人在这里加一种方法
// 而 singbox 那边不认,失败会发生在这里,不是在某次部署的中途。
func TestEveryAcceptedMethodIsDialable(t *testing.T) {
	for _, method := range SSMethods() {
		t.Run(method, func(t *testing.T) {
			p := Params{Method: method, Password: "somepassword"}
			if err := p.Validate(ProtocolShadowsocks); err != nil {
				t.Fatalf("SSMethods() 列出的 %q 自己都通不过校验: %v", method, err)
			}
			if _, err := singbox.ParseOutboundSSMethod(method); err != nil {
				t.Fatalf("登记时接受了 %q,渲染链式出站时却拒绝: %v", method, err)
			}
		})
	}
}

func TestUnknownMethodStillRejected(t *testing.T) {
	p := Params{Method: "rc4-md5", Password: "x"}
	if err := p.Validate(ProtocolShadowsocks); err == nil {
		t.Fatal("放宽之后连早就该淘汰的方法都收下了")
	}
}
