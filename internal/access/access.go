// Package access 管理节点访问等级,并提供"用户能用哪些节点"的唯一解析入口。
//
// 两个概念必须分开:
//   - 管理角色(admin_users)决定能否进后台;
//   - 访问等级(access_tiers)决定代理用户能用哪些节点。
//
// ROOT 只是"能用到全部节点"的代理用户等级,不带任何后台权限。
//
// 有效节点的规则本身只写在数据库视图 user_effective_nodes 里(见迁移 0007),
// 本包与其他包都只查它,不再各自拼等级条件 —— 配置生成、订阅、门户与
// 部署脏标记一旦对"哪些节点归这个用户"给出不同答案,表现是用户在订阅里
// 看得到、连上去却没有凭据,而且全链路不报任何错。
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EffectiveNodesView 是有效节点视图名。需要在自己的查询里做 JOIN 的包
// (配置生成、订阅)引用这个常量,不要另写等级判断。
const EffectiveNodesView = "user_effective_nodes"

// EffectiveInboundsView 是有效入站视图名(迁移 0019)。
//
// 它建立在 EffectiveNodesView 之上再按入站等级收一次 —— 两层是交集:
// 能用这台机器,才谈得上能用它的哪个入口。
//
// **配置生成与订阅一律查它,不查节点那一层。** 只查节点的话,
// 一台机器上的 VIP 入口会把普通用户的凭据也写进去,那是权限凭空放大,
// 而且不报任何错。
const EffectiveInboundsView = "user_effective_inbounds"

// EffectiveRelaysView 是中转线路的有效性视图名(迁移 0018)。
//
// 它的规则比节点多一层:线路可见 ⟹ 用户在【落地】上确实有凭据。
// 透传模式下客户端出示的是落地的凭据,少了这一层的表现是用户在订阅里
// 看得见这条线路、连上去握手直接被拒 —— 而且跨了两台机器,
// 排查的人会先去查中转主机。
const EffectiveRelaysView = "user_effective_relays"

// 三个内置等级的 code。程序内一律用 code 判断,不要用 name —— name 可改。
const (
	CodeNormal = "normal"
	CodeVIP    = "vip"
	CodeRoot   = "root"
)

// TierNormalID 是普通组的固定主键(迁移 0007 写死为 1)。
// 新建用户与新建节点未指定等级时落到它。
const TierNormalID int64 = 1

var ErrTierNotFound = errors.New("访问等级不存在")

// Tier 是一个访问等级。
type Tier struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	// Level 决定继承关系:节点 Level <= 用户 Level 即为可用。
	Level       int    `json:"level"`
	Description string `json:"description"`
	SortOrder   int    `json:"sort_order"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Queryer 让同一份解析实现既能走连接也能走事务。
// 额度检查在事务里判断受影响节点,不能因为拿不到 *sql.DB 就复制一份规则。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Store 读写访问等级。V2 不支持新增与删除等级,只能改展示名称、说明与排序。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const tierColumns = `id, code, name, level, description, sort_order, created_at, updated_at`

func scanTier(scan func(dest ...any) error) (*Tier, error) {
	var t Tier
	err := scan(&t.ID, &t.Code, &t.Name, &t.Level, &t.Description,
		&t.SortOrder, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) List(ctx context.Context) ([]*Tier, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tierColumns+` FROM access_tiers ORDER BY sort_order, level`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tiers := make([]*Tier, 0, 3)
	for rows.Next() {
		t, err := scanTier(rows.Scan)
		if err != nil {
			return nil, err
		}
		tiers = append(tiers, t)
	}
	return tiers, rows.Err()
}

func (s *Store) Get(ctx context.Context, id int64) (*Tier, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+tierColumns+` FROM access_tiers WHERE id = ?`, id)
	t, err := scanTier(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTierNotFound
	}
	return t, err
}

func (s *Store) GetByCode(ctx context.Context, code string) (*Tier, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+tierColumns+` FROM access_tiers WHERE code = ?`, code)
	t, err := scanTier(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTierNotFound
	}
	return t, err
}

// UpdateParams 是可修改的等级字段。code 与 level 不可改:
// 前者是程序内的引用标识,后者决定继承关系,改了会让存量用户
// 的可用节点集合在管理员毫无察觉的情况下整体变化。
type UpdateParams struct {
	Name        string
	Description string
	SortOrder   int
}

func (s *Store) Update(ctx context.Context, id int64, p UpdateParams) (*Tier, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return nil, errors.New("等级名称不能为空")
	}
	if len([]rune(p.Name)) > 32 {
		return nil, errors.New("等级名称不能超过 32 个字符")
	}
	if len([]rune(p.Description)) > 128 {
		return nil, errors.New("等级说明不能超过 128 个字符")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE access_tiers SET name = ?, description = ?, sort_order = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.Description, p.SortOrder, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrTierNotFound
	}
	return s.Get(ctx, id)
}

// Validate 确认等级存在,供创建/编辑用户与节点时校验入参。
// 迁移里没写外键(SQLite 的 ADD COLUMN 限制),这道校验就是唯一的拦截点。
func (s *Store) Validate(ctx context.Context, id int64) error {
	return ValidateTier(ctx, s.db, id)
}

// RowQueryer 让同一份等级校验既能走连接也能走事务。
//
// **必须有事务版本**:数据库连接池上限是 1(见 database.Open),
// 在一个打开的事务里拿 *sql.DB 再查一次会直接死锁 —— 而那是节点连同
// 第一个入站一起创建时必然走到的路径。
type RowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ValidateTier 确认等级存在。
//
// 迁移里没给 access_tier_id 写外键(SQLite 的 ADD COLUMN 在 foreign_keys
// 打开时要求带外键的新列默认值必须是 NULL),这道校验是唯一的拦截点 ——
// 指向不存在的等级会让那一行从有效视图里整个消失(INNER JOIN),
// 表现为"存在但谁都用不到"。
func ValidateTier(ctx context.Context, q RowQueryer, id int64) error {
	var count int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM access_tiers WHERE id = ?`, id).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: id=%d", ErrTierNotFound, id)
	}
	return nil
}

// NodesForUser 返回该用户当前可用的全部节点 ID(等级继承 + 额外授权)。
func NodesForUser(ctx context.Context, q Queryer, userID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT node_id FROM `+EffectiveNodesView+` WHERE proxy_user_id = ? ORDER BY node_id`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// UsersForNode 返回该节点上应当存在配置的用户 ID。
// 只解析归属关系,不判断用户是否可服务(停用/过期/超额由调用方过滤)。
func UsersForNode(ctx context.Context, q Queryer, nodeID int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT proxy_user_id FROM `+EffectiveNodesView+` WHERE node_id = ? ORDER BY proxy_user_id`,
		nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// NodesByUser 一次查出全部用户的有效节点,供列表接口使用,
// 避免每个用户一次查询。
func NodesByUser(ctx context.Context, q Queryer) (map[int64][]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT proxy_user_id, node_id FROM `+EffectiveNodesView+
			` ORDER BY proxy_user_id, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]int64)
	for rows.Next() {
		var userID, nodeID int64
		if err := rows.Scan(&userID, &nodeID); err != nil {
			return nil, err
		}
		result[userID] = append(result[userID], nodeID)
	}
	return result, rows.Err()
}

// RelaysForUser 返回该用户当前可见的全部中转线路 ID。
func RelaysForUser(ctx context.Context, q Queryer, userID int64) ([]int64, error) {
	return scanIDs(ctx, q,
		`SELECT relay_id FROM `+EffectiveRelaysView+` WHERE proxy_user_id = ? ORDER BY relay_id`,
		userID)
}

// UsersForRelay 返回能看到这条线路的用户 ID。
func UsersForRelay(ctx context.Context, q Queryer, relayID int64) ([]int64, error) {
	return scanIDs(ctx, q,
		`SELECT proxy_user_id FROM `+EffectiveRelaysView+` WHERE relay_id = ? ORDER BY proxy_user_id`,
		relayID)
}

func scanIDs(ctx context.Context, q Queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
