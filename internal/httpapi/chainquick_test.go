package httpapi

import (
	"net/http"
	"testing"
)

// 快速组链:预览说清新建与改动;创建时出口在同一步写进库(不部署)、
// 「使用用户」写成仅指定用户;注定不可用的链(同机)整个拒绝、一行不写。
func TestQuickChainCreate(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	uid := env.createUser(t, "甲", 0, "")
	entry := env.mustNode(t, "entry", "192.0.2.11")
	landing := env.mustNode(t, "landing", "192.0.2.12")
	var target int64
	if err := env.db.QueryRow(`SELECT id FROM node_inbounds WHERE node_id = ?`, landing).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE node_inbounds SET deployed_protocol = protocol WHERE id = ?`, target); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"entry_node_id": entry, "display_name": "经 landing 出", "protocol": "SHADOWSOCKS",
		"listen_port": 31555, "target_kind": "INBOUND", "target_id": target, "user_ids": []int64{uid},
	}
	resp := env.do(t, http.MethodPost, "/api/chains/quick-preview", body)
	var pv struct {
		Problems []string `json:"problems"`
		Path     struct {
			Hops []map[string]any `json:"hops"`
		} `json:"path"`
	}
	decodeInto(t, resp, &pv)
	if len(pv.Problems) != 0 || len(pv.Path.Hops) != 2 {
		t.Fatalf("预览 = %+v", pv)
	}
	resp = env.do(t, http.MethodPost, "/api/chains/quick", body)
	var created struct {
		Inbound struct {
			ID int64 `json:"id"`
		} `json:"inbound"`
		AccessError string `json:"access_error"`
	}
	decodeInto(t, resp, &created)
	if created.Inbound.ID == 0 || created.AccessError != "" {
		t.Fatalf("创建 = %+v", created)
	}
	var kind string
	var tgt, restricted int64
	env.db.QueryRow(`SELECT chain_target_kind, chain_target_inbound_id, restricted FROM node_inbounds WHERE id = ?`, created.Inbound.ID).
		Scan(&kind, &tgt, &restricted)
	if kind != "INBOUND" || tgt != target || restricted != 1 {
		t.Fatalf("出口应当在创建时写进库、入口设成仅指定用户,得到 kind=%q target=%d restricted=%d", kind, tgt, restricted)
	}

	// 同机:整个拒绝,一行不写。
	body["entry_node_id"] = landing
	body["listen_port"] = 31556
	resp = env.do(t, http.MethodPost, "/api/chains/quick", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同机组链应当 409,得到 %d", resp.StatusCode)
	}
	var after int
	env.db.QueryRow(`SELECT COUNT(*) FROM node_inbounds WHERE node_id = ?`, landing).Scan(&after)
	if after != 1 {
		t.Fatalf("被拒绝的组链在落地上留下了入口(%d 个)", after)
	}

	resp = env.do(t, http.MethodGet, "/api/chain-path/singbox/"+itoa(created.Inbound.ID), nil)
	var path struct {
		Hops   []map[string]any `json:"hops"`
		Egress string           `json:"egress"`
	}
	decodeInto(t, resp, &path)
	if len(path.Hops) != 2 || path.Egress == "" {
		t.Fatalf("路径 = %+v", path)
	}
}
