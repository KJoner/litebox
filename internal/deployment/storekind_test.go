package deployment

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

func kindTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "deploy.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO nodes (id, name, host, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, created_at, updated_at)
		VALUES (1,'n1','127.0.0.1',443,'www.apple.com','e','p','abcd','ONLINE','t','t')`,
	); err != nil {
		t.Fatal(err)
	}
	return db
}

// 每一种下发都必须存得进 deployments。
//
// **这是 V13 在生产上撞到的**:迁移 0018 那条 CHECK 只写了
// SINGBOX 与 RELAY,而 Mieru 是第三种。于是每一次 Mieru 下发都落不了库:
//
//	CHECK constraint failed: kind IN ('SINGBOX', 'RELAY')
//
// 下发本身照常跑完,只有记录没了 —— 部署记录页上一条 Mieru 的记录都没有,
// 管理员只能去翻 journalctl,而那里按设计只写失败步骤名与错误的第一行。
//
// 遍历的是 Kind 的**全部**取值,所以以后再加一种下发(而忘了改迁移)
// 会在这里失败,而不是在生产上失败。
func TestEveryDeployKindCanBeSaved(t *testing.T) {
	db := kindTestDB(t)
	store := NewStore(db)

	for _, kind := range []Kind{KindSingBox, KindRelay, KindMieru} {
		id, err := store.Save(t.Context(), Result{
			NodeID:       1,
			Kind:         kind,
			Revision:     1,
			ConfigSHA256: "deadbeef",
			Status:       StatusSuccess,
			StartedAt:    time.Now().UTC(),
			FinishedAt:   time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("种类 %s 存不进部署记录:%v", kind, err)
		}
		var got string
		if err := db.QueryRow(
			`SELECT kind FROM deployments WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != string(kind) {
			t.Errorf("存进去的种类 = %q,期望 %q", got, kind)
		}
	}
}
