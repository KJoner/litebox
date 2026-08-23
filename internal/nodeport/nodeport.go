// Package nodeport 是「这台机器上的端口有没有被占」的**唯一**实现。
//
// 一台机器上现在有四类东西在抢端口:
//
//	node_inbounds.listen_port        sing-box 入站,单值
//	node_relays.listen_port          nginx 转发规则,单值
//	node_mieru_inbounds.listen_*     Mieru 入口,**一整段**
//	nodes.api_port                   V2Ray API 的回环端口,单值
//
// 在这个包出现之前它已经有两份实现(node 与 relay 各一份),而 Mieru 带来的
// 是第三份 —— 更糟的是,Mieru 的端口是一段区间,所以另外两份都必须跟着改:
// 新建一个 sing-box 入站时要问"它有没有落进某个 Mieru 段里"。
// 三份实现里漏改任何一处,表现都是同一个:**其中一个服务 bind 失败、整个
// 起不来,而问题要到部署的健康检查才暴露,那时配置已经换过去了**。
//
// 所以三类调用点共用这一个函数。它把单值当成 Start = End 的一段来处理,
// 于是"区间对区间"是唯一需要写对的逻辑,而不是三种两两组合。
//
// 为什么单独一个包:node 已经 import relay(relaydeploy 要读转发规则),
// 所以 relay 不能反过来 import node。两边都能 import 的地方只能是第三个包。
package nodeport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/litebox/litebox/internal/mieru"
)

// ErrConflict 是端口冲突。
//
// **检测到就拒绝保存,不自动挪端口。** 自动避让会让用户手上那份订阅静默失效:
// 客户端还连着旧端口,而那里已经没人监听了。node 与 relay 两个包的
// ErrInboundPortConflict / ErrPortConflict 都指向它,所以上层
// errors.Is 的写法一个字都不用改。
var ErrConflict = errors.New("监听端口冲突")

// Kind 是端口占用者的种类,用于「排除自己」。
type Kind string

const (
	KindInbound Kind = "INBOUND"
	KindRelay   Kind = "RELAY"
	KindMieru   Kind = "MIERU"
)

// Skip 指明这次检查要放过哪一行 —— 编辑一条已有记录时,它自己的端口
// 当然会跟自己撞。
//
// **种类与 id 必须一起给。** 只给 id 的话,一个 id 会在三张表里各匹配一行,
// 而那意味着编辑 3 号入站时会顺带放过 3 号转发规则与 3 号 Mieru 入口 ——
// 于是一次真实的冲突被静默放行。零值表示"新建,谁都不放过"。
type Skip struct {
	Kind Kind
	ID   int64
}

func (s Skip) idFor(k Kind) int64 {
	if s.Kind == k && s.ID > 0 {
		return s.ID
	}
	// 0 不会匹配任何自增主键,所以"不排除"用它表达。
	return 0
}

// Queryer 同时被 *sql.DB 与 *sql.Tx 满足 —— 创建入站是在事务里做的,
// 而检查必须与插入在同一个事务里,否则两次并发创建会双双通过。
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Free 确认 rng 这一段端口在这台机器上没有被占用。
//
// rng 是单端口时传 Start = End 的一段。空段(两端都是 0)直接放行 ——
// 那表示"跟随",还没有落到具体号码上。
func Free(ctx context.Context, q Queryer, nodeID int64, rng mieru.PortRange, skip Skip) error {
	if rng.Empty() {
		return nil
	}
	if err := rng.Validate("监听端口"); err != nil {
		return err
	}

	var role string
	var apiPort int
	err := q.QueryRowContext(ctx,
		`SELECT role, api_port FROM nodes WHERE id = ? AND deleted_at IS NULL`,
		nodeID).Scan(&role, &apiPort)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("节点不存在: id=%d", nodeID)
	}
	if err != nil {
		return err
	}

	// 中转角色的机器上没有 sing-box,也就没有 API 端口与入站 ——
	// 那一列在 RELAY 上保持 0,拿它去比会把 0 端口的段误判成冲突。
	if role != "RELAY" {
		if rng.Contains(apiPort) {
			return conflict(rng, apiPort, "这台机器上 V2Ray API 的端口")
		}
		if err := single(ctx, q, `node_inbounds`, nodeID, rng,
			skip.idFor(KindInbound), "sing-box 入站的监听端口"); err != nil {
			return err
		}
	}
	if err := single(ctx, q, `node_relays`, nodeID, rng,
		skip.idFor(KindRelay), "nginx 转发规则的监听端口"); err != nil {
		return err
	}
	return mieruRange(ctx, q, nodeID, rng, skip.idFor(KindMieru))
}

// single 查一张【单值】端口表:有没有哪一行的端口落进 rng 里。
//
// 表名是拼进 SQL 的,所以只能由本包内的常量调用 —— 它永远不来自外部输入。
func single(
	ctx context.Context, q Queryer, table string,
	nodeID int64, rng mieru.PortRange, skipID int64, what string,
) error {
	var port int
	err := q.QueryRowContext(ctx,
		`SELECT listen_port FROM `+table+`
		  WHERE node_id = ? AND deleted_at IS NULL AND id != ?
		    AND listen_port BETWEEN ? AND ?
		  LIMIT 1`,
		nodeID, skipID, rng.Start, rng.End).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return conflict(rng, port, what)
}

// mieruRange 查 Mieru 入口:有没有哪一段与 rng 相交。
//
// 相交的判据是 `a.start <= b.end && b.start <= a.end`,不是"端点落在里面" ——
// 后者漏判**包含**的情形(一段完全套在另一段里时两个端点都不落在对方的
// 端点上),而那正是"改窄了一个已有段"最容易造出来的形状。
func mieruRange(
	ctx context.Context, q Queryer, nodeID int64, rng mieru.PortRange, skipID int64,
) error {
	var start, end int
	err := q.QueryRowContext(ctx,
		`SELECT listen_port_start, listen_port_end FROM node_mieru_inbounds
		  WHERE node_id = ? AND deleted_at IS NULL AND id != ?
		    AND listen_port_start <= ? AND ? <= listen_port_end
		  LIMIT 1`,
		nodeID, skipID, rng.End, rng.Start).Scan(&start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w:%s 与这台机器上一个 Mieru 入口的监听端口段 %s 重叠",
		ErrConflict, label(rng), mieru.PortRange{Start: start, End: end})
}

func conflict(rng mieru.PortRange, port int, what string) error {
	return fmt.Errorf("%w:%s 撞上了 %d —— 它是%s",
		ErrConflict, label(rng), port, what)
}

// label 让错误信息里既说得出单个端口,也说得出一整段。
// 一律写成"端口段 8443-8453"的话,单端口的情形读起来像是配错了。
func label(rng mieru.PortRange) string {
	if rng.Single() {
		return "端口 " + rng.String()
	}
	return "端口段 " + rng.String()
}
