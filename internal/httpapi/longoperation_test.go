package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type ctxProbeKey struct{}

// 客户端断开(或反代读超时把连接掐了)不得中止一次已经开始的节点操作。
//
// 不这么做的话,部署会停在半路 —— 配置已经换过、服务已经重启,而部署记录、
// 节点状态与审计一条都写不进去。生产上真的发生过一次 chain_apply 被掐断,
// 面板日志里只剩三行 context canceled,而面板上连一条部署记录都没有。
func TestLongOperationSurvivesClientDisconnect(t *testing.T) {
	clientGone, disconnect := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/api/nodes/1/deploy", nil).
		WithContext(clientGone)
	// 处理器还没开始跑,客户端就断了 —— 与反代超时掐连接是同一件事。
	disconnect()

	var seen error
	handler := longOperation(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Context().Err()
	})
	handler(httptest.NewRecorder(), r)

	if seen != nil {
		t.Fatalf("客户端断开后处理器的 ctx 被取消了(%v),部署会停在半路", seen)
	}
}

// WithoutCancel 只丢弃取消与期限,ctx 里的值必须照常带下去 ——
// 管理员身份就在里面,而审计要记的正是他。丢了的话,一次长操作
// 写出来的审计记录没有操作人。
func TestLongOperationKeepsContextValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/nodes/1/deploy", nil).
		WithContext(context.WithValue(context.Background(), ctxProbeKey{}, "admin-7"))

	var seen any
	longOperation(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Context().Value(ctxProbeKey{})
	})(httptest.NewRecorder(), r)

	if seen != "admin-7" {
		t.Fatalf("ctx 里的值没带下去:%v", seen)
	}
}

// 解绑不等于永远挂着:10 分钟的上限仍然要在,否则一个连不上的节点
// 会把处理器和它占着的节点锁一起拖住。
func TestLongOperationStillHasDeadline(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/nodes/1/deploy", nil)

	var deadline time.Time
	var ok bool
	longOperation(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	})(httptest.NewRecorder(), r)

	if !ok {
		t.Fatal("处理器的 ctx 没有期限")
	}
	if left := time.Until(deadline); left <= 0 || left > LongOperationTimeout {
		t.Errorf("期限不对:剩余 %v,上限 %v", left, LongOperationTimeout)
	}
}
