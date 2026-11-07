package sshx

import (
	"context"
	"errors"
	"fmt"
)

// 失锁之后的远程写入(G02)。
//
// C02 只在两个地方问闸门:Do / TryDo 进门时,以及部署事务替换配置之前。可已经在跑的任务不止这两步 ——
// 连接断了之后的重连重试、替换之后的重启、失败之后的回滚(回滚还特意用 WithoutCancel 跑完),
// 都会在闸门已经关上之后接着碰节点。另一个实例这时已经接管的话,旧实例的「善后」就是拿旧配置
// 覆盖新实例刚下发的东西。
//
// 所以现在**每一条远程操作**(执行命令、开通道、传文件、读文件)之前都问一次,而且问两件事:
//   - 此刻能不能确认持有锁(Fence);
//   - 锁还是不是这个任务开始时的那一代(FenceGeneration)—— 锁断过又重新拿回来,中间可能有别的实例
//     接管并下发过更新的配置,旧任务凭它开始时渲染的那份接着写,就是用旧状态覆盖新状态。
// 网络故障不可能零延迟发现,这里要做的是:闸门一关,之后的远程变更一步都不再发生。

// Fence 回答「现在能不能去节点上做事」(C02):主控在 serve 里设成 database.CheckWriteFence。
// 不直接依赖 database 包:sshx 是底层,谁来提供闸门由上层决定。
var Fence func() error

// FenceGeneration 返回运行锁当前的世代(G02):主控设成 database.FenceGeneration。为 nil 或返回 0 时不比世代。
var FenceGeneration func() uint64

// ErrFenced 表示这个实例已经不能确认自己持有单活动主控锁,或者锁在这个任务开始之后换过手。
// 碰到它的任务一律停下:不重试、不回滚、不写「部署失败」—— 新的持锁实例会按最新的状态重新协调。
var ErrFenced = errors.New("失去单活动主控锁:这个实例不再对节点做任何操作")

// IsFenced 回答这个错误是不是闸门关上造成的。
func IsFenced(err error) bool { return errors.Is(err, ErrFenced) }

type fenceGenKey struct{}

// PinFence 记下这个任务开始时运行锁的世代。之后它经连接池做的每一步远程操作都要求世代没变。
// 已经记过的不再覆盖(一个任务里嵌套的调用沿用最外层那一次)。
func PinFence(ctx context.Context) context.Context {
	if _, ok := ctx.Value(fenceGenKey{}).(uint64); ok {
		return ctx
	}
	return context.WithValue(ctx, fenceGenKey{}, currentGeneration())
}

func currentGeneration() uint64 {
	if FenceGeneration == nil {
		return 0
	}
	return FenceGeneration()
}

// generationFor 是这个任务要求的世代:记过的用记过的,没记过的就是此刻的。
func generationFor(ctx context.Context) uint64 {
	if g, ok := ctx.Value(fenceGenKey{}).(uint64); ok {
		return g
	}
	return currentGeneration()
}

// checkFence 是每一条远程操作之前的那一问。gen 为 0 表示不比世代(连接池以外直接拨的连接)。
func checkFence(gen uint64) error {
	if Fence != nil {
		if err := Fence(); err != nil {
			return fmt.Errorf("%w(%w)", ErrFenced, err)
		}
	}
	if gen != 0 {
		if now := currentGeneration(); now != gen {
			return fmt.Errorf("%w(这个任务开始之后运行锁换过手:第 %d 代 → 第 %d 代)", ErrFenced, gen, now)
		}
	}
	return nil
}

// CheckFence 给不经 Client 的调用方(部署事务的回滚分支)用:这一步之前闸门还开着吗。
func (c *Client) CheckFence() error {
	if c == nil {
		return checkFence(0)
	}
	return checkFence(c.fenceGen)
}
