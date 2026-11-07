package node

import (
	"database/sql"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

func queuedKind(t *testing.T, db *sql.DB, nodeID int64, kind string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM apply_queue WHERE node_id = ? AND kind = ?`, nodeID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// G01:入口等级变了(谁能在这台机器上认证变了)、换了 Snell 的 psk,下发意图与改动同一个事务提交;
// 意图写不进去时改动一起回滚。别的改动(协议、端口)只置 NeedsDeploy,不排队 —— 那要管理员挑时机。
func TestInboundChangesCommitIntent(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProbe(t.Context(), n.ID, "amd64", "v1.14.0-litebox", RequiredBuildTag, 457, true); err != nil {
		t.Fatal(err)
	}
	in := only(t, n)
	clear := func() {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM apply_queue`); err != nil {
			t.Fatal(err)
		}
	}

	clear()
	p := inboundParamsOf(in)
	p.ListenPort = in.ListenPort + 1
	if _, _, err := store.UpdateInbound(t.Context(), in.ID, p); err != nil {
		t.Fatal(err)
	}
	if queuedKind(t, db, n.ID, "SINGBOX") {
		t.Fatal("改端口只该置待部署,不该自动排队")
	}

	clear()
	p = inboundParamsOf(mustInbound(t, store, in.ID))
	p.AccessTierID = 2
	if _, effect, err := store.UpdateInbound(t.Context(), in.ID, p); err != nil || !effect.TierChanged {
		t.Fatalf("改等级:%+v %v", effect, err)
	}
	if !queuedKind(t, db, n.ID, "SINGBOX") {
		t.Fatal("等级调高之后队列里没有这台机器(被移出的人的凭据会一直留着)")
	}

	// 意图写不进去:等级改动一起回滚。
	if _, err := db.Exec(`ALTER TABLE apply_queue RENAME TO apply_queue_gone`); err != nil {
		t.Fatal(err)
	}
	p = inboundParamsOf(mustInbound(t, store, in.ID))
	p.AccessTierID = 3
	if _, _, err := store.UpdateInbound(t.Context(), in.ID, p); err == nil {
		t.Fatal("意图写不进去时改等级应当失败")
	}
	if got := mustInbound(t, store, in.ID).AccessTierID; got != 2 {
		t.Fatalf("意图没写进去,等级却改成了 %d", got)
	}
	if _, err := db.Exec(`ALTER TABLE apply_queue_gone RENAME TO apply_queue`); err != nil {
		t.Fatal(err)
	}

	// 换 psk。
	snell, err := store.CreateInbound(t.Context(), n.ID, InboundParams{
		DisplayName: "Snell", Protocol: string(singbox.ProtocolSnell), ListenPort: 28443,
		SnellVersion: singbox.SnellVersion6,
	})
	if err != nil {
		t.Fatal(err)
	}
	clear()
	if _, err := store.RotateSnellPSK(t.Context(), snell.ID); err != nil {
		t.Fatal(err)
	}
	if !queuedKind(t, db, n.ID, "SINGBOX") {
		t.Fatal("换 psk 之后队列里没有这台机器")
	}
}

func mustInbound(t *testing.T, s *Store, id int64) *Inbound {
	t.Helper()
	in, err := s.GetInbound(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
