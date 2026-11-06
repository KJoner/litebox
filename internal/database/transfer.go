package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// SQLite ↔ PostgreSQL 的离线迁移(VNext DB09–DB11)。
//
// 只做「原样搬」:保留 id、密文、关联关系,不重新加密、不重新发凭据;主密钥跟着部署走。
// 顺序是:目标先由 Migrate 建好同一版本的结构 → 确认目标是空的(只有迁移带进来的初始数据)→
// 按外键依赖逐表复制(每张表一个事务)→ 校正 PG 的自增序列 → 逐表比对行数与逐行哈希、比对有效权限视图。
// 源库只读,任何一步失败都不会动它;目标导入一半失败时它不是「空的」了,重来之前要先删掉重建。

// TableReport 是一张表的行数。
type TableReport struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// InspectReport 是 `litebox db inspect` 的结果。
type InspectReport struct {
	Driver        string        `json:"driver"`
	SchemaVersion int           `json:"schema_version"`
	SizeBytes     int64         `json:"size_bytes"`
	Tables        []TableReport `json:"tables"`
	TotalRows     int64         `json:"total_rows"`
}

// Inspect 读出一个库的版本、表与行数、大约占多少空间。
func Inspect(ctx context.Context, db *sql.DB) (InspectReport, error) {
	r := InspectReport{Driver: "sqlite"}
	if IsPostgres(db) {
		r.Driver = "postgres"
		db.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&r.SizeBytes)
	} else {
		var pages, size int64
		db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages)
		db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size)
		r.SizeBytes = pages * size
	}
	db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&r.SchemaVersion)
	tables, err := userTables(ctx, db)
	if err != nil {
		return r, err
	}
	for _, t := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(t)).Scan(&n); err != nil {
			return r, fmt.Errorf("数表 %s: %w", t, err)
		}
		r.Tables = append(r.Tables, TableReport{Name: t, Rows: n})
		r.TotalRows += n
	}
	return r, nil
}

// seedTables 是迁移本身会插入初始数据的表。导入时目标里这几张表的行先清掉,换成源库的。
var seedTables = map[string]bool{
	"system_settings": true, "access_tiers": true, "probe_sources": true, "boce_state": true,
}

// ErrTargetNotEmpty 表示导入目标里已经有业务数据 —— 工具绝不对一个有数据的库清库。
var ErrTargetNotEmpty = errors.New("目标库不是空的")

// CopyReport 是一次复制的结果。
type CopyReport struct {
	Tables []TableReport `json:"tables"`
	Rows   int64         `json:"rows"`
}

// CopyAll 把 src 的全部业务数据复制到 dst。两边必须已迁移到同一版本;dst 必须是空的。
// progress 在每张表开始时调用(可以为 nil)。
func CopyAll(ctx context.Context, src, dst *sql.DB, progress func(table string, rows int64)) (CopyReport, error) {
	var rep CopyReport
	sv, dv := schemaVersion(ctx, src), schemaVersion(ctx, dst)
	if sv == 0 || sv != dv {
		return rep, fmt.Errorf("两边的结构版本不一致(源 %d,目标 %d):先用同一个版本的程序分别迁移到最新", sv, dv)
	}
	order, err := TableOrder(ctx, src)
	if err != nil {
		return rep, err
	}
	for _, t := range order {
		if seedTables[t] {
			continue
		}
		var n int64
		if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(t)).Scan(&n); err != nil {
			return rep, err
		}
		if n > 0 {
			return rep, fmt.Errorf("%w:表 %s 里已有 %d 行。导入只往一个刚建好的空库里导,不会清库", ErrTargetNotEmpty, t, n)
		}
	}
	// 初始数据表:目标里迁移带进来的那几行换成源库的(源库里它们可能已被管理员改过)。
	// 先按反向依赖顺序删,再按依赖顺序插。
	for i := len(order) - 1; i >= 0; i-- {
		if t := order[i]; seedTables[t] {
			if _, err := dst.ExecContext(ctx, `DELETE FROM `+quoteIdent(t)); err != nil {
				return rep, fmt.Errorf("清空目标的初始数据表 %s: %w", t, err)
			}
		}
	}
	for _, t := range order {
		n, err := copyTable(ctx, src, dst, t, progress)
		if err != nil {
			return rep, fmt.Errorf("复制表 %s: %w", t, err)
		}
		rep.Tables = append(rep.Tables, TableReport{Name: t, Rows: n})
		rep.Rows += n
	}
	if IsPostgres(dst) {
		if err := resetSequences(ctx, dst, order); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

const copyBatch = 500

func copyTable(ctx context.Context, src, dst *sql.DB, table string, progress func(string, int64)) (int64, error) {
	cols, err := listColumns(ctx, src, table)
	if err != nil {
		return 0, err
	}
	if progress != nil {
		var total int64
		src.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(table)).Scan(&total)
		progress(table, total)
	}
	order, err := orderBy(ctx, src, table)
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(quoted, ", ")+` FROM `+quoteIdent(table)+` ORDER BY `+order)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	prefix := `INSERT INTO ` + quoteIdent(table) + ` (` + strings.Join(quoted, ", ") + `) VALUES `
	var (
		batch []any
		count int64
		inBat int
	)
	flush := func() error {
		if inBat == 0 {
			return nil
		}
		q := prefix + strings.TrimSuffix(strings.Repeat(one+",", inBat), ",")
		if _, err := tx.ExecContext(ctx, q, batch...); err != nil {
			return err
		}
		batch, inBat = batch[:0], 0
		return nil
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b) // 库里没有 BLOB 列,文本按字符串搬
			}
		}
		batch = append(batch, vals...)
		inBat++
		count++
		// 一条 INSERT 的参数个数有上限(SQLite 32766、PG 65535),按列数折算批大小。
		if inBat >= copyBatch || len(batch)+len(cols) > 30000 {
			if err := flush(); err != nil {
				return count, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	if err := flush(); err != nil {
		return count, err
	}
	return count, tx.Commit()
}

// resetSequences 把每张有 identity 列的表的序列推到 MAX(id) 之后,导入之后新插的行不会撞号。
func resetSequences(ctx context.Context, db *sql.DB, tables []string) error {
	for _, t := range tables {
		var seq sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT pg_get_serial_sequence(?, 'id')`, t).Scan(&seq); err != nil || !seq.Valid {
			continue // 没有 id 列或不是 identity
		}
		if _, err := db.ExecContext(ctx,
			`SELECT setval(?::regclass, COALESCE((SELECT MAX(id) FROM `+quoteIdent(t)+`), 1), (SELECT COUNT(*) FROM `+quoteIdent(t)+`) > 0)`,
			seq.String); err != nil {
			return fmt.Errorf("校正序列 %s: %w", seq.String, err)
		}
	}
	return nil
}

// VerifyReport 是逐表比对的结果。
type VerifyReport struct {
	Tables   []VerifyTable `json:"tables"`
	Views    []VerifyTable `json:"views"`
	Mismatch int           `json:"mismatch"`
}

// VerifyTable 是一张表(或视图)的比对。
type VerifyTable struct {
	Name      string `json:"name"`
	SrcRows   int64  `json:"src_rows"`
	DstRows   int64  `json:"dst_rows"`
	SrcSHA256 string `json:"src_sha256"`
	DstSHA256 string `json:"dst_sha256"`
	OK        bool   `json:"ok"`
}

// effectiveViews 是权限规则所在的视图:配置生成、订阅、门户都查它们,两边的结果集必须一模一样。
var effectiveViews = []string{
	"user_effective_inbounds", "user_effective_nodes", "user_effective_mieru_inbounds",
	"user_effective_relays", "user_effective_external_proxies",
}

// Verify 逐表比对行数与按主键排序的逐行哈希,再比对有效权限视图的结果集。
// meta 是用来读表结构的那一边(SQLite 那一侧)。
func Verify(ctx context.Context, src, dst *sql.DB) (VerifyReport, error) {
	var rep VerifyReport
	meta := src
	if IsPostgres(src) && !IsPostgres(dst) {
		meta = dst
	}
	tables, err := TableOrder(ctx, meta)
	if err != nil {
		return rep, err
	}
	sort.Strings(tables)
	for _, t := range tables {
		if t == "schema_migrations" {
			continue // 两种引擎的迁移记录本来就不同(PG 从基线起步)
		}
		cols, err := listColumns(ctx, meta, t)
		if err != nil {
			return rep, err
		}
		order, err := orderBy(ctx, meta, t)
		if err != nil {
			return rep, err
		}
		q := `SELECT ` + joinQuoted(cols) + ` FROM ` + quoteIdent(t) + ` ORDER BY ` + order
		v, err := compareQuery(ctx, src, dst, t, q)
		if err != nil {
			return rep, err
		}
		rep.Tables = append(rep.Tables, v)
		if !v.OK {
			rep.Mismatch++
		}
	}
	for _, view := range effectiveViews {
		q := `SELECT * FROM ` + view + ` ORDER BY 1, 2`
		v, err := compareQuery(ctx, src, dst, view, q)
		if err != nil {
			return rep, err
		}
		rep.Views = append(rep.Views, v)
		if !v.OK {
			rep.Mismatch++
		}
	}
	return rep, nil
}

func compareQuery(ctx context.Context, src, dst *sql.DB, name, q string) (VerifyTable, error) {
	v := VerifyTable{Name: name}
	var err error
	if v.SrcRows, v.SrcSHA256, err = hashRows(ctx, src, q); err != nil {
		return v, fmt.Errorf("源库 %s: %w", name, err)
	}
	if v.DstRows, v.DstSHA256, err = hashRows(ctx, dst, q); err != nil {
		return v, fmt.Errorf("目标库 %s: %w", name, err)
	}
	v.OK = v.SrcRows == v.DstRows && v.SrcSHA256 == v.DstSHA256
	return v, nil
}

// hashRows 把查询结果逐行逐列规整成文本后算 sha256:整数十进制、浮点最短表示、NULL 记成 \N。
// 两种引擎对同一个值给出的 Go 类型可能不同(PG 的 SUM 是 numeric 文本),这里比的都是表里的原值。
func hashRows(ctx context.Context, db *sql.DB, q string) (int64, string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	h := sha256.New()
	var n int64
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, "", err
		}
		for i, v := range vals {
			if i > 0 {
				io.WriteString(h, "\x1f")
			}
			io.WriteString(h, canonical(v))
		}
		io.WriteString(h, "\x1e")
		n++
	}
	return n, hex.EncodeToString(h.Sum(nil)), rows.Err()
}

func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return `\N`
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// TableOrder 按外键依赖排好的业务表(被引用的在前)。
func TableOrder(ctx context.Context, db *sql.DB) ([]string, error) {
	tables, err := userTables(ctx, db)
	if err != nil {
		return nil, err
	}
	deps := map[string][]string{}
	for _, t := range tables {
		var q string
		if IsPostgres(db) {
			q = `SELECT DISTINCT ccu.table_name FROM information_schema.table_constraints tc
			       JOIN information_schema.constraint_column_usage ccu ON ccu.constraint_name = tc.constraint_name
			      WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = current_schema() AND tc.table_name = ?`
		} else {
			q = `SELECT DISTINCT "table" FROM pragma_foreign_key_list(?)`
		}
		rows, err := db.QueryContext(ctx, q, t)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d string
			rows.Scan(&d)
			if d != t {
				deps[t] = append(deps[t], d)
			}
		}
		rows.Close()
	}
	var out []string
	state := map[string]int{}
	var visit func(string) error
	visit = func(t string) error {
		switch state[t] {
		case 1:
			return nil
		case 2:
			return fmt.Errorf("表之间的外键成环(%s)", t)
		}
		state[t] = 2
		for _, d := range deps[t] {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[t] = 1
		out = append(out, t)
		return nil
	}
	for _, t := range tables {
		if err := visit(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func userTables(ctx context.Context, db *sql.DB) ([]string, error) {
	var q string
	if IsPostgres(db) {
		q = `SELECT table_name FROM information_schema.tables
		      WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`
	} else {
		q = `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		if t != "schema_migrations" {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func listColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	var q string
	if IsPostgres(db) {
		q = `SELECT column_name FROM information_schema.columns
		      WHERE table_schema = current_schema() AND table_name = ? ORDER BY ordinal_position`
	} else {
		q = `SELECT name FROM pragma_table_info(?) ORDER BY cid`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("表 %s 不存在", table)
	}
	return out, rows.Err()
}

// orderBy 是一张表的稳定排序:主键列;没有主键时按全部列。
func orderBy(ctx context.Context, db *sql.DB, table string) (string, error) {
	var q string
	if IsPostgres(db) {
		q = `SELECT a.attname FROM pg_index i
		       JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		      WHERE i.indrelid = ?::regclass AND i.indisprimary
		      ORDER BY array_position(i.indkey, a.attnum)`
	} else {
		q = `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`
	}
	rows, err := db.QueryContext(ctx, q, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		pk = append(pk, quoteIdent(c))
	}
	if len(pk) == 0 {
		cols, err := listColumns(ctx, db, table)
		if err != nil {
			return "", err
		}
		return joinQuoted(cols), nil
	}
	return strings.Join(pk, ", "), nil
}

func schemaVersion(ctx context.Context, db *sql.DB) int {
	var v int
	db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v
}

func joinQuoted(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c)
	}
	return strings.Join(out, ", ")
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
