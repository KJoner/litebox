package sshx

import (
	"errors"
	"fmt"
	"testing"
)

// 拨测失败的错误几乎一定带 "EOF" —— 隧道通了、对端在 SSH 握手时直接断开。
// 它与「面板到节点这条连接坏了」长得一模一样,而后果天差地别:
// pool.Do 会重连并重跑整个 fn,而部署走到那一步时配置已经换过、
// 服务已经重启过。真机上因此跑过两轮、重启两次。
func TestRemoteFailureIsNotAConnectionError(t *testing.T) {
	raw := errors.New("经代理完成 SSH 认证失败: ssh: handshake failed: EOF")

	if !isConnectionError(raw) {
		t.Fatal("前提变了:这条错误本来就不该被当成连接错误,那这个测试没有意义了")
	}
	if isConnectionError(RemoteFailure(raw)) {
		t.Error("打了标记还是被当成连接错误,部署会被整个重跑一遍")
	}
}

// 打标记不能改错误的文本 —— 它要原样进部署记录和系统日志。
func TestRemoteFailureKeepsTheMessage(t *testing.T) {
	raw := errors.New("经代理未读到任何数据: EOF")
	if got := RemoteFailure(raw).Error(); got != raw.Error() {
		t.Errorf("消息被改了:%q", got)
	}
}

// 原错误照常可以 errors.Is —— 上层还要靠它分辨具体是哪一种失败。
func TestRemoteFailureKeepsTheChain(t *testing.T) {
	sentinel := errors.New("拨测超时")
	wrapped := RemoteFailure(fmt.Errorf("健康检查失败: %w", sentinel))

	if !errors.Is(wrapped, sentinel) {
		t.Error("原错误链断了")
	}
	if !errors.Is(wrapped, ErrRemoteFailure) {
		t.Error("标记没打上")
	}
}

// nil 不该被包成一个非 nil 的错误 —— 那会让「部署成功」变成「部署失败」。
func TestRemoteFailureOfNilStaysNil(t *testing.T) {
	if RemoteFailure(nil) != nil {
		t.Error("nil 被包成了非 nil")
	}
}

// 写配置【之前】的失败必须仍然能触发重连重试:连接池按节点复用长连接,
// 一条连接可能几小时没用过,而 ensure 不探活 —— 那次重连正是为它准备的。
func TestPlainTransportErrorStillRetries(t *testing.T) {
	if !isConnectionError(errors.New("ssh: unexpected packet: EOF")) {
		t.Error("没打标记的传输层错误应当照常重连重试")
	}
}
