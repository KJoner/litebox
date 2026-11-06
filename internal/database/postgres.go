package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL 支持(VNext DB01–DB04)。
//
// **一份 SQL 两种引擎都认,而不是两套 SQL。** 79k 行 Go 里有七十多个文件写 SQL,复制一套 PG 版
// 会让两边迟早分叉。所以包一层 database/sql 驱动,只做三件事:
//
//  1. 占位符 `?` 按词法改成 `$1..$n`(跳过字符串、双引号标识符与注释,与 sqlx.Rebind 同一种做法)。
//     **这不是对 SQL 文本做全局替换**:只认占位符这一种记号,其余一个字节不动;
//  2. Go 的 bool 参数转成 0/1 —— 库里的布尔列两边都是整数(SQLite 没有布尔类型,PG 基线照样用 BIGINT);
//  3. **事务在进程内串行**:BeginTx 取一把进程级的锁,提交 / 回滚时释放。SQLite 单写者下「事务里先查后写」
//     天然串行(Boce 预算、续费幂等、端口冲突检测都靠它),PG 的 READ COMMITTED 不保证。主控同一时刻只有
//     一个实例(运行锁保证),进程内的锁就等价于全局串行。事务外的单条语句仍然并发。
//
// 不可移植的写法(LastInsertId、INSERT OR IGNORE、IFNULL 之类)在调用处改成两边都认的写法,不在这里翻译。

// PGOptions 是 PostgreSQL 连接的参数(DB02、DB08)。
type PGOptions struct {
	DSN string
	// MaxOpen / MaxIdle 默认 5 / 2:共享实例上两个应用各自从保守值起步。
	MaxOpen int
	MaxIdle int
	// ConnectTimeout 默认 10 秒;StatementTimeout 默认 60 秒,0 表示不限。
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
}

// OpenPostgres 连接 PostgreSQL。连不上就报错 —— **绝不回落成一个空 SQLite**:那会让管理员以为数据丢了。
func OpenPostgres(opts PGOptions) (*sql.DB, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, errors.New("database.driver 是 postgres,但没有提供连接串(LITEBOX_DB_DSN 或 LITEBOX_DB_DSN_FILE)")
	}
	cfg, err := pgx.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("解析 PostgreSQL 连接串失败(%s): %w", RedactDSN(opts.DSN), err)
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 10 * time.Second
	}
	cfg.ConnectTimeout = opts.ConnectTimeout
	if opts.StatementTimeout == 0 {
		opts.StatementTimeout = 60 * time.Second
	}
	if opts.StatementTimeout > 0 {
		cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	// 时间全部是 RFC3339 文本,时区只影响 PG 自己的 now() 之类 —— 固定 UTC,避免与服务器默认时区纠缠。
	cfg.RuntimeParams["timezone"] = "UTC"
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "litebox"
	}

	db := sql.OpenDB(&pgConnector{base: stdlib.GetConnector(*cfg), txMu: make(chan struct{}, 1), stats: &queryStats{}})
	if opts.MaxOpen <= 0 {
		opts.MaxOpen = 5
	}
	if opts.MaxIdle <= 0 {
		opts.MaxIdle = 2
	}
	db.SetMaxOpenConns(opts.MaxOpen)
	db.SetMaxIdleConns(opts.MaxIdle)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), opts.ConnectTimeout+5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败(%s): %w", RedactDSN(opts.DSN), err)
	}
	return db, nil
}

// IsPostgres 回答这个连接池背后是不是 PostgreSQL。
func IsPostgres(db *sql.DB) bool {
	_, ok := db.Driver().(*pgDriver)
	return ok
}

// QueryCount 返回这个连接池到目前为止执行过的语句条数(基准测试用,VNext PERF01)。
// 拿不到时返回 -1。
func QueryCount(db *sql.DB) int64 {
	switch d := db.Driver().(type) {
	case *pgDriver:
		return d.stats.n.Load()
	case *splitDriver:
		return d.stats.n.Load()
	}
	return -1
}

type queryStats struct{ n atomic.Int64 }

// RedactDSN 去掉连接串里的口令,只留给日志与报错用。
func RedactDSN(dsn string) string {
	if cfg, err := pgx.ParseConfig(dsn); err == nil {
		user := cfg.User
		if user == "" {
			user = "?"
		}
		return fmt.Sprintf("%s@%s:%d/%s", user, cfg.Host, cfg.Port, cfg.Database)
	}
	return "<无法解析的连接串>"
}

type pgDriver struct {
	base  driver.Driver
	stats *queryStats
}

func (d *pgDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("database: 请经 OpenPostgres 打开 PostgreSQL")
}

type pgConnector struct {
	base  driver.Connector
	txMu  chan struct{}
	stats *queryStats
}

func (c *pgConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pgConn{Conn: conn, txMu: c.txMu, stats: c.stats}, nil
}

func (c *pgConnector) Driver() driver.Driver { return &pgDriver{base: c.base.Driver(), stats: c.stats} }

type pgConn struct {
	driver.Conn
	txMu  chan struct{}
	stats *queryStats
}

var (
	_ driver.ConnBeginTx        = (*pgConn)(nil)
	_ driver.ExecerContext      = (*pgConn)(nil)
	_ driver.QueryerContext     = (*pgConn)(nil)
	_ driver.ConnPrepareContext = (*pgConn)(nil)
	_ driver.NamedValueChecker  = (*pgConn)(nil)
	_ driver.SessionResetter    = (*pgConn)(nil)
	_ driver.Validator          = (*pgConn)(nil)
	_ driver.Pinger             = (*pgConn)(nil)
)

func (c *pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *pgConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.stats.n.Add(1)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, Rebind(query))
}

func (c *pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.stats.n.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, Rebind(query), args)
}

func (c *pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.stats.n.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, Rebind(query), args)
}

// CheckNamedValue 把 bool 换成 0/1,其余交给 pgx 自己的转换。
func (c *pgConn) CheckNamedValue(nv *driver.NamedValue) error {
	if b, ok := nv.Value.(bool); ok {
		if b {
			nv.Value = int64(1)
		} else {
			nv.Value = int64(0)
		}
		return nil
	}
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (c *pgConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	select {
	case c.txMu <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		<-c.txMu
		return nil, err
	}
	return &pgTx{Tx: tx, release: func() { <-c.txMu }}, nil
}

type pgTx struct {
	driver.Tx
	release func()
}

func (t *pgTx) Commit() error {
	defer t.release()
	return t.Tx.Commit()
}

func (t *pgTx) Rollback() error {
	defer t.release()
	return t.Tx.Rollback()
}

func (c *pgConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *pgConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *pgConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// Rebind 把 `?` 占位符按出现顺序改成 `$1..$n`。字符串字面量、双引号标识符、`--` 与 `/* */` 注释、
// 以及 PG 的美元引号字符串里的 `?` 一律不动。没有 `?` 时原样返回。
func Rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); {
		ch := q[i]
		switch {
		case ch == '\'' || ch == '"':
			j := i + 1
			for j < len(q) {
				if q[j] == ch {
					if j+1 < len(q) && q[j+1] == ch { // '' 或 "" 是转义
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(q))
			b.WriteString(q[i:end])
			i = end
		case ch == '-' && i+1 < len(q) && q[i+1] == '-':
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				b.WriteString(q[i:])
				return b.String()
			}
			b.WriteString(q[i : i+j+1])
			i += j + 1
		case ch == '/' && i+1 < len(q) && q[i+1] == '*':
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				b.WriteString(q[i:])
				return b.String()
			}
			b.WriteString(q[i : i+2+j+2])
			i += 2 + j + 2
		case ch == '$':
			// 美元引号 $tag$...$tag$(tag 可以为空)。$1 这类已经是 PG 占位符的原样留着。
			if tag, ok := dollarTag(q[i:]); ok {
				j := strings.Index(q[i+len(tag):], tag)
				if j < 0 {
					b.WriteString(q[i:])
					return b.String()
				}
				end := i + len(tag) + j + len(tag)
				b.WriteString(q[i:end])
				i = end
				continue
			}
			b.WriteByte(ch)
			i++
		case ch == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

func dollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (j > 1 && c >= '0' && c <= '9')) {
			return "", false
		}
	}
	return "", false
}
