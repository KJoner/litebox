package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/crypto"
)

// v23.3 链路画布接口:只给管理员;快照里没有任何凭据;快照、候选、预览只读;
// 预览之后数据变了,带着旧修订提交要被拒(409,要求重新预览)。
func TestTopologyEndpoints(t *testing.T) {
	env := newTestEnv(t)
	const ts = "2026-11-14T00:00:00Z"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 未登录:拿不到。
	resp := env.do(t, http.MethodGet, "/api/topology", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录取全图应当 401,得到 %d", resp.StatusCode)
	}
	resp.Body.Close()
	env.login(t)

	const secret = "SECRET-uuid-should-never-appear"
	cipher, err := crypto.NewCipher("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := cipher.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 3; id++ {
		mustExec(`INSERT INTO nodes (id, name, host, proxy_port, reality_dest, reality_privkey_encrypted, reality_pubkey,
			reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 24443, 'www.example.com', ?, 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("192.0.2.%d", 60+id), enc, ts, ts)
	}
	// 1:入口 A(链到 R),2:落地 B,3:转发 R → B。
	mustExec(`INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, reality_dest,
		reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol, chain_code, chain_uuid_encrypted,
		created_at, updated_at) VALUES (2, 2, 'in-2', '落地B', 'VLESS_REALITY', 443, 'www.example.com', ?, 'p', 'abcd',
		'VLESS_REALITY', '', '', ?, ?)`, enc, ts, ts)
	mustExec(`INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id, target_addr_mode,
		created_at, updated_at) VALUES (5, 3, '转发R', 30443, 'INBOUND', 2, 'DEFAULT', ?, ?)`, ts, ts)
	mustExec(`INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, reality_dest,
		reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol,
		chain_target_kind, chain_target_relay_id, chain_target_addr_mode, chain_code, chain_uuid_encrypted,
		created_at, updated_at) VALUES (1, 1, 'in-1', '入口A', 'VLESS_REALITY', 443, 'www.example.com', ?, 'p', 'abcd',
		'VLESS_REALITY', 'RELAY', 5, 'DEFAULT', 'chain_000001', ?, ?, ?)`, enc, enc, ts, ts)

	get := func(path string) (int, map[string]any, string) {
		t.Helper()
		resp := env.do(t, http.MethodGet, path, nil)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out, string(raw)
	}
	code, snap, raw := get("/api/topology")
	if code != http.StatusOK {
		t.Fatalf("取全图 %d: %s", code, raw)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, enc) {
		t.Fatal("拓扑快照里出现了凭据或它的密文")
	}
	entries, _ := snap["entries"].([]any)
	edges, _ := snap["edges"].([]any)
	if len(entries) != 3 || len(edges) != 2 {
		t.Fatalf("应有三张卡片(A、B、R)两条边,得到 %d / %d:%s", len(entries), len(edges), raw)
	}
	var aToR map[string]any
	for _, e := range edges {
		if m := e.(map[string]any); m["from"] == "SINGBOX:1" {
			aToR = m
		}
	}
	if aToR == nil || aToR["to"] != "NGINX:5" || aToR["dial"] != "192.0.2.63:30443" || aToR["kind"] != "PROXY" {
		t.Fatalf("A → R 的边 = %v", aToR)
	}
	rev, _ := snap["revision"].(string)

	// 候选:B 能选,A 自己不在列表里,Mieru 之类不存在;R 也能选(经它到 B)。
	code, cands, raw := get("/api/topology/targets?from=SINGBOX:1")
	if code != http.StatusOK || strings.Contains(raw, secret) {
		t.Fatalf("候选 %d: %s", code, raw)
	}
	groups, _ := cands["groups"].([]any)
	found := map[string]bool{}
	for _, g := range groups {
		for _, c := range g.(map[string]any)["candidates"].([]any) {
			m := c.(map[string]any)
			found[m["key"].(string)] = m["disabled"] != true
		}
	}
	if !found["SINGBOX:2"] || !found["NGINX:5"] {
		t.Fatalf("B 与 R 都应当可选,得到 %v", found)
	}
	if _, self := found["SINGBOX:1"]; self {
		t.Fatal("来源自己不该出现在候选里")
	}

	// 预览之后改了数据:带旧修订提交被拒。
	mustExec(`UPDATE node_relays SET listen_port = 30444 WHERE id = 5`)
	resp = env.do(t, http.MethodPost, "/api/inbounds/1/chain", map[string]any{
		"target_kind": "INBOUND", "target_inbound_id": 2, "revision": rev})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("预览之后数据变了,提交应当 409,得到 %d:%s", resp.StatusCode, b)
	}
	var stale map[string]any
	json.NewDecoder(resp.Body).Decode(&stale)
	if stale["code"] != "STALE_PREVIEW" {
		t.Fatalf("409 的 code = %v", stale["code"])
	}

	// 删被引用的转发:拒绝并点名。
	del := env.do(t, http.MethodDelete, "/api/relays/5", nil)
	defer del.Body.Close()
	if del.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(del.Body)
		t.Fatalf("删被引用的转发应当 409,得到 %d:%s", del.StatusCode, b)
	}
}
