package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// 复核批次三:一个中转配多个落地。预览列出每一条;同一批里端口重复、与机器上已有的监听冲突时整批拒绝,
// 一条都不写;全部通过时在一个事务里全部写入。
func TestRelayBatch(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	const ts = "2026-11-07T00:00:00Z"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 1 = 中转机(上面已经有一个 sing-box 入口占着 40001),2、3 = 两台落地。
	for id, role := range map[int]string{1: "RELAY", 2: "LANDING", 3: "LANDING"} {
		mustExec(`INSERT INTO nodes (id, name, host, proxy_port, reality_dest, reality_privkey_encrypted, reality_pubkey,
			reality_short_id, status, role, created_at, updated_at)
			VALUES (?, ?, ?, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("192.0.2.%d", 40+id), role, ts, ts)
	}
	for id, node := range map[int]int{1: 2, 2: 3} {
		mustExec(`INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'VLESS_REALITY', 443, 'www.example.com', 'e', 'p', 'abcd', 'VLESS_REALITY', ?, ?)`,
			id, node, fmt.Sprintf("in-%d", id), fmt.Sprintf("落地%d", id), ts, ts)
	}
	mustExec(`INSERT INTO node_mieru_inbounds (node_id, display_name, listen_port_start, listen_port_end,
		transport, multiplexing, access_tier_id, created_at, updated_at)
		VALUES (1, '占位', 40001, 40001, 'TCP', 'MULTIPLEXING_LOW', 1, ?, ?)`, ts, ts)

	item := func(name string, port, inbound int) map[string]any {
		return map[string]any{"display_name": name, "listen_port": port, "target_kind": "INBOUND", "target_inbound_id": inbound}
	}
	count := func() int {
		var n int
		env.db.QueryRow(`SELECT COUNT(*) FROM node_relays WHERE node_id = 1 AND deleted_at IS NULL`).Scan(&n)
		return n
	}
	post := func(body map[string]any) (int, map[string]any) {
		resp := env.do(t, http.MethodPost, "/api/nodes/1/relays/batch", body)
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	problems := func(plan map[string]any) []string {
		var out []string
		items, _ := plan["items"].([]any)
		for _, it := range items {
			if p, _ := it.(map[string]any)["problem"].(string); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	// 预览:同一批里两条撞端口,另一条撞上机器上已有的 Mieru 段 —— 三个问题都列出来,不写库。
	code, plan := post(map[string]any{"dry_run": true, "items": []any{
		item("到落地1", 40010, 1), item("到落地2", 40010, 2), item("撞 Mieru", 40001, 1),
	}})
	if code != http.StatusOK || plan["ok"] != false || len(problems(plan)) != 2 || count() != 0 {
		t.Fatalf("预览 = %d %v,问题 %v,已写 %d 条", code, plan, problems(plan), count())
	}
	// 提交同一批:整批拒绝(409 + 预览),一条都不写。
	code, plan = post(map[string]any{"items": []any{
		item("到落地1", 40010, 1), item("到落地2", 40011, 2), item("撞 Mieru", 40001, 1),
	}})
	if code != http.StatusConflict || plan["ok"] != false || count() != 0 {
		t.Fatalf("有一条通不过时整批应当拒绝:%d %v,已写 %d 条", code, plan, count())
	}
	// 落地不存在同样整批拒绝。
	if code, _ = post(map[string]any{"items": []any{item("好的", 40020, 1), item("坏的", 40021, 999)}}); code != http.StatusConflict || count() != 0 {
		t.Fatalf("落地不存在 = %d,已写 %d 条", code, count())
	}
	// 全部通过:一个事务里两条都写进去。
	code, plan = post(map[string]any{"items": []any{item("到落地1", 40010, 1), item("到落地2", 40011, 2)}})
	created, _ := plan["created"].([]any)
	if code != http.StatusCreated || plan["ok"] != true || len(created) != 2 || count() != 2 {
		t.Fatalf("整批写入 = %d %v,已写 %d 条", code, plan, count())
	}
}
