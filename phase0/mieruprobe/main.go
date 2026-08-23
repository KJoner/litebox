// mieruprobe 是 LiteBox V13(Mieru 落地协议)的技术验证探针。
//
// 它回答一个问题,而那个问题挡着 V13 的全部后续工作:
//
//	**mita 的 per-user 流量计数器,到底是什么语义?**
//
// 面板现有的入账模型建立在 sing-box 的 V2Ray Stats 之上:计数器随进程存在、
// 单调递增、重启归零(靠 GetSysStats.Uptime 判重启)。mita 那边看起来不一样 ——
// 指标持久化在 /var/lib/mita/metrics.pb 且「重启后依然保留」,而 CLI
// (mita get users)给出的是 1 天 / 30 天的**滚动窗口**,还四舍五入成
// "938.1MiB" 这种可读形式。
//
// 滚动窗口做增量会算出负数;四舍五入后的值精度只有几十 KB。两者都不能
// 直接拿来入账,而**猜错的代价是把用户流量算成天文数字或者归零** ——
// 那正是 CLAUDE.md 里「流量同步读取失败必须在进入数据库事务前返回,
// 任何情况下不得把用户流量归零」要防的那一类。所以先量,再写代码。
//
// 用法(在【节点本机】上跑,它要连 mita 的 Unix socket):
//
//	./mieruprobe                 # 打印用户与全局指标的原始值
//	./mieruprobe -json           # 原样输出 protobuf 的 JSON,便于粘回来
//	./mieruprobe -sock /path     # 指定 socket 路径
//	./mieruprobe -watch 30s      # 每隔一段时间打一次,用来看计数器怎么动
//
// 与 statsprobe 一样是独立 module:它 import 上游 mieru 的生成代码,
// 而那份依赖不该进主二进制。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/enfein/mieru/v3/pkg/appctl/appctlgrpc"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/enfein/mieru/v3/pkg/metrics/metricspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// defaultSock 是 mita 服务端管理接口的默认地址。
//
// 它是 **Unix domain socket**,不是 TCP —— 面板现有的 sshx 通道只做
// direct-tcpip,要读它得再加一条 direct-streamlocal@openssh.com。
// 这一点本身也是要验的:socket 属于 mita 组,面板以 root 登录能不能连上。
const defaultSock = "/var/run/mita/mita.sock"

func main() {
	sock := flag.String("sock", defaultSock, "mita 管理 socket 路径")
	asJSON := flag.Bool("json", false, "原样输出 protobuf JSON")
	watch := flag.Duration("watch", 0, "非零时按该间隔反复采样,用于观察计数器怎么变化")
	flag.Parse()

	conn, err := grpc.NewClient("unix://"+*sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("建立 gRPC 连接: %v", err)
	}
	defer conn.Close()

	client := appctlgrpc.NewServerManagementServiceClient(conn)

	if *watch <= 0 {
		dump(client, *asJSON)
		return
	}
	// 反复采样正是这个探针最有价值的用法:两次之间跑一笔已知大小的流量,
	// 看差值对不对得上。对不上的话,那个 metric 就不是"累积字节"。
	for i := 0; ; i++ {
		fmt.Printf("\n========== 第 %d 次采样 %s ==========\n",
			i+1, time.Now().UTC().Format(time.RFC3339))
		dump(client, *asJSON)
		time.Sleep(*watch)
	}
}

func dump(client appctlgrpc.ServerManagementServiceClient, asJSON bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if v, err := client.GetVersion(ctx, &emptypb.Empty{}); err == nil {
		fmt.Printf("mita 版本: v%d.%d.%d\n", v.GetMajor(), v.GetMinor(), v.GetPatch())
	} else {
		fmt.Printf("读取版本失败(不致命): %v\n", err)
	}
	if st, err := client.GetStatus(ctx, &emptypb.Empty{}); err == nil {
		fmt.Printf("服务状态: %s\n\n", st.GetStatus())
	}

	users, err := client.GetUsers(ctx, &emptypb.Empty{})
	if err != nil {
		fail("GetUsers: %v", err)
	}
	if asJSON {
		printJSON("users", users)
	} else {
		printUsers(users)
	}

	// GetMetrics 返回的是一整串 JSON(Metrics.json),不是结构化的 AllMetrics ——
	// 那是 mita 自己的 CLI 也在用的形式,原样打出来即可。
	all, err := client.GetMetrics(ctx, &emptypb.Empty{})
	if err != nil {
		fail("GetMetrics: %v", err)
	}
	fmt.Printf("\n---------- 全局指标(mita 原样给的 JSON)----------\n%s\n", all.GetJson())
}

// printUsers 是这个探针的核心输出。
//
// 每一项都要打全:**名字、类型、原始 int64、以及 history 的条数**。
//
//   - 名字告诉我们哪个 metric 是上行/下行字节 —— 面板要按名字去取,
//     而名字只能量出来,猜一个"UserDownloadBytes"然后取不到的表现是
//     用户流量恒为零,而同步任务每一轮都"成功";
//   - 类型区分 COUNTER(单调累积)与 COUNTER_TIME_SERIES(带 history 的
//     累积)与 GAUGE(瞬时值)。**拿 GAUGE 去做增量是灾难性的** ——
//     它会跌,而跌了之后差值是负数;
//   - 原始 int64 才是我们要的字节数,CLI 那份表格是四舍五入过的可读形式。
func printUsers(users *appctlpb.UserWithMetricsList) {
	items := users.GetItems()
	fmt.Printf("---------- 用户(%d 个)----------\n", len(items))
	if len(items) == 0 {
		fmt.Println("(一个用户都没有 —— 先用 mita apply config 配上用户并跑一点流量)")
		return
	}
	for _, item := range items {
		fmt.Printf("\n用户 %q\n", item.GetUser().GetName())
		ms := append([]*metricspb.Metric(nil), item.GetMetrics()...)
		sort.Slice(ms, func(i, j int) bool { return ms[i].GetName() < ms[j].GetName() })
		if len(ms) == 0 {
			fmt.Println("  (没有任何 metric)")
			continue
		}
		fmt.Printf("  %-32s %-22s %20s %s\n", "METRIC", "TYPE", "VALUE", "HISTORY")
		for _, m := range ms {
			fmt.Printf("  %-32s %-22s %20d %d 条\n",
				m.GetName(), m.GetType().String(), m.GetValue(), len(m.GetHistory()))
		}
	}
}

// printJSON 走 protojson 而不是 encoding/json:protobuf 的 Go 字段名与
// 它的 JSON 名不一样,用后者打出来的键名与上游文档对不上,
// 而这份输出的用途正是拿去和文档、和源码对照。
func printJSON(label string, msg proto.Message) {
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		fail("序列化 %s: %v", label, err)
	}
	fmt.Printf("\n---------- %s(原始 JSON)----------\n%s\n", label, raw)
}

// fail 统一退出。探针的错误要说清"下一步该做什么" ——
// 它是给在真机上敲命令的人看的,不是给日志收集器看的。
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n探测失败:"+format+"\n", args...)
	fmt.Fprintf(os.Stderr, `
排查顺序:
  1. mita 在跑吗?          systemctl status mita
  2. socket 在吗?          ls -l %s
  3. 权限够吗?             socket 属于 mita 组,用 root 或 usermod -a -G mita $USER
  4. 配置生效了吗?         mita describe config
`, defaultSock)
	os.Exit(1)
}
