package subscription

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/user"
)

// 端到端验证 V16 的 DB 路径:endpointsByEntry 读条目、attach 到物理节点、
// Expand 遍历。没有 endpoint 时回落到 host(与迁移前逐字节一致);加了
// endpoint 之后订阅按条目下发,每条各自的地址、端口与名字。
func TestSubscriptionFromEndpoints(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNodeFull(t, nodeFixture{
		Name: "节点A", DisplayName: "香港01", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, Host: "192.0.2.10",
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	var inboundID int64
	if err := env.db.QueryRow(`SELECT id FROM node_inbounds WHERE node_id = ?`, nodeID).
		Scan(&inboundID); err != nil {
		t.Fatal(err)
	}

	// ① 没有 endpoint —— 回落到 host,一条 IPv4 条目。
	uris := env.uriEntries(t, u.SubToken)
	if len(uris) != 1 {
		t.Fatalf("没有 endpoint 时应只有一条(host),得到 %d:%v", len(uris), uris)
	}
	if !strings.Contains(uris[0], "@192.0.2.10:24443") || !strings.Contains(uris[0], "%E9%A6%99%E6%B8%AF01") {
		t.Errorf("回落条目 = %q,期望 host:24443 与入口名", uris[0])
	}

	// ② 加地址池 + 三条 endpoint:host(跟随端口跟随名)、额外 IPv4(自定端口与名)、
	//    IPv6(自定端口)。
	var v4ID, v6ID int64
	env.db.Exec(`INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
		VALUES (?, 'V4', '198.51.100.7', 0, '', '')`, nodeID)
	env.db.QueryRow(`SELECT id FROM node_addresses WHERE node_id=? AND family='V4'`, nodeID).Scan(&v4ID)
	env.db.Exec(`INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
		VALUES (?, 'V6', '2602:fed2::1', 1, '', '')`, nodeID)
	env.db.QueryRow(`SELECT id FROM node_addresses WHERE node_id=? AND family='V6'`, nodeID).Scan(&v6ID)

	ins := `INSERT INTO inbound_endpoints (node_id, entry_kind, entry_id, address_id,
		public_port, public_port_end, display_name, sort_order, created_at, updated_at)
		VALUES (?, 'SINGBOX', ?, ?, ?, 0, ?, ?, '', '')`
	// host:跟随(端口 0、名空)
	if _, err := env.db.Exec(ins, nodeID, inboundID, nil, 0, "", 0); err != nil {
		t.Fatal(err)
	}
	// 额外 IPv4:端口 8443、名「香港01-备用」
	if _, err := env.db.Exec(ins, nodeID, inboundID, v4ID, 8443, "香港01-备用", 1); err != nil {
		t.Fatal(err)
	}
	// IPv6:端口 443、名跟随(空 → 香港01-IPV6)
	if _, err := env.db.Exec(ins, nodeID, inboundID, v6ID, 443, "", 2); err != nil {
		t.Fatal(err)
	}

	uris = env.uriEntries(t, u.SubToken)
	if len(uris) != 3 {
		t.Fatalf("三条 endpoint 应产出三条条目,得到 %d:\n%s", len(uris), strings.Join(uris, "\n"))
	}
	if !strings.Contains(uris[0], "@192.0.2.10:24443") || !strings.Contains(uris[0], "%E9%A6%99%E6%B8%AF01") {
		t.Errorf("第一条(host)= %q", uris[0])
	}
	if !strings.Contains(uris[1], "@198.51.100.7:8443") || !strings.Contains(uris[1], "%E5%A4%87%E7%94%A8") {
		t.Errorf("第二条(额外 IPv4)= %q,期望 198.51.100.7:8443 与「备用」", uris[1])
	}
	// IPv6 字面量在 URI 里要加方括号,名字带 -IPV6 后缀。
	if !strings.Contains(uris[2], "@[2602:fed2::1]:443") || !strings.Contains(uris[2], "IPV6") {
		t.Errorf("第三条(IPv6)= %q,期望 [2602:fed2::1]:443 与 -IPV6", uris[2])
	}
}

func (e *subEnv) uriEntries(t *testing.T, token string) []string {
	t.Helper()
	res, err := e.svc.Build(t.Context(), token, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Body)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
