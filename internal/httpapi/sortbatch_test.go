package httpapi

import (
	"net/http"
	"testing"
)

// 批量排序:旧值对得上时整组写入;期间有一项被别人改过时整组拒绝、一行不改(A19)。
func TestSortBatchDetectsConflict(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	a := env.mustNode(t, "a", "192.0.2.21")
	b := env.mustNode(t, "b", "192.0.2.22")
	order := func(id int64) int {
		var v int
		env.db.QueryRow(`SELECT sort_order FROM nodes WHERE id = ?`, id).Scan(&v)
		return v
	}
	oa, ob := order(a), order(b)

	resp := env.do(t, http.MethodPost, "/api/sort-order/batch", map[string]any{"items": []map[string]any{
		{"kind": "NODE", "id": a, "sort_order": 20, "expected": oa},
		{"kind": "NODE", "id": b, "sort_order": 10, "expected": ob},
	}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || order(a) != 20 || order(b) != 10 {
		t.Fatalf("状态 %d,a=%d b=%d", resp.StatusCode, order(a), order(b))
	}

	// 另一个标签页把 b 改成 15;这边仍按旧值 10 提交 —— 整组拒绝,a 也不能动。
	if _, err := env.db.Exec(`UPDATE nodes SET sort_order = 15 WHERE id = ?`, b); err != nil {
		t.Fatal(err)
	}
	resp = env.do(t, http.MethodPost, "/api/sort-order/batch", map[string]any{"items": []map[string]any{
		{"kind": "NODE", "id": a, "sort_order": 5, "expected": 20},
		{"kind": "NODE", "id": b, "sort_order": 6, "expected": 10},
	}})
	var body struct {
		Conflicts []map[string]any `json:"conflicts"`
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("期望 409,得到 %d", resp.StatusCode)
	}
	decodeInto(t, resp, &body)
	if len(body.Conflicts) != 1 || order(a) != 20 || order(b) != 15 {
		t.Fatalf("冲突 = %v,a=%d b=%d(应当一行不改)", body.Conflicts, order(a), order(b))
	}

	// 外部代理不走批量(要锁字段),种类不认得也拒绝。
	resp = env.do(t, http.MethodPost, "/api/sort-order/batch", map[string]any{"items": []map[string]any{
		{"kind": "EXTERNAL", "id": 1, "sort_order": 1, "expected": 0},
	}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("外部代理走批量应当 400,得到 %d", resp.StatusCode)
	}
}
