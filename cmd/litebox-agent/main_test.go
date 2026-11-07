//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/litebox/litebox/internal/agentproto"
)

// R08:只有能解析、带 accepted、处理条数对得上的应答才算确认。
func TestParseReplyRequiresRealAck(t *testing.T) {
	cases := []struct {
		name string
		body string
		sent int
		ok   bool
	}{
		{"正常确认", `{"accepted":2,"handled":3,"interval_seconds":60}`, 3, true},
		{"旧版本主控没有 handled", `{"accepted":3,"interval_seconds":60}`, 3, true},
		{"反代的出错页", `<html><body>502 Bad Gateway</body></html>`, 3, false},
		{"空响应", ``, 3, false},
		{"别的 JSON", `{"code":"UNAUTHENTICATED"}`, 3, false},
		{"只处理了一部分", `{"accepted":1,"handled":1}`, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseReply([]byte(c.body), c.sent)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v,期望确认 = %v", err, c.ok)
			}
		})
	}
}

// R08:旧版本写的队列里样本不带开机 ID,读状态时补成状态文件里那一次开机;
// 机器重启过时序号从头来,但队列里的旧样本保留各自的开机 ID。
func TestLoadStateKeepsQueueBootIDs(t *testing.T) {
	dir := t.TempDir()
	old := persisted{BootID: "boot-a", Seq: 100, Queue: []agentproto.Sample{{Seq: 99}, {Seq: 100}}, DroppedCum: 7}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &agent{state: dir}
	a.loadState() // 测试机的真实 boot_id 与 boot-a 不同:相当于宿主重启过
	if a.bootID == "boot-a" {
		t.Skip("测试机的 boot_id 恰好是 boot-a")
	}
	if a.seq != 0 {
		t.Fatalf("宿主重启之后序号应当从头来,得到 %d", a.seq)
	}
	for _, s := range a.queue {
		if s.BootID != "boot-a" {
			t.Fatalf("旧队列里的样本冒充了新开机:%+v", s)
		}
	}
	if a.droppedCum != 7 {
		t.Fatalf("累计丢弃数没读回来:%d", a.droppedCum)
	}
}
