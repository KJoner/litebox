package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// 入口授权的三个接口:预览不落库、保存后正向与反向看到的是同一张表、审计详情不带地址。
func TestUserAccessEndpoints(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	uid := env.createUser(t, "甲", 0, "")
	nodeID := env.mustNode(t, "n1", "192.0.2.10")
	var inbound int64
	if err := env.db.QueryRow(`SELECT id FROM node_inbounds WHERE node_id = ? AND deleted_at IS NULL`, nodeID).Scan(&inbound); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE node_inbounds SET access_tier_id = 2 WHERE id = ?`, inbound); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"access_mode": "INHERIT", "grants": []map[string]any{{"kind": "SINGBOX", "id": inbound}}}

	resp := env.do(t, http.MethodPost, "/api/users/"+itoa(uid)+"/access/preview", body)
	var diff struct {
		Added    []map[string]any `json:"added"`
		Affected []int64          `json:"affected_node_ids"`
	}
	decodeInto(t, resp, &diff)
	if len(diff.Added) != 1 || len(diff.Affected) != 1 || diff.Affected[0] != nodeID {
		t.Fatalf("预览 = %+v", diff)
	}
	resp = env.do(t, http.MethodGet, "/api/users/"+itoa(uid)+"/access", nil)
	var view struct {
		Mode   string           `json:"access_mode"`
		Grants []map[string]any `json:"grants"`
	}
	decodeInto(t, resp, &view)
	if view.Mode != "INHERIT" || len(view.Grants) != 0 {
		t.Fatalf("预览不该落库,得到 %+v", view)
	}

	resp = env.do(t, http.MethodPut, "/api/users/"+itoa(uid)+"/access", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存 %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = env.do(t, http.MethodGet, "/api/inbound-access/singbox/"+itoa(inbound), nil)
	var ea struct {
		Restricted bool             `json:"restricted"`
		Users      []map[string]any `json:"users"`
	}
	decodeInto(t, resp, &ea)
	if len(ea.Users) != 1 || ea.Users[0]["user_code"] != "user_000001" {
		t.Fatalf("反向授权 = %+v", ea)
	}

	var detail string
	if err := env.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'user.access'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail == "" || strings.Contains(detail, "192.0.2.10") || strings.Contains(detail, "24443") {
		t.Fatalf("审计详情 = %q(不该带地址与端口)", detail)
	}

	// 错误的种类与不存在的入口都是 400,不是 500。
	for _, b := range []map[string]any{
		{"access_mode": "INHERIT", "grants": []map[string]any{{"kind": "NGINX", "id": 1}}},
		{"access_mode": "INHERIT", "grants": []map[string]any{{"kind": "MIERU", "id": 999}}},
		{"access_mode": "EVERYTHING"},
	} {
		resp = env.do(t, http.MethodPut, "/api/users/"+itoa(uid)+"/access", b)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v → %d,期望 400", b, resp.StatusCode)
		}
	}
	resp = env.do(t, http.MethodPut, "/api/inbound-access/relay/1", map[string]any{"restricted": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("中转不走入口授权,得到 %d", resp.StatusCode)
	}
}
