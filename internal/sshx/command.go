// Package sshx 提供到节点的 SSH 连接池、远程命令执行与文件传输。
//
// 主控与节点之间的所有交互都走这里。两条硬性约束:
//   - 远程命令不得由字符串拼接产生,必须通过 Command 构造并对参数做 shell 转义;
//   - SSH 连接按节点复用(建连约 1.3 秒,单次调用约 157 毫秒,见 Phase 0 报告第 8 节)。
package sshx

import (
	"fmt"
	"strings"
)

// Command 是一条待执行的远程命令。
// 程序名与参数分开保存,序列化时对每个参数做 shell 转义,
// 因此参数中的空格、引号、分号、反引号都不会被远端 shell 解释。
type Command struct {
	Name string
	Args []string
}

// NewCommand 构造一条远程命令。
func NewCommand(name string, args ...string) Command {
	return Command{Name: name, Args: args}
}

// String 返回可安全交给远端 shell 执行的命令行。
func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, shellQuote(c.Name))
	for _, arg := range c.Args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// ShellQuote 是 shellQuote 的导出版本,供需要自己拼 sh -c 脚本的调用方用。
//
// 拼脚本本身要尽量避免(所有远程参数严格校验是硬约束),但有几处
// 确实绕不开 —— 比如「文件在就 cp、不在就退 42」这种带条件的动作,
// 拆成两条命令会在两次往返之间出现一个别人可以插进来的窗口。
func ShellQuote(s string) string { return shellQuote(s) }

// shellQuote 用单引号包裹字符串。单引号内 POSIX shell 不做任何解释,
// 唯一需要处理的是单引号本身:闭合、插入转义后的单引号、再重新开启。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// 仅由安全字符组成时不加引号,便于阅读日志。
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == ':', r == '=', r == '+', r == ',', r == '@':
		default:
			return false
		}
	}
	return true
}

// Result 是一次远程命令的执行结果。
type Result struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
}

// Err 在退出码非零时返回带上下文的错误。
func (r Result) Err() error {
	if r.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(r.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(r.Stdout)
	}
	if detail == "" {
		return fmt.Errorf("命令 %s 退出码 %d", r.Command, r.ExitCode)
	}
	return fmt.Errorf("命令 %s 退出码 %d: %s", r.Command, r.ExitCode, truncate(detail, 512))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(已截断)"
}
