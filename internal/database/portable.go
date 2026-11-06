package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// 两种引擎都认的几个小工具(VNext DB03)。不可移植的写法一律在调用处改成这里的形状,
// 不在驱动层偷偷翻译 —— 读代码的人要能从调用处看出这条语句在两种引擎上都成立。

// RowQuerier 是 *sql.DB、*sql.Tx、*sql.Conn 共有的那一个方法。
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InsertID 执行一条 INSERT 并返回新行的 id:语句末尾加 `RETURNING id`(SQLite ≥ 3.35 与 PG 都支持)。
// 取代 `Result.LastInsertId` —— PG 的驱动不支持它。
//
// 语句用 `ON CONFLICT ... DO NOTHING` 时冲突的那次没有返回行,这里给 (0, nil),
// 与原来「RowsAffected() == 0」那条判断一一对应。
func InsertID(ctx context.Context, q RowQuerier, query string, args ...any) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, strings.TrimRight(query, " \t\r\n;")+" RETURNING id", args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// IsUniqueViolation 回答 err 是不是唯一约束冲突:SQLite 认错误文本,PG 认 SQLSTATE 23505。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "23505"
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsForeignKeyViolation 同上,外键约束(PG 23503)。
func IsForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "23503"
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// Savepoint 在事务 tx 里开一个保存点跑 fn:fn 出错时回到保存点、把错误交回调用方,事务本身还能继续用。
//
// **PG 下事务里任何一条语句出错,整个事务就进入中止状态**,之后每一条都报错、提交变成回滚;
// SQLite 不会。所以「在事务里试着插一行、撞了唯一约束就跳过、接着处理下一行」这种写法,
// 必须把那一条包在保存点里 —— 两种引擎都支持 SAVEPOINT。name 只能是字母、数字与下划线。
func Savepoint(ctx context.Context, tx *sql.Tx, name string, fn func() error) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+name); err != nil {
		return err
	}
	if err := fn(); err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+name); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		tx.ExecContext(ctx, `RELEASE SAVEPOINT `+name)
		return err
	}
	_, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+name)
	return err
}
