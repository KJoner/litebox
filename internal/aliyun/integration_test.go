package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 真机验证(V17 Phase 45)。需要一个真实的阿里云账号与一台可以随意开关的实例:
//
//	LITEBOX_TEST_ALIYUN_AK_ID      AccessKey ID
//	LITEBOX_TEST_ALIYUN_AK_SECRET  AccessKey Secret
//	LITEBOX_TEST_ALIYUN_REGION     实例所在区域,如 cn-hongkong
//	LITEBOX_TEST_ALIYUN_INSTANCE   实例 ID,如 i-xxxxxxxx
//	LITEBOX_TEST_ALIYUN_POWER=1    **才**会做一次 节省停机 → 开机 的循环(默认只读)
//	LITEBOX_TEST_ALIYUN_OUT        目录,设了就把每个响应原文写成 <Action>.json 当夹具
//
// 只读部分不改任何东西;POWER 那一段会让这台实例停几分钟,而且节省停机后
// 开机可能因为库存不足失败 —— 只对测试机跑。
func TestIntegrationAliyun(t *testing.T) {
	creds := Credentials{
		AccessKeyID:     os.Getenv("LITEBOX_TEST_ALIYUN_AK_ID"),
		AccessKeySecret: os.Getenv("LITEBOX_TEST_ALIYUN_AK_SECRET"),
	}
	region := os.Getenv("LITEBOX_TEST_ALIYUN_REGION")
	instance := os.Getenv("LITEBOX_TEST_ALIYUN_INSTANCE")
	if creds.AccessKeyID == "" || creds.AccessKeySecret == "" || region == "" || instance == "" {
		t.Skip("未设置 LITEBOX_TEST_ALIYUN_*,跳过阿里云真机验证")
	}
	outDir := os.Getenv("LITEBOX_TEST_ALIYUN_OUT")
	opts := []Option{}
	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		opts = append(opts, WithObserver(func(action string, status int, body []byte) {
			name := filepath.Join(outDir, fmt.Sprintf("%s-%d.json", action, time.Now().Unix()))
			_ = os.WriteFile(name, body, 0o644)
			t.Logf("[%s] HTTP %d, %d 字节 → %s", action, status, len(body), name)
		}))
	}
	c := New(opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// 1. CDT 流量。
	start := time.Now()
	list, err := c.ListCdtInternetTraffic(ctx, creds)
	if err != nil {
		t.Fatalf("ListCdtInternetTraffic: %v", err)
	}
	t.Logf("ListCdtInternetTraffic 耗时 %s,%d 个区域", time.Since(start).Round(time.Millisecond), len(list))
	for _, r := range list {
		t.Logf("  %-20s %s = %d 字节 (%.3f GiB)", r.BusinessRegionID, ClassOf(r.BusinessRegionID), r.Bytes, float64(r.Bytes)/(1<<30))
	}
	sums := SumByClass(list)
	t.Logf("  国际 %.3f GiB,内地 %.3f GiB", float64(sums[ClassInternational])/(1<<30), float64(sums[ClassChina])/(1<<30))

	// 2. 实例状态与详情。
	start = time.Now()
	status, err := c.DescribeInstanceStatus(ctx, creds, region, instance)
	if err != nil {
		t.Fatalf("DescribeInstanceStatus: %v", err)
	}
	t.Logf("DescribeInstanceStatus 耗时 %s: %s", time.Since(start).Round(time.Millisecond), status)
	inst, err := c.DescribeInstance(ctx, creds, region, instance)
	if err != nil {
		t.Fatalf("DescribeInstance: %v", err)
	}
	dump, _ := json.MarshalIndent(inst, "  ", "  ")
	t.Logf("DescribeInstance:\n  %s", dump)
	all, err := c.ListInstances(ctx, creds, region)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	t.Logf("ListInstances: 区域 %s 共 %d 台", region, len(all))

	power := os.Getenv("LITEBOX_TEST_ALIYUN_POWER")
	if power != "1" && power != "stop" {
		t.Log("未设置 LITEBOX_TEST_ALIYUN_POWER=1,跳过停机 / 开机循环")
		return
	}
	if status != StatusRunning {
		t.Fatalf("实例当前是 %s,不是 Running,不做停机循环", status)
	}
	ipBefore := inst.EffectivePublicIP()

	// 3. 节省停机 → 等 Stopped → 看 StoppedMode 与 IP。
	start = time.Now()
	if err := c.StopInstance(ctx, creds, region, instance, StopCharging); err != nil {
		t.Fatalf("StopInstance(StopCharging): %v", err)
	}
	t.Logf("StopInstance 已受理,耗时 %s", time.Since(start).Round(time.Millisecond))
	waitStatus(t, ctx, c, creds, region, instance, StatusStopped)
	t.Logf("到达 Stopped 共用 %s", time.Since(start).Round(time.Second))
	stopped, err := c.DescribeInstance(ctx, creds, region, instance)
	if err != nil {
		t.Fatalf("DescribeInstance(停机后): %v", err)
	}
	t.Logf("停机后: Status=%s StoppedMode=%q PublicIP=%q EIP=%q", stopped.Status, stopped.StoppedMode, stopped.PublicIP, stopped.EIP)
	if power == "stop" {
		// 只停不开:给面板那一侧验「不是面板停的机器,保活会把它拉起来」用。
		t.Log("LITEBOX_TEST_ALIYUN_POWER=stop,实例留在 Stopped 状态")
		return
	}

	// 4. 开机 → 等 Running → 比对 IP。
	start = time.Now()
	if err := c.StartInstance(ctx, creds, region, instance); err != nil {
		t.Fatalf("StartInstance: %v (NoStock=%v)", err, IsNoStock(err))
	}
	t.Logf("StartInstance 已受理,耗时 %s", time.Since(start).Round(time.Millisecond))
	waitStatus(t, ctx, c, creds, region, instance, StatusRunning)
	t.Logf("到达 Running 共用 %s", time.Since(start).Round(time.Second))
	after, err := c.DescribeInstance(ctx, creds, region, instance)
	if err != nil {
		t.Fatalf("DescribeInstance(开机后): %v", err)
	}
	t.Logf("开机后: PublicIP=%q EIP=%q;开机前对外地址 %q,开机后 %q,变了=%v",
		after.PublicIP, after.EIP, ipBefore, after.EffectivePublicIP(), ipBefore != after.EffectivePublicIP())
}

func waitStatus(t *testing.T, ctx context.Context, c *Client, creds Credentials, region, instance string, want InstanceStatus) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		st, err := c.DescribeInstanceStatus(ctx, creds, region, instance)
		if err != nil {
			t.Logf("  查状态失败: %v", err)
		} else {
			t.Logf("  %s 状态 %s", time.Now().Format("15:04:05"), st)
			if st == want {
				return
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("等了 6 分钟没有到达 %s", want)
}
