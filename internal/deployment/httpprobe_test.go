package deployment

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// 设置里留空必须回到默认目标,而默认目标必须是 https。
func TestParseProbeURLDefaults(t *testing.T) {
	got, err := ParseProbeURL("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Raw != DefaultProbeURL || !got.TLS || got.Host != "www.gstatic.com" ||
		got.Port != 443 || got.Path != "/generate_204" || got.HostHeader != "www.gstatic.com" {
		t.Errorf("默认目标解析成了 %+v", got)
	}
}

// 非默认端口既要进 CONNECT,也要进 Host 头 —— 少了后者,虚拟主机会回 404,
// 而那与"链路不通"一样会被判成失败。
func TestParseProbeURLKeepsExplicitPort(t *testing.T) {
	got, err := ParseProbeURL("http://probe.example.com:8080/ok?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TLS || got.Port != 8080 || got.HostHeader != "probe.example.com:8080" || got.Path != "/ok?x=1" {
		t.Errorf("解析结果 %+v", got)
	}
}

// 拒掉的每一种都要在保存设置时就被挡下来,而不是在十几秒后的部署记录里。
func TestParseProbeURLRejectsUnusable(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/",
		"www.gstatic.com/generate_204",
		"https://user:pw@example.com/",
		"https://example.com:70000/",
		"https:///nohost",
	} {
		if _, err := ParseProbeURL(raw); err == nil {
			t.Errorf("%q 应该被拒绝", raw)
		}
	}
}

// 在一条普通 TCP 连接上取一次响应:2xx/3xx 通过,4xx 以上失败,
// 而且请求要带 Host 头、要请求设置里那个路径。
func TestFetchOverConnJudgesByStatus(t *testing.T) {
	serve := func(t *testing.T, status string) (net.Conn, <-chan string) {
		t.Helper()
		server, client := net.Pipe()
		got := make(chan string, 1)
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			var lines []string
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					break
				}
				lines = append(lines, line)
			}
			got <- strings.Join(lines, "\n")
			_, _ = server.Write([]byte("HTTP/1.1 " + status + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		}()
		return client, got
	}

	target, _ := ParseProbeURL("http://probe.example.com:8080/generate_204")

	conn, got := serve(t, "204 No Content")
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	detail, err := fetchOverConn(context.Background(), conn, target)
	conn.Close()
	if err != nil {
		t.Fatalf("204 应该通过: %v", err)
	}
	if !strings.Contains(detail, "HTTP 204") {
		t.Errorf("详情要写明状态码:%s", detail)
	}
	req := <-got
	if !strings.HasPrefix(req, "GET /generate_204 HTTP/1.1") {
		t.Errorf("请求行不对:%q", req)
	}
	if !strings.Contains(req, "Host: probe.example.com:8080") {
		t.Errorf("Host 头要带端口:%q", req)
	}

	conn, _ = serve(t, "503 Service Unavailable")
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fetchOverConn(context.Background(), conn, target)
	conn.Close()
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("5xx 必须判失败并带回状态码,实际 %v", err)
	}
}

// 对端在数据阶段直接断开 —— 这是"隧道通了、后面某一跳断了"的新形状,
// 报错里必须让人认得出它不是 SOCKS 阶段的失败。
func TestFetchOverConnReportsEOFAsNoResponse(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		r := bufio.NewReader(server)
		for {
			line, err := r.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		server.Close()
	}()
	target, _ := ParseProbeURL("http://probe.example.com/")
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_, err := fetchOverConn(context.Background(), client, target)
	client.Close()
	if err == nil || !strings.Contains(err.Error(), "未取到 HTTP 响应") {
		t.Errorf("对端断开要报成「未取到响应」,实际 %v", err)
	}
}

// 重试次数与间隔要足以跨过启动期抖动,又不能把一次失败拖得没法忍。
func TestDialAttemptsIsSane(t *testing.T) {
	if dialAttempts < 2 {
		t.Fatal("不重试的话,一次抖动就会把健康节点回滚掉")
	}
	if worst := time.Duration(dialAttempts-1) * retryDelay; worst > 30*time.Second {
		t.Errorf("最坏情况要等 %v,太久了", worst)
	}
}
