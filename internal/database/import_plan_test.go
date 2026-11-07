package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
)

// schemaFingerprint 是一个库的结构快照:迁移版本、有哪些表、各表多少行。
func schemaFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	st, err := database.ReadSchemaStatus(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := database.Inspect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(struct {
		S database.SchemaStatus
		T []database.TableReport
	}{st, rep.Tables})
	return string(b)
}

const insertAdmin = `INSERT INTO admin_users (username, password_hash, created_at, updated_at)
	VALUES (?, 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`

func migratedSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "src.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertAdmin, "a"); err != nil {
		t.Fatal(err)
	}
	return db
}

// R02:import 的预检(也就是默认的演练)对目标库一个字节都不写 —— 空库、已有数据的库、外来库三种。
func TestPlanImportNeverWritesTarget(t *testing.T) {
	ctx := context.Background()
	src := migratedSQLite(t)

	t.Run("空库", func(t *testing.T) {
		dst := dbtest.OpenEmpty(t)
		before := schemaFingerprint(t, dst)
		plan, err := database.PlanImport(ctx, src, dst)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.OK() || !plan.TargetEmpty || plan.MigrateTarget == 0 || plan.Rows == 0 {
			t.Fatalf("空库应可导入、需要建全部结构:%+v", plan)
		}
		if after := schemaFingerprint(t, dst); after != before {
			t.Fatalf("预检改了目标库:\n前 %s\n后 %s", before, after)
		}
	})

	t.Run("已有数据", func(t *testing.T) {
		dst := dbtest.Open(t)
		if _, err := dst.Exec(insertAdmin, "b"); err != nil {
			t.Fatal(err)
		}
		before := schemaFingerprint(t, dst)
		plan, err := database.PlanImport(ctx, src, dst)
		if err != nil {
			t.Fatal(err)
		}
		if plan.OK() {
			t.Fatal("有业务数据的目标库必须被拒绝")
		}
		if after := schemaFingerprint(t, dst); after != before {
			t.Fatal("拒绝之前已经改了目标库")
		}
	})

	t.Run("外来的库", func(t *testing.T) {
		dst := dbtest.OpenEmpty(t)
		if _, err := dst.Exec(`CREATE TABLE other_app (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		before := schemaFingerprint(t, dst)
		plan, err := database.PlanImport(ctx, src, dst)
		if err != nil {
			t.Fatal(err)
		}
		if plan.OK() {
			t.Fatal("不是 LiteBox 迁移出来的库必须被拒绝")
		}
		if after := schemaFingerprint(t, dst); after != before {
			t.Fatal("拒绝之前已经改了目标库")
		}
	})

	t.Run("源库版本落后", func(t *testing.T) {
		old, err := database.Open(filepath.Join(t.TempDir(), "old.db"), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer old.Close()
		if err := database.Migrate(old, nil); err != nil {
			t.Fatal(err)
		}
		st, _ := database.ReadSchemaStatus(ctx, old)
		if _, err := old.Exec(`DELETE FROM schema_migrations WHERE version = ?`, st.Latest); err != nil {
			t.Fatal(err)
		}
		plan, err := database.PlanImport(ctx, old, dbtest.OpenEmpty(t))
		if err != nil {
			t.Fatal(err)
		}
		if plan.OK() {
			t.Fatal("源库没迁移到最新时必须先让人去迁移源库")
		}
	})
}

// R01:ReadSchemaStatus 不建 schema_migrations,对空库是纯读。
func TestReadSchemaStatusIsReadOnly(t *testing.T) {
	db := dbtest.OpenEmpty(t)
	st, err := database.ReadSchemaStatus(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != 0 || st.Pending == 0 || st.Tables != 0 {
		t.Fatalf("%+v", st)
	}
	again, _ := database.ReadSchemaStatus(context.Background(), db)
	if again != st {
		t.Fatalf("两次读取不一致:%+v / %+v", st, again)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n) // 表不存在时报错、n 保持 0
	rep, _ := database.Inspect(context.Background(), db)
	if len(rep.Tables) != 0 {
		t.Fatalf("空库里冒出了表:%+v", rep.Tables)
	}
}
