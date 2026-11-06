package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
)

// cmdDB 是 `litebox db <inspect|import|verify|export>`(VNext DB09–DB11)。
//
// 全部是离线操作:导入与导出之前必须停掉主控(工具会去取运行锁,取不到就拒绝)。
// 源库只读;目标必须是刚建好的空库(只有迁移带进来的初始数据),绝不对一个有数据的库清库。
// **import 默认只演练**,不加 --execute 一个字节都不写。
func cmdDB(args []string) error {
	if len(args) == 0 {
		return errors.New("用法:litebox db <inspect|import|verify|export> [参数],详见 docs/vnext/migration.md")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("db "+sub, flag.ExitOnError)
	configPath := fs.String("config", "litebox.yaml", "配置文件路径(目标 / 源 PG 取自其中的 database 一节)")
	fromSQLite := fs.String("from-sqlite", "", "源 SQLite 文件(建议用 litebox backup 做出来的快照)")
	toSQLite := fs.String("to-sqlite", "", "导出目标:一个还不存在的 SQLite 文件路径")
	execute := fs.Bool("execute", false, "import:真的写入目标库(不加时只演练)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch sub {
	case "inspect":
		db, err := openForTool(cfg.Database, *fromSQLite)
		if err != nil {
			return err
		}
		defer db.Close()
		return printInspect("库", db)

	case "import":
		if *fromSQLite == "" {
			return errors.New("import 需要 --from-sqlite <源 SQLite 文件>")
		}
		if !cfg.Database.IsPostgres() {
			return errors.New("import 的目标取自配置的 database 一节,而它现在是 sqlite —— 先把 driver 改成 postgres 并给出连接串")
		}
		src, unlockSrc, err := openSourceSQLite(*fromSQLite)
		if err != nil {
			return err
		}
		defer src.Close()
		defer unlockSrc()
		dst, err := openDatabase(cfg.Database)
		if err != nil {
			return err
		}
		defer dst.Close()
		lock, err := database.AcquireRunLock(ctx, dst, "", nil, nil)
		if err != nil {
			return fmt.Errorf("目标库上有主控在跑,先停掉它:%w", err)
		}
		defer lock.Release()
		if err := database.Migrate(dst, nil); err != nil {
			return fmt.Errorf("准备目标结构: %w", err)
		}
		if err := printInspect("源(SQLite)", src); err != nil {
			return err
		}
		if err := printInspect("目标(PostgreSQL)", dst); err != nil {
			return err
		}
		if !*execute {
			fmt.Println("\n这是演练:没有写入任何东西。确认无误后加 --execute 再执行一次。")
			return nil
		}
		start := time.Now()
		rep, err := database.CopyAll(ctx, src, dst, func(table string, rows int64) {
			fmt.Printf("  复制 %-34s %d 行\n", table, rows)
		})
		if err != nil {
			return fmt.Errorf("导入失败(源库没有被改动;目标库已不是空的,重来之前请删掉重建):%w", err)
		}
		fmt.Printf("\n已复制 %d 张表、%d 行,用时 %s。开始校验……\n", len(rep.Tables), rep.Rows, time.Since(start).Round(time.Second))
		return verifyAndReport(ctx, cfg, src, dst)

	case "verify":
		if *fromSQLite == "" {
			return errors.New("verify 需要 --from-sqlite <源 SQLite 文件>")
		}
		src, unlockSrc, err := openSourceSQLite(*fromSQLite)
		if err != nil {
			return err
		}
		defer src.Close()
		defer unlockSrc()
		dst, err := openDatabase(cfg.Database)
		if err != nil {
			return err
		}
		defer dst.Close()
		return verifyAndReport(ctx, cfg, src, dst)

	case "export":
		if *toSQLite == "" {
			return errors.New("export 需要 --to-sqlite <还不存在的文件路径>")
		}
		if !cfg.Database.IsPostgres() {
			return errors.New("export 的源取自配置的 database 一节,而它现在是 sqlite;SQLite 之间请用 litebox backup")
		}
		if _, err := os.Stat(*toSQLite); err == nil {
			return fmt.Errorf("%s 已经存在 —— 导出只写一个新文件,不覆盖", *toSQLite)
		}
		src, err := openDatabase(cfg.Database)
		if err != nil {
			return err
		}
		defer src.Close()
		lock, err := database.AcquireRunLock(ctx, src, "", nil, nil)
		if err != nil {
			return fmt.Errorf("源库上有主控在跑,先停掉它(否则导出的不是一个一致的时间点):%w", err)
		}
		defer lock.Release()
		dst, err := database.Open(*toSQLite, 5*time.Second)
		if err != nil {
			return err
		}
		defer dst.Close()
		if err := database.Migrate(dst, nil); err != nil {
			return err
		}
		rep, err := database.CopyAll(ctx, src, dst, func(table string, rows int64) {
			fmt.Printf("  复制 %-34s %d 行\n", table, rows)
		})
		if err != nil {
			return fmt.Errorf("导出失败(PG 没有被改动;删掉 %s 重来):%w", *toSQLite, err)
		}
		fmt.Printf("\n已导出 %d 行到 %s。开始校验……\n", rep.Rows, *toSQLite)
		return verifyAndReport(ctx, cfg, src, dst)

	default:
		return fmt.Errorf("未知的 db 子命令 %q(可用:inspect import verify export)", sub)
	}
}

// openForTool:给了 --from-sqlite 就开那个文件,否则开配置里的库。
func openForTool(cfg config.DatabaseConfig, sqlitePath string) (*sql.DB, error) {
	if sqlitePath != "" {
		db, _, err := openSourceSQLite(sqlitePath)
		return db, err
	}
	return openDatabase(cfg)
}

// openSourceSQLite 打开源 SQLite 并取它的运行锁(取不到说明主控正开着它)。不迁移、不改它。
func openSourceSQLite(path string) (*sql.DB, func(), error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("源文件 %s: %w", path, err)
	}
	db, err := database.OpenSQLite(path, 5*time.Second, 0)
	if err != nil {
		return nil, nil, err
	}
	lock, err := database.AcquireRunLock(context.Background(), db, path, nil, nil)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("主控正在用 %s,先停掉它或者对 litebox backup 做出来的快照操作:%w", path, err)
	}
	return db, lock.Release, nil
}

func printInspect(title string, db *sql.DB) error {
	r, err := database.Inspect(context.Background(), db)
	if err != nil {
		return err
	}
	fmt.Printf("\n%s:%s,结构版本 %d,约 %.1f MB,%d 张表、共 %d 行\n",
		title, r.Driver, r.SchemaVersion, float64(r.SizeBytes)/(1<<20), len(r.Tables), r.TotalRows)
	for _, t := range r.Tables {
		if t.Rows > 0 {
			fmt.Printf("  %-34s %d\n", t.Name, t.Rows)
		}
	}
	return nil
}

func verifyAndReport(ctx context.Context, cfg config.Config, src, dst *sql.DB) error {
	rep, err := database.Verify(ctx, src, dst)
	if err != nil {
		return err
	}
	for _, t := range append(rep.Tables, rep.Views...) {
		if !t.OK {
			fmt.Printf("  ✗ %-34s 源 %d 行 / 目标 %d 行(哈希 %.12s / %.12s)\n", t.Name, t.SrcRows, t.DstRows, t.SrcSHA256, t.DstSHA256)
		}
	}
	// 密文还能用配置里的主密钥解开:导入不重新加密,主密钥必须跟着走。
	if cipher, err := crypto.NewCipher(cfg.Security.MasterKey); err == nil {
		ok, bad := decryptSample(ctx, dst, cipher)
		fmt.Printf("密文抽查:用当前主密钥解开 %d 个,失败 %d 个\n", ok, bad)
		if bad > 0 {
			rep.Mismatch++
		}
	}
	if rep.Mismatch > 0 {
		return fmt.Errorf("校验不通过:%d 处不一致(源库没有被改动)", rep.Mismatch)
	}
	fmt.Printf("校验通过:%d 张表逐行一致,%d 个有效权限视图结果一致。\n", len(rep.Tables), len(rep.Views))
	return nil
}

// decryptSample 解开目标库里用户 UUID 与 REALITY 私钥的密文。
func decryptSample(ctx context.Context, db *sql.DB, cipher *crypto.Cipher) (int, int) {
	var ok, bad int
	for _, q := range []string{
		`SELECT uuid_encrypted FROM proxy_users WHERE uuid_encrypted != ''`,
		`SELECT reality_privkey_encrypted FROM node_inbounds WHERE reality_privkey_encrypted != ''`,
	} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		for rows.Next() {
			var enc string
			rows.Scan(&enc)
			if _, err := cipher.Decrypt(enc); err != nil {
				bad++
			} else {
				ok++
			}
		}
		rows.Close()
	}
	return ok, bad
}
