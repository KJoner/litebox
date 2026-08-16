package node

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// facts 造一份内存为 memMB 的节点事实。
func tuneFactsWithMem(memMB int64) TuneFacts {
	return TuneFacts{
		OSName:      "Debian GNU/Linux 12 (bookworm)",
		Kernel:      "6.1.0-18-amd64",
		Virt:        "kvm",
		MemTotalKB:  memMB * 1024,
		CPUCount:    1,
		DiskTotalKB: 20 * 1024 * 1024,
		DiskFreeKB:  15 * 1024 * 1024,
		CCAvailable: []string{"reno", "cubic", "bbr"},
		HasSysctl:   true,
	}
}

// statesAllWritable 造一份"每个键都在、都可写、当前值是默认值"的节点状态。
func statesAllWritable(items []TuneItem) map[string]tuneValue {
	out := map[string]tuneValue{}
	for _, it := range items {
		out[it.Key] = tuneValue{value: "0"}
	}
	return out
}

func desiredOf(t *testing.T, items []TuneItem, key string) string {
	t.Helper()
	for _, it := range items {
		if it.Key == key {
			return it.Desired
		}
	}
	t.Fatalf("方案里没有 %s", key)
	return ""
}

func desiredInt(t *testing.T, items []TuneItem, key string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(desiredOf(t, items, key), 10, 64)
	if err != nil {
		t.Fatalf("%s 的值不是整数:%v", key, err)
	}
	return v
}

// 参考脚本是照着一台 1C1G 写死的常量表。原样搬到 128MB 的 NAT 小鸡上,
// 有几项会从"优化"变成"把机器推进 OOM",而且全程不报错。
// 这个用例钉住:每一项都必须随内存变小而变小。
func TestPlanScalesDownOnSmallMemory(t *testing.T) {
	small, _ := planTune(tuneInputs{facts: tuneFactsWithMem(128)})
	large, _ := planTune(tuneInputs{facts: tuneFactsWithMem(4096)})

	for _, key := range []string{
		"net.core.rmem_max",
		"net.core.wmem_max",
		"fs.file-max",
		"net.core.somaxconn",
		"net.ipv4.tcp_max_syn_backlog",
		"net.ipv4.tcp_max_tw_buckets",
	} {
		if s, l := desiredInt(t, small, key), desiredInt(t, large, key); s >= l {
			t.Errorf("%s 在 128MB 上是 %d,在 4GB 上是 %d —— 没有随内存缩小", key, s, l)
		}
	}

	// 参考脚本里的三个绝对值,在 128MB 机器上都必须被压下来。
	if v := desiredInt(t, small, "net.core.rmem_max"); v >= 64<<20 {
		t.Errorf("128MB 机器的 rmem_max 是 %d,不该达到参考脚本的 64MB", v)
	}
	if v := desiredInt(t, small, "net.ipv4.tcp_max_tw_buckets"); v >= 2000000 {
		t.Errorf("128MB 机器的 tw_buckets 是 %d —— 每桶约 200 字节,200 万个比整台机器的内存还大", v)
	}
	if v := desiredInt(t, small, "fs.file-max"); v >= 6553500 {
		t.Errorf("128MB 机器的 file-max 是 %d,那是一个它永远兑现不了的承诺", v)
	}
}

// 1C1G 是参考脚本自己的场景,方案在这一档上应当与它对齐 ——
// 缩放规则不能把一个本来正确的配置改坏。
func TestPlanMatchesReferenceOnOneGig(t *testing.T) {
	items, profile := planTune(tuneInputs{facts: tuneFactsWithMem(1024)})
	if !strings.Contains(profile, "常规档") {
		t.Errorf("1GB 应当落在常规档,得到 %q", profile)
	}
	for key, want := range map[string]int64{
		"net.core.rmem_max": 64 << 20,
		"net.core.wmem_max": 64 << 20,
	} {
		if got := desiredInt(t, items, key); got != want {
			t.Errorf("%s = %d,期望与参考脚本一致的 %d", key, got, want)
		}
	}
	if got := desiredOf(t, items, "net.ipv4.ip_local_port_range"); got != "10000 65535" {
		t.Errorf("临时端口范围 %q 与参考脚本不一致", got)
	}
}

// 缓冲区上限是**每条连接**的上限,内存越小越要压住。
// 这里对一串内存规格断言不变式,而不是逐个写死期望值 ——
// 后者在改了缩放系数之后只会全部重写一遍,守不住任何东西。
func TestBufferCeilingStaysWithinMemoryBudget(t *testing.T) {
	for _, memMB := range []int64{64, 128, 256, 512, 1024, 2048, 8192, 32768} {
		facts := tuneFactsWithMem(memMB)
		items, _ := planTune(tuneInputs{facts: facts})
		buf := desiredInt(t, items, "net.core.rmem_max")

		if buf < 4<<20 || buf > 64<<20 {
			t.Errorf("%d MB:缓冲区上限 %d 越出 [4MB, 64MB]", memMB, buf)
		}
		// 除了被下限托住的极小机器,上限不得超过内存的 1/16。
		if buf > 4<<20 && buf > facts.MemTotalKB*1024/16 {
			t.Errorf("%d MB:缓冲区上限 %d 超过内存的 1/16", memMB, buf)
		}

		// 起步值是真的会占用的内存,必须比上限小一个数量级以上。
		rmem := strings.Fields(desiredOf(t, items, "net.ipv4.tcp_rmem"))
		if len(rmem) != 3 {
			t.Fatalf("tcp_rmem 应当是三段值,得到 %v", rmem)
		}
		def, _ := strconv.ParseInt(rmem[1], 10, 64)
		max, _ := strconv.ParseInt(rmem[2], 10, 64)
		if def >= max {
			t.Errorf("%d MB:tcp_rmem 起步 %d 不小于上限 %d", memMB, def, max)
		}
		if max != buf {
			t.Errorf("%d MB:tcp_rmem 上限 %d 与 rmem_max %d 不一致 —— 两者不同等于只调了一半",
				memMB, max, buf)
		}
	}
}

// 把 10000-65535 全划给临时端口之后,落在这个区间里的 listen_port 会在
// **重启后**被某条出站连接抢走,sing-box 起不来。这件事只在重启那一刻发生,
// 面板上一切正常 —— 所以必须在调优时就把本机端口保留出来。
func TestReservesNodeOwnPorts(t *testing.T) {
	items, _ := planTune(tuneInputs{
		facts:        tuneFactsWithMem(1024),
		reservePorts: []int{20443, 18080, 22},
	})
	got := desiredOf(t, items, "net.ipv4.ip_local_reserved_ports")
	for _, want := range []string{"20443", "18080"} {
		if !strings.Contains(got, want) {
			t.Errorf("保留端口 %q 里缺少 %s", got, want)
		}
	}
	// 22 在临时端口范围之外,保留它只是噪音。
	if strings.Contains(got, "22") {
		t.Errorf("保留端口 %q 不该包含范围外的 22", got)
	}

	// 保留项必须排在端口范围之后 —— 报告是按顺序读的,先看到保留再看到
	// 范围的话,读的人不知道保留的是相对什么范围而言。
	rangeIdx, reservedIdx := -1, -1
	for i, it := range items {
		switch it.Key {
		case "net.ipv4.ip_local_port_range":
			rangeIdx = i
		case "net.ipv4.ip_local_reserved_ports":
			reservedIdx = i
		}
	}
	if rangeIdx < 0 || reservedIdx != rangeIdx+1 {
		t.Errorf("保留端口应当紧跟在端口范围之后,得到 range=%d reserved=%d", rangeIdx, reservedIdx)
	}
}

// 节点没有端口落在临时范围里时不能写这个键 —— 写空值会把别人已有的保留清掉。
func TestReservedPortsOmittedWhenNothingToDo(t *testing.T) {
	items, _ := planTune(tuneInputs{
		facts:        tuneFactsWithMem(1024),
		reservePorts: []int{443, 22},
	})
	for _, it := range items {
		if it.Key == "net.ipv4.ip_local_reserved_ports" {
			t.Fatalf("没有需要保留的端口时不该出现这一项,却给了 %q", it.Desired)
		}
	}
}

// 这个键可能已经有管理员或别的组件写的值,直接覆盖等于悄悄取消他们的保留。
func TestReservedPortsMergeWithExisting(t *testing.T) {
	cases := []struct {
		name    string
		current string
		ports   []int
		want    string
	}{
		{"并入已有单值", "30000", []int{20443}, "30000,20443"},
		{"已被区间覆盖则不重复添加", "20000-21000", []int{20443}, "20000-21000"},
		{"完全重复不添加", "20443", []int{20443}, "20443"},
		{"原来为空", "", []int{20443, 18080}, "18080,20443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := planReservedPorts(c.current, c.ports)
			if got != c.want {
				t.Errorf("得到 %q,期望 %q", got, c.want)
			}
		})
	}
}

// sysctl 不剥离行尾的 #,`key = 1 # 因为…` 会把注释一起当成值,
// 而写入失败只在 dmesg 里留一句话。所以注释必须独占一行。
func TestConfCommentsNeverShareLineWithValue(t *testing.T) {
	items, profile := planTune(tuneInputs{facts: tuneFactsWithMem(512)})
	markTuneState(items, statesAllWritable(items))
	conf := renderTuneConf(tuneFactsWithMem(512), profile, items, "2026-08-16T00:00:00Z")

	valueLines := 0
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		valueLines++
		if strings.Contains(line, "#") {
			t.Errorf("值行里出现了 #,整段会被当成值的一部分:%q", line)
		}
		if !strings.Contains(line, " = ") {
			t.Errorf("既不是注释也不是 key = value:%q", line)
		}
	}
	if valueLines == 0 {
		t.Fatal("渲染结果里一个键都没有")
	}
	if !strings.HasPrefix(conf, tuneMarker) {
		t.Error("首行必须是可被机器解析的 ASCII 标记,否则面板认不出这台机器调过")
	}
}

// 内核里没有的键、容器里写不了的键都不能进 conf:
// 前者会让 sysctl -p 报错,后者会让开机日志每次都刷一条失败。
func TestConfSkipsUnsupportedAndReadOnlyKeys(t *testing.T) {
	items, profile := planTune(tuneInputs{facts: tuneFactsWithMem(512)})
	markTuneState(items, statesAllWritable(items))
	items[0].State = TuneUnsupported
	items[1].State = TuneReadOnly

	conf := renderTuneConf(tuneFactsWithMem(512), profile, items, "2026-08-16T00:00:00Z")
	for _, it := range items[:2] {
		for _, line := range strings.Split(conf, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), it.Key+" ") {
				t.Errorf("%s 不可写却被写进了 conf:%q", it.Key, line)
			}
		}
		if !strings.Contains(conf, "跳过 "+it.Key) {
			t.Errorf("跳过 %s 时必须留一行说明,否则下次打开文件的人以为漏了", it.Key)
		}
	}
}

// 基线是"还原"唯一的依据。猜一个内核默认值写进去,还原时就是在乱改参数。
func TestBaselineOnlyRecordsValuesActuallyRead(t *testing.T) {
	items := []TuneItem{
		{Key: "net.core.rmem_max", Current: "212992", State: TunePending},
		{Key: "net.ipv4.tcp_bogus", Current: "", State: TuneUnsupported},
		{Key: "net.ipv4.tcp_locked", Current: "1", State: TuneReadOnly},
		{Key: "net.ipv4.tcp_rmem", Current: "4096 131072 6291456", State: TunePending},
		// 原值就是空的键必须照样入基线。按空串排除的话,还原之后这一项会
		// 保持调优时写进去的值 —— 一份说「已还原」的报告里躺着一个没还原的键。
		{Key: "net.ipv4.ip_local_reserved_ports", Current: "", State: TunePending},
	}
	baseline := renderBaseline(items, "2026-08-16T00:00:00Z")
	if strings.Contains(baseline, "tcp_bogus") {
		t.Error("内核里没有的键不能进基线 —— 还原时会去写一个不存在的参数")
	}
	if strings.Contains(baseline, "tcp_locked") {
		t.Error("写不进去的键不能进基线 —— 还原时它只会报一次必然的失败")
	}
	pairs := parseKeyValueConf(baseline)
	if len(pairs) != 3 {
		t.Fatalf("基线应当有 3 条,得到 %d 条:%s", len(pairs), baseline)
	}
	if pairs[1][1] != "4096 131072 6291456" {
		t.Errorf("多字段值被破坏了:%q", pairs[1][1])
	}
	if pairs[2][0] != "net.ipv4.ip_local_reserved_ports" || pairs[2][1] != "" {
		t.Errorf("空值的原值没有被记录:%v", pairs[2])
	}
	// 基线本身不能被 sysctl 当配置加载,所以刻意不放在 /etc/sysctl.d 下。
	if strings.HasPrefix(tuneBaselinePath, "/etc/sysctl.d") {
		t.Error("基线文件不能放在 /etc/sysctl.d 下,否则开机时会被当成配置加载")
	}
}

// systemd 从 v240 起在 PID 1 里把 fs.file-max 顶到 LONG_MAX。按内存算出来的
// 65536 照写下去,就是把一个系统级上限调低了三个数量级 —— 收不到任何好处,
// 却可能让这台机器上别的服务撞到 EMFILE。真机(Debian 12)上就是这个值。
func TestNeverLowersSystemCeilings(t *testing.T) {
	items, _ := planTune(tuneInputs{facts: tuneFactsWithMem(457)})
	states := statesAllWritable(items)
	states["fs.file-max"] = tuneValue{value: "9223372036854775807"}
	states["net.core.somaxconn"] = tuneValue{value: "65535"}
	// 对照组:TIME_WAIT 桶必须能被调低 —— 那正是"参考脚本的 200 万在小机器上
	// 是危险的"这件事的修复动作本身。
	states["net.ipv4.tcp_max_tw_buckets"] = tuneValue{value: "2000000"}
	markTuneState(items, states)

	byKey := map[string]TuneItem{}
	for _, it := range items {
		byKey[it.Key] = it
	}

	for _, key := range []string{"fs.file-max", "net.core.somaxconn"} {
		it := byKey[key]
		if it.State != TuneSame || !it.keptHigher {
			t.Errorf("%s 已经更高,应当保持不动,得到 state=%q keptHigher=%v", key, it.State, it.keptHigher)
		}
		if it.Desired != it.Current {
			t.Errorf("%s 保持不动时目标值应当等于当前值,得到 %q → %q", key, it.Current, it.Desired)
		}
	}

	tw := byKey["net.ipv4.tcp_max_tw_buckets"]
	if tw.State != TunePending || tw.keptHigher {
		t.Errorf("tcp_max_tw_buckets 必须能被调低,得到 state=%q keptHigher=%v", tw.State, tw.keptHigher)
	}

	// 保持不动的项不写进 conf:面板不接管一个不是它选的值。
	conf := renderTuneConf(tuneFactsWithMem(457), "常规档", items, "2026-08-16T00:00:00Z")
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "fs.file-max ") {
			t.Errorf("保持不动的键被写进了 conf:%q", line)
		}
	}
	if !strings.Contains(conf, "保持 fs.file-max") {
		t.Error("保持不动时要在 conf 里留一行说明,否则下次打开文件的人以为漏了")
	}
	// 也不能进写入列表 —— 写下去就是把它调低。
	for _, it := range writableItems(items) {
		if it.Key == "fs.file-max" {
			t.Error("保持不动的键进了写入列表,会把节点上更高的值改小")
		}
	}
}

// /proc 里的多字段值用制表符分隔,而我们写的是空格。
// 不归一化的话每次检查都报"不一致",管理员会以为调优根本没生效。
func TestSameValueNormalizesWhitespace(t *testing.T) {
	if !sameValue("4096\t131072\t33554432", "4096 131072 33554432") {
		t.Error("制表符与空格分隔的同一个值被判成了不同")
	}
	if sameValue("4096 131072 33554432", "4096 131072 16777216") {
		t.Error("不同的值被判成了相同")
	}
}

// bbr 常常只是模块没加载。检查阶段把它写成"内核不支持"会让管理员放弃,
// 而应用阶段会先 modprobe —— 两个阶段的结论必须不一样。
func TestBBRVerdictDiffersBetweenPreviewAndApply(t *testing.T) {
	facts := tuneFactsWithMem(1024)
	facts.CCAvailable = []string{"reno", "cubic"}

	preview, _ := planTune(tuneInputs{facts: facts})
	apply, _ := planTune(tuneInputs{facts: facts, applying: true})

	var previewItem, applyItem TuneItem
	for _, it := range preview {
		if it.Key == "net.ipv4.tcp_congestion_control" {
			previewItem = it
		}
	}
	for _, it := range apply {
		if it.Key == "net.ipv4.tcp_congestion_control" {
			applyItem = it
		}
	}
	if previewItem.State == TuneUnsupported {
		t.Error("检查阶段不该断言 bbr 不可用 —— 应用时会先 modprobe tcp_bbr")
	}
	if !strings.Contains(previewItem.Detail, "modprobe") {
		t.Errorf("检查阶段要说明应用时会尝试加载模块,得到 %q", previewItem.Detail)
	}
	if applyItem.State != TuneUnsupported {
		t.Errorf("modprobe 之后仍然拿不到 bbr 就是不支持,得到状态 %q", applyItem.State)
	}
	if !strings.Contains(applyItem.Detail, "cubic") {
		t.Errorf("不支持时要说明这台机器上有什么可用,得到 %q", applyItem.Detail)
	}
}

// 容器里写 /proc/sys 常常"成功"却不生效,sysctl 的退出码同样靠不住。
// 唯一算数的证据是把值读回来 —— 这个用例钉住判定依据是读回值而不是写入结果。
func TestVerifyTrustsReadBackNotWriteResult(t *testing.T) {
	items := []TuneItem{
		{Key: "net.core.rmem_max", Desired: "33554432", State: TunePending},
		{Key: "net.ipv4.tcp_fastopen", Desired: "3", State: TunePending},
		{Key: "net.ipv4.tcp_sack", Desired: "1", State: TuneSame},
	}
	// 第一项:写入没报错,但值没变 —— 必须判失败。
	after := map[string]tuneValue{
		"net.core.rmem_max":     {value: "212992"},
		"net.ipv4.tcp_fastopen": {value: "3"},
		"net.ipv4.tcp_sack":     {value: "1"},
	}
	verifyTuneState(items, after, map[string]string{})

	if items[0].State != TuneFailed {
		t.Errorf("写入无报错但读回是旧值,应当判失败,得到 %q", items[0].State)
	}
	if !strings.Contains(items[0].Detail, "212992") {
		t.Errorf("失败详情要写出实际读回的值,得到 %q", items[0].Detail)
	}
	if items[1].State != TuneApplied {
		t.Errorf("读回一致应当是已生效,得到 %q", items[1].State)
	}
	if items[2].State != TuneSame {
		t.Errorf("本来就一致的项不该被改写成「已生效」,得到 %q", items[2].State)
	}
}

// 写入失败时,errno 是最有用的一条线索:EROFS 说明这是容器,
// EINVAL 说明内核不认这个值。它不能被"读回来不一样"这句话盖掉。
func TestVerifyKeepsWriteErrno(t *testing.T) {
	items := []TuneItem{{Key: "net.core.rmem_max", Desired: "33554432", State: TunePending}}
	verifyTuneState(items,
		map[string]tuneValue{"net.core.rmem_max": {value: "212992"}},
		map[string]string{"net.core.rmem_max": "sh: can't create /proc/sys/net/core/rmem_max: Read-only file system"})

	if items[0].State != TuneFailed {
		t.Fatalf("状态应当是失败,得到 %q", items[0].State)
	}
	if !strings.Contains(items[0].Detail, "Read-only file system") {
		t.Errorf("失败详情丢掉了 errno:%q", items[0].Detail)
	}
}

func TestParseTuneFacts(t *testing.T) {
	out := strings.Join([]string{
		"os Debian GNU/Linux 12 (bookworm)",
		"kernel 6.1.0-18-amd64",
		"mem_total_kb 1010352",
		"cpus 2",
		"disk 20514816 15728640",
		"virt kvm",
		"cc_available reno cubic bbr",
		"cc_current cubic",
		"qdisc_now fq_codel",
		"reserved_now 30000,30001",
		"has_sysctl 1",
		"tuned 2026-08-16T09:12:33Z",
		"baseline 1",
		"nofile 1048576",
		"persist systemd",
	}, "\n")

	facts, extra := parseTuneFacts(out)
	if facts.OSName != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("带空格的系统名被截断了:%q", facts.OSName)
	}
	if facts.MemTotalKB != 1010352 || facts.MemTotalMB() != 986 {
		t.Errorf("内存解析错误:%d KB", facts.MemTotalKB)
	}
	if facts.DiskTotalKB != 20514816 || facts.DiskFreeKB != 15728640 {
		t.Errorf("磁盘解析错误:%d / %d", facts.DiskTotalKB, facts.DiskFreeKB)
	}
	if len(facts.CCAvailable) != 3 || facts.CCAvailable[2] != "bbr" {
		t.Errorf("拥塞算法列表解析错误:%v", facts.CCAvailable)
	}
	if facts.NoFileLimit != 1048576 || !facts.HasSysctl {
		t.Errorf("句柄上限 / sysctl 解析错误:%d %v", facts.NoFileLimit, facts.HasSysctl)
	}
	if extra.tunedAt != "2026-08-16T09:12:33Z" || !extra.baseline {
		t.Errorf("conf 与基线状态解析错误:%+v", extra)
	}
	if len(extra.persist) != 1 || extra.persist[0] != "systemd" {
		t.Errorf("持久化状态解析错误:%v", extra.persist)
	}
}

// 采不到内存就必须停:缓冲区的每个数字都由它推出来,
// 随便挑一个默认值等于在一台我们一无所知的机器上写内核参数。
func TestFactsWithoutMemoryIsRejected(t *testing.T) {
	facts, _ := parseTuneFacts("os Alpine Linux v3.20\nkernel 6.6.0\n")
	if facts.MemTotalKB != 0 {
		t.Fatalf("没有 MemTotal 行时不该有内存值,得到 %d", facts.MemTotalKB)
	}
}

// 报告里的数组字段为 nil 会序列化成 JSON null,而前端一律当数组用。
// 最难发现的是它只在成功路径上出现:一台完全不需要调整的机器
// Warnings 恰恰是 nil —— "一切正常"反而让详情抽屉在渲染期抛 TypeError。
func TestTuneReportMarshalsEmptyArraysNotNull(t *testing.T) {
	raw, err := json.Marshal(newTuneReport(1, TuneModePreview))
	if err != nil {
		t.Fatalf("序列化失败:%v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败:%v", err)
	}
	for _, field := range []string{"items", "warnings", "notes"} {
		if strings.TrimSpace(string(decoded[field])) == "null" {
			t.Errorf("字段 %s 是 null,应当是 []", field)
		}
	}
	// 嵌套的那个同样要看:前端读的是 facts.cc_available。
	var facts map[string]json.RawMessage
	if err := json.Unmarshal(decoded["facts"], &facts); err != nil {
		t.Fatalf("facts 反序列化失败:%v", err)
	}
	if strings.TrimSpace(string(facts["cc_available"])) == "null" {
		t.Error("facts.cc_available 是 null,应当是 []")
	}
}

func TestParseKeyValueConfIgnoresCommentsAndBlankLines(t *testing.T) {
	pairs := parseKeyValueConf(strings.Join([]string{
		"# 注释",
		"; 另一种注释",
		"",
		"net.core.rmem_max = 212992",
		"net.ipv4.tcp_rmem =  4096\t131072\t6291456 ",
		"没有等号的行",
		"= 没有键",
	}, "\n"))

	if len(pairs) != 2 {
		t.Fatalf("应当解析出 2 条,得到 %d 条:%v", len(pairs), pairs)
	}
	if pairs[1][1] != "4096 131072 6291456" {
		t.Errorf("多字段值未归一化:%q", pairs[1][1])
	}
}

// 方案里的每个键都必须带一句"这个数字怎么来的" —— 它会原样写进节点上的
// 配置文件,半年后打开那个文件的人不该还得回到面板才看得懂。
func TestEveryItemExplainsItself(t *testing.T) {
	items, _ := planTune(tuneInputs{facts: tuneFactsWithMem(512), reservePorts: []int{20443}})
	if len(items) < 20 {
		t.Fatalf("方案只有 %d 项,少于参考脚本覆盖的范围", len(items))
	}
	seen := map[string]bool{}
	for _, it := range items {
		if it.Reason == "" {
			t.Errorf("%s 没有说明它的取值依据", it.Key)
		}
		if it.Group == "" {
			t.Errorf("%s 没有分组", it.Key)
		}
		if seen[it.Key] {
			t.Errorf("%s 出现了两次 —— 后一次会覆盖前一次,而 conf 里看起来两条都在", it.Key)
		}
		seen[it.Key] = true
	}
	// 参考脚本里明确写了"不要用 -2",这条结论不能在缩放的时候丢掉。
	if v := desiredOf(t, items, "net.ipv4.tcp_adv_win_scale"); v != "1" {
		t.Errorf("tcp_adv_win_scale = %q,参考脚本特意写明高 BDP 下不要用 -2", v)
	}
	// tcp_mem 是全系统 TCP 内存的硬上限,内核已按物理内存自算。
	if seen["net.ipv4.tcp_mem"] {
		t.Error("不该设置 net.ipv4.tcp_mem —— 写死一个值是让这台机器 OOM 最快的路径")
	}
}
