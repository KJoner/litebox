package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"unicode"
)

// 读写分离的 SQLite 连接(VNext PERF04)。
//
// SQLite 是单写者模型,原来整个面板只有一条连接:读也排在写后面,仪表盘一次首屏
// 十几个并发读请求逐个执行,5 个会话同时打开时整屏 p95 到了 1.7 秒(50 台主机档,
// 见 docs/vnext/acceptance.md)。WAL 允许多个读连接与一个写连接并存,于是:
//
//   - **写与事务仍然只走一条写连接**,由 writeMu 串行 —— 与原来单连接的语义完全相同:
//     事务里「先查后写」照样是串行的(Boce 预算、续费幂等、端口冲突检测都靠它);
//   - **事务外的 SELECT 走只读连接**(每条池连接私有一条,`query_only` 打开)。
//     一条写语句误判成读也不会悄悄写进去:只读连接会当场报 "attempt to write a readonly database"。
//
// 读到的永远是已提交的数据:写在自动提交里结束之后才会有下一条读,读连接新开的读事务看得见它;
// 一个开着的写事务没提交之前,读连接看到的是它开始之前的快照 —— 与「这条读早发生了一点」等价。
// 原来「事务里再用 db 查」会单连接自锁(所以代码里没有这种写法),现在它能跑通;
// 「事务里再用 db 写」仍然会在 writeMu 上自锁,与原来一样。
//
// database/sql 看到的是普通的连接池(池子大小 = 读连接数),上层一行代码不用改。
type splitConnector struct {
	drv       driver.Driver
	writerDSN string
	readerDSN string

	// writeMu 是容量为 1 的信号量而不是 sync.Mutex:等锁要能被 ctx 取消。
	writeMu chan struct{}

	mu     sync.Mutex
	writer driver.Conn
	stats  queryStats
}

func newSplitConnector(drv driver.Driver, writerDSN, readerDSN string) *splitConnector {
	return &splitConnector{drv: drv, writerDSN: writerDSN, readerDSN: readerDSN, writeMu: make(chan struct{}, 1)}
}

func (c *splitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	reader, err := c.drv.Open(c.readerDSN)
	if err != nil {
		return nil, err
	}
	return &splitConn{c: c, reader: reader}, nil
}

func (c *splitConnector) Driver() driver.Driver { return &splitDriver{Driver: c.drv, stats: &c.stats} }

// splitDriver 只是给 db.Driver() 一个能认出来的类型(QueryCount 要用),开连接仍走 Connector。
type splitDriver struct {
	driver.Driver
	stats *queryStats
}

// Close 由 sql.DB.Close 调用。
func (c *splitConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer == nil {
		return nil
	}
	err := c.writer.Close()
	c.writer = nil
	return err
}

// lockWriter 取得写连接。返回的 unlock 必须且只能调用一次。
func (c *splitConnector) lockWriter(ctx context.Context) (driver.Conn, func(), error) {
	select {
	case c.writeMu <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	unlock := func() { <-c.writeMu }
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writer == nil {
		w, err := c.drv.Open(c.writerDSN)
		if err != nil {
			unlock()
			return nil, nil, err
		}
		c.writer = w
	}
	return c.writer, unlock, nil
}

type splitConn struct {
	c      *splitConnector
	reader driver.Conn

	// 事务进行中时,这条池连接上的一切都走写连接(锁在 BeginTx 时取得,提交 / 回滚时释放)。
	tx     driver.Tx
	writer driver.Conn
	unlock func()
}

var (
	_ driver.ConnBeginTx        = (*splitConn)(nil)
	_ driver.ExecerContext      = (*splitConn)(nil)
	_ driver.QueryerContext     = (*splitConn)(nil)
	_ driver.ConnPrepareContext = (*splitConn)(nil)
	_ driver.SessionResetter    = (*splitConn)(nil)
	_ driver.Validator          = (*splitConn)(nil)
	_ driver.Pinger             = (*splitConn)(nil)
)

func (s *splitConn) Prepare(query string) (driver.Stmt, error) {
	return s.PrepareContext(context.Background(), query)
}

func (s *splitConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s.c.stats.n.Add(1)
	if s.tx != nil {
		return s.writer.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	}
	if isReadOnlyQuery(query) {
		return s.reader.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	}
	return &lockedStmt{c: s.c, query: query}, nil
}

func (s *splitConn) Close() error { return s.reader.Close() }

func (s *splitConn) Begin() (driver.Tx, error) {
	return s.BeginTx(context.Background(), driver.TxOptions{})
}

func (s *splitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if s.tx != nil {
		return nil, errors.New("database: 同一条连接上已有进行中的事务")
	}
	w, unlock, err := s.c.lockWriter(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := w.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		unlock()
		return nil, err
	}
	s.tx, s.writer, s.unlock = tx, w, unlock
	return &splitTx{s: s}, nil
}

type splitTx struct{ s *splitConn }

func (t *splitTx) Commit() error   { return t.end(t.s.tx.Commit) }
func (t *splitTx) Rollback() error { return t.end(t.s.tx.Rollback) }

func (t *splitTx) end(fn func() error) error {
	err := fn()
	unlock := t.s.unlock
	t.s.tx, t.s.writer, t.s.unlock = nil, nil, nil
	unlock()
	return err
}

func (s *splitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	s.c.stats.n.Add(1)
	if s.tx != nil {
		return s.writer.(driver.ExecerContext).ExecContext(ctx, query, args)
	}
	w, unlock, err := s.c.lockWriter(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return w.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (s *splitConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s.c.stats.n.Add(1)
	if s.tx != nil {
		return s.writer.(driver.QueryerContext).QueryContext(ctx, query, args)
	}
	if isReadOnlyQuery(query) {
		return s.reader.(driver.QueryerContext).QueryContext(ctx, query, args)
	}
	// INSERT ... RETURNING 这类「返回行的写」:锁要拿到 Rows 关闭为止。
	w, unlock, err := s.c.lockWriter(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := w.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		unlock()
		return nil, err
	}
	return &unlockRows{Rows: rows, unlock: unlock}, nil
}

func (s *splitConn) ResetSession(ctx context.Context) error {
	if r, ok := s.reader.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (s *splitConn) IsValid() bool {
	if v, ok := s.reader.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (s *splitConn) Ping(ctx context.Context) error {
	if p, ok := s.reader.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// lockedStmt 是在写连接上执行的预编译语句:每次执行时取锁、在写连接上编译、执行、释放。
// 写连接不能被两个 goroutine 同时用,所以语句不能脱离锁长期挂在它上面。
type lockedStmt struct {
	c     *splitConnector
	query string
}

func (l *lockedStmt) Close() error  { return nil }
func (l *lockedStmt) NumInput() int { return -1 }

func (l *lockedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return l.ExecContext(context.Background(), valuesToNamed(args))
}

func (l *lockedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return l.QueryContext(context.Background(), valuesToNamed(args))
}

func (l *lockedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	w, unlock, err := l.c.lockWriter(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return w.(driver.ExecerContext).ExecContext(ctx, l.query, args)
}

func (l *lockedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	w, unlock, err := l.c.lockWriter(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := w.(driver.QueryerContext).QueryContext(ctx, l.query, args)
	if err != nil {
		unlock()
		return nil, err
	}
	return &unlockRows{Rows: rows, unlock: unlock}, nil
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

type unlockRows struct {
	driver.Rows
	once   sync.Once
	unlock func()
}

func (r *unlockRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(r.unlock)
	return err
}

// isReadOnlyQuery 回答这条语句能不能交给只读连接:跳过开头的空白与注释,第一个关键字是
// SELECT、WITH 或 EXPLAIN。判错的代价是不对称的:把读当成写只是少一点并发;
// 把写当成读会被只读连接当场拒绝(query_only),不会悄悄写进去。
func isReadOnlyQuery(q string) bool {
	q = skipSpaceAndComments(q)
	end := strings.IndexFunc(q, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(q)
	}
	switch strings.ToUpper(q[:end]) {
	case "SELECT", "WITH", "EXPLAIN":
		return true
	}
	return false
}

func skipSpaceAndComments(q string) string {
	for {
		q = strings.TrimLeftFunc(q, unicode.IsSpace)
		switch {
		case strings.HasPrefix(q, "--"):
			if i := strings.IndexByte(q, '\n'); i >= 0 {
				q = q[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(q, "/*"):
			if i := strings.Index(q, "*/"); i >= 0 {
				q = q[i+2:]
			} else {
				return ""
			}
		default:
			return q
		}
	}
}
