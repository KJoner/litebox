// litebox-probe 是出站网络质量探测的非驻留辅助程序。
//
// 主控经已有的 SSH 长连接把它放到 VPS 上(/opt/litebox/litebox-probe),每一轮启动一次:
// 从 stdin 读一份 JSON(要测的目标)、在本机并发测完、把结果 JSON 写到 stdout,然后退出。
// 它不是守护进程:不常驻、不开机自启、不监听任何端口、不写任何文件。
//
// 目标只从 stdin 读,不收命令行参数 —— 地址与端口不经过远端 shell,没有注入面。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "-version" || os.Args[1] == "--version" {
			fmt.Printf("litebox-probe %d\n", netprobe.Version)
			return
		}
		fmt.Fprintln(os.Stderr, "用法:litebox-probe < request.json(目标只从 stdin 读)")
		os.Exit(2)
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取输入失败:", err)
		os.Exit(2)
	}
	var req netprobe.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintln(os.Stderr, "输入不是合法的 JSON:", err)
		os.Exit(2)
	}
	// 认得的版本区间里都能跑,并按请求的版本回话:面板回滚到旧版本时,节点上这一份新的照样能用。
	// 比它新的请求说明主控升级了而这里还是旧文件 —— 主控会按 sha256 重传,这一轮如实报「版本不符」。
	if req.Version < netprobe.MinVersion || req.Version > netprobe.Version {
		fmt.Fprintf(os.Stderr, "协议版本不符:主控 %d,辅助程序支持 %d~%d\n", req.Version, netprobe.MinVersion, netprobe.Version)
		os.Exit(3)
	}
	if len(req.Targets) > netprobe.MaxTargets {
		req.Targets = req.Targets[:netprobe.MaxTargets]
	}
	for i := range req.Targets {
		// 先夹好参数再算总时长:总时长决定这一次最多跑多久。
		_ = netprobe.Normalize(&req.Targets[i])
	}
	// 硬上限:哪怕某个调用阻塞住,这个进程也一定会在预算之后退出,不会留在机器上。
	budget := netprobe.Budget(req.Targets) + 2*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	go func() {
		<-time.After(budget + 3*time.Second)
		fmt.Fprintln(os.Stderr, "超出时间预算,强制退出")
		os.Exit(4)
	}()
	resp := netprobe.Default().RunBatch(ctx, req)
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		os.Exit(5)
	}
}
