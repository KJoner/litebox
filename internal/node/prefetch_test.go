package node

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/user"
)

// 节点列表的批量预取与逐台渲染必须给出逐字节相同的配置(VNext PERF03):两条路只是凭据从哪里来不同。
// 覆盖等级继承、整机额外授权、停用用户、SS 入口与一条链式凭据;顺带确认批量那条路的查询确实少了。
func TestPrefetchRenderMatchesPerNode(t *testing.T) {
	store, db := newTestStore(t)
	users := user.NewStore(db, store.cipher)
	svc := NewService(ServiceOptions{Store: store, Users: users, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := t.Context()

	var nodeIDs []int64
	var firstInbound int64
	for i := 0; i < 3; i++ {
		p := defaultCreateParams()
		p.Name, p.Host, p.ProxyPort = fmt.Sprintf("n%d", i), fmt.Sprintf("192.0.2.%d", 30+i), 24443
		n, err := store.Create(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		nodeIDs = append(nodeIDs, n.ID)
		if i == 0 {
			firstInbound = n.Inbounds[0].ID
		}
		for k := 1; k <= 3; k++ {
			ip := InboundParams{Protocol: "SHADOWSOCKS", ListenPort: 30000 + k, PublicPort: 30000 + k,
				DisplayName: fmt.Sprintf("n%d-ss%d", i, k), AccessTierID: int64(1 + k%2)}
			if _, err := store.CreateInbound(ctx, n.ID, ip); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 4; i++ {
		u, err := users.Create(ctx, user.CreateParams{DisplayName: fmt.Sprintf("u%d", i),
			AccessTierID: int64(1 + i%2), NodeIDs: []int64{nodeIDs[i%3]}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			db.Exec(`UPDATE proxy_users SET status = 'DISABLED' WHERE id = ?`, u.ID)
		}
	}
	// 第二台机器的一个入口链到第一台机器的第一个入口:落地上要多一份链路凭据。
	enc, _ := store.cipher.Encrypt("11111111-2222-3333-4444-555555555555")
	if _, err := db.Exec(`UPDATE node_inbounds SET chain_target_kind = 'INBOUND', chain_target_inbound_id = ?,
		chain_code = 'chain_000001', chain_uuid_encrypted = ? WHERE node_id = ? AND listen_port = 30001`,
		firstInbound, enc, nodeIDs[1]); err != nil {
		t.Fatal(err)
	}

	// 链式渲染要求落地入站部署过。
	if _, err := db.Exec(`UPDATE node_inbounds SET deployed_protocol = protocol`); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q0 := database.QueryCount(db)
	pf, err := svc.prefetchRender(ctx, nodes)
	if err != nil || pf == nil || len(pf.ids) == 0 {
		t.Fatalf("预取没有生效:%v %v", pf, err)
	}
	if len(pf.chain[firstInbound]) != 1 {
		t.Fatalf("链路凭据没有预取到:%v", pf.chain)
	}
	for _, id := range nodeIDs {
		if _, err := svc.desiredConfigWith(ctx, id, pf); err != nil {
			t.Fatal(err)
		}
	}
	batched := database.QueryCount(db) - q0

	q0 = database.QueryCount(db)
	for _, id := range nodeIDs {
		single, err := svc.desiredConfig(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		withPF, err := svc.desiredConfigWith(ctx, id, pf)
		if err != nil {
			t.Fatal(err)
		}
		if single.SHA256 != withPF.SHA256 {
			t.Errorf("节点 %d:逐台渲染与批量预取的配置不一致", id)
		}
	}
	perNode := database.QueryCount(db) - q0
	if q0 >= 0 && batched >= perNode/2 {
		t.Errorf("批量那条路没有省下查询:批量 %d 条,逐台(含一遍批量复算)%d 条", batched, perNode)
	}
}
