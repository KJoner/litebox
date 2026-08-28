package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// deadlineWriter 是一个实现了 SetWriteDeadline/SetReadDeadline 的假底层
// ResponseWriter,冒充 net/http 真正的 *http.response。
type deadlineWriter struct {
	http.ResponseWriter
	setWrite int
	setRead  int
}

func (d *deadlineWriter) SetWriteDeadline(time.Time) error { d.setWrite++; return nil }
func (d *deadlineWriter) SetReadDeadline(time.Time) error  { d.setRead++; return nil }

// statusRecorder 一旦丢了 Unwrap,http.NewResponseController 就穿不过它,
// SetWriteDeadline 静默失败,longOperation 的 10 分钟期限形同虚设,
// 60s 的 WriteTimeout 会把每一个慢操作的响应掐断 —— 而操作本身在服务端
// 跑到成功。这个用例把那条穿透路径钉死。
func TestStatusRecorderUnwrapReachesUnderlyingDeadline(t *testing.T) {
	inner := &deadlineWriter{}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	rc := http.NewResponseController(rec)
	if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetWriteDeadline 穿不过 statusRecorder(说明少了 Unwrap):%v", err)
	}
	if err := rc.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetReadDeadline 穿不过 statusRecorder:%v", err)
	}
	if inner.setWrite != 1 || inner.setRead != 1 {
		t.Fatalf("期限没有落到底层 writer 上:write=%d read=%d", inner.setWrite, inner.setRead)
	}
}

// longOperation 必须真的把写入期限设上去;设不上时要走 longOpDeadlineFailed
// 报警,而不是静默把 60s 上限留给一个可能跑几分钟的操作。
func TestLongOperationSetsWriteDeadline(t *testing.T) {
	inner := &deadlineWriter{}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	called := false
	prev := longOpDeadlineFailed
	longOpDeadlineFailed = func(http.ResponseWriter, error) { called = true }
	defer func() { longOpDeadlineFailed = prev }()

	h := longOperation(func(http.ResponseWriter, *http.Request) {})
	req, _ := http.NewRequest(http.MethodPost, "/x", nil)
	h(rec, req)

	if inner.setWrite == 0 {
		t.Fatal("longOperation 没有放宽写入期限")
	}
	if called {
		t.Fatal("底层支持 SetWriteDeadline,不该报期限设置失败")
	}
}
