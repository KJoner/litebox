package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/task"
)

func waitTask(t *testing.T, store *task.Store, id int64) *task.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != task.StatusRunning {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("任务一直没跑完")
	return nil
}

// ?async=1 立即返回 202 + task_id;处理器在后台跑完,本来要返回的状态与 JSON 原样记下来;
// 请求体照样读得到,ctx 不随请求结束而取消。不带 async 时行为不变。
func TestAsyncLongOperation(t *testing.T) {
	env := newTestEnv(t)
	s := env.api
	release := make(chan struct{})
	h := s.longOperation(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		<-release
		if err := r.Context().Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "ctx 被取消了:"+err.Error())
			return
		}
		task.Report(r.Context(), "第一步")
		if strings.Contains(string(body), "fail") {
			writeError(w, http.StatusConflict, "按要求失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"echo": string(body)})
	})

	post := func(query, body, idem string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/7/deploy"+query, strings.NewReader(body))
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	rec := post("?async=1", `{"x":1}`, "k-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("async 应返回 202,得到 %d:%s", rec.Code, rec.Body)
	}
	var accepted struct {
		TaskID int64 `json:"task_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &accepted)
	if accepted.TaskID == 0 {
		t.Fatal("没有 task_id")
	}
	// 同一个幂等键再提交一次:拿到同一个任务,处理器不会再跑一遍。
	dup := post("?async=1", `{"x":1}`, "k-1")
	var dupBody struct {
		TaskID    int64 `json:"task_id"`
		Duplicate bool  `json:"duplicate"`
	}
	json.Unmarshal(dup.Body.Bytes(), &dupBody)
	if dupBody.TaskID != accepted.TaskID || !dupBody.Duplicate {
		t.Fatalf("重复提交得到 %+v", dupBody)
	}
	close(release)

	got := waitTask(t, s.tasks, accepted.TaskID)
	if got.Status != task.StatusSucceeded || got.HTTPStatus != http.StatusOK {
		t.Fatalf("任务结果 %+v", got)
	}
	if !strings.Contains(string(got.Result), `{\"x\":1}`) {
		t.Errorf("请求体没带到后台:%s", got.Result)
	}
	if got.Target != "node:7" && got.Target != "node" {
		t.Errorf("target = %q", got.Target)
	}

	// 失败的状态码原样记下来,任务算失败。
	rec = post("?async=1", `fail`, "")
	json.Unmarshal(rec.Body.Bytes(), &accepted)
	got = waitTask(t, s.tasks, accepted.TaskID)
	if got.Status != task.StatusFailed || got.HTTPStatus != http.StatusConflict || !strings.Contains(string(got.Result), "按要求失败") {
		t.Fatalf("失败任务 %+v %s", got, got.Result)
	}

	// 不带 async:同步返回,不登记任务。
	rec = post("", `{"y":2}`, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo") {
		t.Fatalf("同步调用 %d %s", rec.Code, rec.Body)
	}
}

// 主控重启:运行中的任务标成待核对,不会被当成还在跑,也不会被重放。
func TestTasksMarkedInterruptedOnRestart(t *testing.T) {
	env := newTestEnv(t)
	tk, _, err := env.api.tasks.Begin(t.Context(), "POST /api/nodes/{id}/deploy", "/api/nodes/1/deploy", "node:1", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := env.api.tasks.MarkInterrupted(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("标记了 %d 个(%v)", n, err)
	}
	got, _ := env.api.tasks.Get(t.Context(), tk.ID)
	if got.Status != task.StatusInterrupted || got.FinishedAt == nil {
		t.Fatalf("重启后状态 %+v", got)
	}
	// 已结束的任务不会被 Finish 改写。
	env.api.tasks.Finish(t.Context(), tk.ID, 200, []byte(`{}`))
	if again, _ := env.api.tasks.Get(t.Context(), tk.ID); again.Status != task.StatusInterrupted {
		t.Fatal("待核对的任务被改成了别的状态")
	}
}

// 任务接口经路由可读;不存在的 404。
func TestTaskEndpoints(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	tk, _, _ := env.api.tasks.Begin(t.Context(), "k", "/api/x", "node:1", 1, "")
	env.api.tasks.Finish(t.Context(), tk.ID, 200, []byte(`{"ok":true}`))
	resp := env.do(t, http.MethodGet, "/api/tasks/"+itoa(tk.ID), nil)
	var got task.Task
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Status != task.StatusSucceeded || string(got.Result) != `{"ok":true}` {
		t.Fatalf("GET 任务 %+v %s", got, got.Result)
	}
	resp = env.do(t, http.MethodGet, "/api/tasks/999999", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的任务 %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = env.do(t, http.MethodGet, "/api/tasks?target=node:1", nil)
	var list struct {
		Items []task.Task `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Errorf("按对象筛选得到 %d 条", len(list.Items))
	}
}
