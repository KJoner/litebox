package singbox

import (
	"fmt"
	"sort"
	"strings"
)

// UserDiff 描述两份配置之间用户列表的差异。
type UserDiff struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	UUIDReset []string `json:"uuid_reset"`
}

// Empty 表示用户列表没有变化。
func (d UserDiff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.UUIDReset) == 0
}

// Diff 是两份配置之间的完整差异。
type Diff struct {
	Changed  bool     `json:"changed"`
	Users    UserDiff `json:"users"`
	NodeAttr []string `json:"node_attr"`
	Summary  string   `json:"summary"`
}

// Compare 比较两份配置并给出可读差异。
//
// 刻意只比较业务上有意义的字段,而不是做通用的 JSON 文本 diff:
// 后者会把字段顺序、缩进之类的噪声也报出来,而管理员真正关心的是
// "哪些用户会被加进来、哪些会掉线、哪些凭据被换掉了"。
func Compare(old, new Config) Diff {
	var d Diff

	oldUsers := userMap(old)
	newUsers := userMap(new)

	for code, uuid := range newUsers {
		oldUUID, existed := oldUsers[code]
		switch {
		case !existed:
			d.Users.Added = append(d.Users.Added, code)
		case oldUUID != uuid:
			d.Users.UUIDReset = append(d.Users.UUIDReset, code)
		}
	}
	for code := range oldUsers {
		if _, still := newUsers[code]; !still {
			d.Users.Removed = append(d.Users.Removed, code)
		}
	}
	sort.Strings(d.Users.Added)
	sort.Strings(d.Users.Removed)
	sort.Strings(d.Users.UUIDReset)

	d.NodeAttr = compareNodeAttrs(old, new)
	d.Changed = !d.Users.Empty() || len(d.NodeAttr) > 0
	d.Summary = summarize(d)
	return d
}

func userMap(cfg Config) map[string]string {
	users := make(map[string]string)
	if len(cfg.Inbounds) == 0 {
		return users
	}
	for _, u := range cfg.Inbounds[0].Users {
		users[u.Name] = u.UUID
	}
	return users
}

func compareNodeAttrs(old, new Config) []string {
	var changes []string
	if len(old.Inbounds) == 0 || len(new.Inbounds) == 0 {
		return changes
	}
	oldIn, newIn := old.Inbounds[0], new.Inbounds[0]

	if oldIn.ListenPort != newIn.ListenPort {
		// 写明"主机":节点上还有一个公网代理端口,它不进节点配置,这里的差异与它无关。
		changes = append(changes, fmt.Sprintf("主机代理端口 %d → %d", oldIn.ListenPort, newIn.ListenPort))
	}
	if oldIn.TLS.ServerName != newIn.TLS.ServerName {
		changes = append(changes, fmt.Sprintf("握手目标 %s → %s",
			oldIn.TLS.ServerName, newIn.TLS.ServerName))
	}
	if oldIn.TLS.Reality.Handshake.ServerPort != newIn.TLS.Reality.Handshake.ServerPort {
		changes = append(changes, fmt.Sprintf("握手端口 %d → %d",
			oldIn.TLS.Reality.Handshake.ServerPort, newIn.TLS.Reality.Handshake.ServerPort))
	}
	// 私钥内容不出现在 diff 里,只提示"已更换"。
	if oldIn.TLS.Reality.PrivateKey != newIn.TLS.Reality.PrivateKey {
		changes = append(changes, "REALITY 私钥已更换(所有客户端需更新公钥)")
	}
	if strings.Join(oldIn.TLS.Reality.ShortID, ",") != strings.Join(newIn.TLS.Reality.ShortID, ",") {
		changes = append(changes, "short_id 已更换(所有客户端需更新)")
	}
	if old.Experimental.V2RayAPI.Listen != new.Experimental.V2RayAPI.Listen {
		changes = append(changes, fmt.Sprintf("V2Ray API 监听 %s → %s",
			old.Experimental.V2RayAPI.Listen, new.Experimental.V2RayAPI.Listen))
	}
	return changes
}

func summarize(d Diff) string {
	if !d.Changed {
		return "配置无变化"
	}
	var parts []string
	if n := len(d.Users.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d 个用户(%s)", n, strings.Join(d.Users.Added, ", ")))
	}
	if n := len(d.Users.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("移除 %d 个用户(%s),其 UUID 重启后立即失效",
			n, strings.Join(d.Users.Removed, ", ")))
	}
	if n := len(d.Users.UUIDReset); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个用户更换了 UUID(%s)",
			n, strings.Join(d.Users.UUIDReset, ", ")))
	}
	parts = append(parts, d.NodeAttr...)
	return strings.Join(parts, ";")
}
