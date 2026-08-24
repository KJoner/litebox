package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/sshx"
)

// MieruConfigDiff 是一个 Mieru 入口的「库里想要的」与「节点上跑着的」之差。
//
// **它与 sing-box 那份 diff 是两件事,所以分开给。** 两者比的是不同的进程、
// 不同的配置文件,合成一份的话,「配置无变化」这句话会变得没有意义 ——
// sing-box 一字未改而某个 mita 实例的用户列表少了一个人,是完全可能的,
// 而那正是管理员最需要看见的。
type MieruConfigDiff struct {
	InboundID   int64  `json:"inbound_id"`
	DisplayName string `json:"display_name"`

	// Deployed 为 false 表示这个入口还没上过节点 —— 那不是"有差异",
	// 是"还没开始"。混在一起会让管理员去比对一份根本不存在的配置。
	Deployed bool `json:"deployed"`
	// Changed 是「参数或用户有差异」。
	Changed bool `json:"changed"`
	// Attrs 是参数层面的差异(端口段、传输层、MTU、出口),
	// 判据是库里的期望值与 deployed_* 那几列 —— 不需要连节点。
	Attrs []string `json:"attrs"`

	// DesiredUsers 是库里算出来的用户列表。
	DesiredUsers []string `json:"desired_users"`
	// RemoteUsers 是节点上那个 mita 实例此刻的用户列表。
	// 读不到时为空,并由 Error 说明原因。
	RemoteUsers []string `json:"remote_users"`
	UsersAdded  []string `json:"users_added"`
	UsersRemove []string `json:"users_removed"`

	// Error 是读节点那一步的失败原因。**不让它中止整份比对** ——
	// 一台机器上有好几个实例,其中一个读不到不该让另外几个也看不成。
	Error string `json:"error,omitempty"`
}

// mieruDiffs 逐个比对这台机器上的 Mieru 入口。
//
// 复用调用方那一次 pool.Do 的连接:节点级互斥锁不可重入,而每建一次连接
// 约 1.3 秒 —— 每个实例各连一次会让「比对配置」在三个入口的机器上
// 多花四秒,换不来任何东西。
func (s *Service) mieruDiffs(
	ctx context.Context, client *sshx.Client, n *Node,
) ([]MieruConfigDiff, error) {
	list, err := s.store.MieruInboundsForNode(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	out := make([]MieruConfigDiff, 0, len(list))
	layout := s.layout
	for _, m := range list {
		if !m.Enabled {
			continue
		}
		one := MieruConfigDiff{
			InboundID:    m.ID,
			DisplayName:  m.DisplayName,
			Deployed:     m.DeployedTransport != "",
			Attrs:        compareMieruAttrs(m),
			DesiredUsers: []string{},
			RemoteUsers:  []string{},
			UsersAdded:   []string{},
			UsersRemove:  []string{},
		}
		// 期望用户读不到时只记原因,不中止整份比对 —— 一台机器上有好几个
		// 入口,其中一个读不出来不该让另外几个也看不成。
		if users, err := s.users.MieruUsersForInbound(ctx, m.ID); err != nil {
			one.Error = "读取期望用户失败:" + err.Error()
		} else {
			for _, u := range users {
				one.DesiredUsers = append(one.DesiredUsers, u.Name)
			}
			sort.Strings(one.DesiredUsers)
		}
		if !one.Deployed {
			// 没下发过就不去问节点:那个实例的服务定义还不存在,
			// 问它只会拿到一句"连不上 socket",而那句话会被读成故障。
			one.Changed = len(one.DesiredUsers) > 0
			out = append(out, one)
			continue
		}
		remote, err := readMieruRemoteUsers(ctx, client, layout, m.ID)
		if err != nil {
			one.Error = err.Error()
			one.Changed = true
			out = append(out, one)
			continue
		}
		one.RemoteUsers = remote
		one.UsersAdded, one.UsersRemove = diffNames(remote, one.DesiredUsers)
		one.Changed = len(one.Attrs) > 0 ||
			len(one.UsersAdded) > 0 || len(one.UsersRemove) > 0
		out = append(out, one)
	}
	return out, nil
}

// compareMieruAttrs 比较期望参数与节点上【已生效】的那几列。
//
// 与 singbox.compareNodeAttrs 同一条道理:配置状态按整份配置的哈希算,
// 而「配置比对」按字段白名单算 —— 只改渲染不改这里,同一个抽屉里
// 上面写着「待下发」、点开比对却说「无变化」,而两个都是我们自己给的。
//
// **凡是进 mita 配置、且会被 MarkMieruDeployed 记下来的字段,都要在这里出现。**
func compareMieruAttrs(m *MieruInbound) []string {
	var out []string
	if m.DeployedTransport != m.Transport {
		out = append(out, fmt.Sprintf("传输层 %s → %s", m.DeployedTransport, m.Transport))
	}
	if m.DeployedMultiplexing != m.Multiplexing {
		out = append(out, fmt.Sprintf("多路复用 %s → %s",
			m.DeployedMultiplexing, m.Multiplexing))
	}
	if m.DeployedMTU != m.MTU {
		out = append(out, fmt.Sprintf("MTU %d → %d", m.DeployedMTU, m.MTU))
	}
	deployed := mieru.PortRange{Start: m.DeployedListenPortStart, End: m.DeployedListenPortEnd}
	if deployed != m.ListenPorts {
		out = append(out, fmt.Sprintf("监听端口段 %s → %s", deployed, m.ListenPorts))
	}
	return out
}

// readMieruRemoteUsers 问一个实例此刻的用户列表。
//
// 走 `mita describe config` 而不是读那份 .pb:.pb 是二进制的 protobuf,
// 面板这一侧要多带一份 mita 的 proto 定义才解得开,而 describe 给的是
// 现成的 JSON。**里面的 password 是空的**(实测:mita 只存 sha256 哈希,
// 而 describe 不回显哈希),所以这条路径不会把凭据带回面板。
func readMieruRemoteUsers(
	ctx context.Context, client *sshx.Client, layout deployment.Layout, id int64,
) ([]string, error) {
	res, err := client.Run(ctx, deployment.MieruDescribeCommand(layout, id))
	if err != nil {
		return nil, fmt.Errorf("读取节点上的 Mieru 配置失败:%w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("读取节点上的 Mieru 配置失败:%s",
			firstLine(res.Stdout+res.Stderr, 200))
	}
	// describe 会在 JSON 前后带上日志行,取第一个 '{' 到最后一个 '}'。
	raw := res.Stdout + res.Stderr
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("节点上的 Mieru 配置解析不出来(输出里没有 JSON)")
	}
	var cfg mieru.ServerConfig
	if err := json.Unmarshal([]byte(raw[start:end+1]), &cfg); err != nil {
		return nil, fmt.Errorf("节点上的 Mieru 配置解析失败:%w", err)
	}
	names := make([]string, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	return names, nil
}

// diffNames 返回「要加的」与「要删的」。
func diffNames(remote, desired []string) (added, removed []string) {
	have := make(map[string]bool, len(remote))
	for _, n := range remote {
		have[n] = true
	}
	want := make(map[string]bool, len(desired))
	for _, n := range desired {
		want[n] = true
	}
	added, removed = []string{}, []string{}
	for _, n := range desired {
		if !have[n] {
			added = append(added, n)
		}
	}
	for _, n := range remote {
		if !want[n] {
			removed = append(removed, n)
		}
	}
	return added, removed
}
