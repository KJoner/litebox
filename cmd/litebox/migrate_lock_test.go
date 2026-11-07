package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
)

// openUnmigrated 给出一个还没迁移过的库与对应的配置,外加一个「另一个主控」用来持锁的连接池。
// SQLite 用自己的文件(运行锁按文件路径取),设了 LITEBOX_TEST_PG_DSN 时用 dbtest 的临时 PG 库
// (advisory lock 按会话算,同一个连接池里另取一条连接就是另一个会话)。
func openUnmigrated(t *testing.T) (config.Config, *sql.DB, *sql.DB) {
	t.Helper()
	var cfg config.Config
	if dbtest.UsingPostgres() {
		cfg.Database.Driver = "postgres"
		db := dbtest.OpenEmpty(t)
		return cfg, db, db
	}
	cfg.Database.Driver = "sqlite"
	cfg.Database.Path = filepath.Join(t.TempDir(), "litebox.db")
	open := func() *sql.DB {
		db, err := database.Open(cfg.Database.Path, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return cfg, open(), open()
}

// R01:旧主控还占着运行锁时,新程序(带着它认识而库里还没有的迁移)一个迁移都不能先跑。
func TestMigrateRefusesWhileAnotherControllerHoldsTheLock(t *testing.T) {
	cfg, db, holderDB := openUnmigrated(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	holder, err := database.AcquireRunLock(ctx, holderDB, cfg.Database.Path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	before, err := database.ReadSchemaStatus(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if before.Pending == 0 || before.Current != 0 || before.Tables != 0 {
		t.Fatalf("前提不对:%+v", before)
	}

	for name, fn := range map[string]func() error{
		"migrate":          func() error { return migrateExclusive(ctx, cfg, db, logger) },
		"reset-password 类": func() error { return ensureSchemaForWrite(ctx, cfg, db, logger) },
	} {
		err := fn()
		if !errors.Is(err, database.ErrRunLockHeld) {
			t.Fatalf("%s:应因运行锁被占而拒绝,得到 %v", name, err)
		}
		after, err := database.ReadSchemaStatus(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Fatalf("%s:被拒绝之前结构已经变了:%+v → %+v", name, before, after)
		}
	}

	// 锁放掉之后照常迁移;再次执行是无操作。
	holder.Release()
	if err := migrateExclusive(ctx, cfg, db, logger); err != nil {
		t.Fatal(err)
	}
	st, _ := database.ReadSchemaStatus(ctx, db)
	if st.Pending != 0 || st.Current != st.Latest {
		t.Fatalf("迁移之后:%+v", st)
	}
	if err := ensureSchemaForWrite(ctx, cfg, db, logger); err != nil {
		t.Fatalf("已是最新时不该再取锁或迁移:%v", err)
	}
}

// R01:库已是最新时,重置密码之类的小命令不取锁 —— 主控开着时照样能用。
func TestWriteCommandsSkipLockWhenSchemaCurrent(t *testing.T) {
	cfg, db, holderDB := openUnmigrated(t)
	ctx := context.Background()
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	holder, err := database.AcquireRunLock(ctx, holderDB, cfg.Database.Path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if err := ensureSchemaForWrite(ctx, cfg, db, nil); err != nil {
		t.Fatalf("主控开着、库已是最新,重置密码应该照常可用:%v", err)
	}
}
