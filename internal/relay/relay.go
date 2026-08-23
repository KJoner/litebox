// Package relay 管理中转主机上的 nginx stream 转发规则。
//
// 一条规则 = A 上的一个 nginx stream server:客户端连 A 的某个端口,
// 字节被原样搬到落地 B。**A 不解密、不认证、不计流量** ——
// 客户端与 B 之间的协议(REALITY 握手、SS2022 的 AEAD)完全端到端。
//
// 因此订阅里这是一条独立条目:地址是 A 的,协议参数与凭据是 B 的。
// 而"用户看得到这条线路"必须蕴含"用户在 B 上有凭据",否则他会拿到一个
// 订阅里看得见、连上去握手直接被拒的条目 —— 规则写在数据库视图
// user_effective_relays 里(迁移 0018),谁都不再自己拼条件。
package relay

import (
	"errors"

	"fmt"
	"github.com/litebox/litebox/internal/nodeport"
	"strings"
)

var (
	ErrNotFound = errors.New("转发规则不存在")
	// ErrPortConflict 监听端口与这台机器上已有的东西冲突。
	//
	// **它就是 nodeport.ErrConflict 本身**,与 node.ErrInboundPortConflict
	// 是同一个值 —— 检测统一在那一个包里,各留一个哨兵会让 errors.Is
	// 在跨包传递时静默失配,而失配的表现是 400 变成 500。
	//
	// 检测到就拒绝保存,**不自动挪端口** —— 自动避让会让用户手上那份
	// 订阅静默失效:客户端还连着旧端口,而那里已经没人监听了。
	ErrPortConflict = nodeport.ErrConflict
)

// TargetKind 是落地去向的种类。
type TargetKind string

const (
	// TargetInbound 的落地是一个【入站】而不是一台机器:一台机器上有两个
	// 入站时,"转发到 B"是有歧义的,而歧义的表现是流量进了管理员没打算用的
	// 那个入口(协议、端口、等级都不同),没有任何一层会报错。
	TargetInbound  TargetKind = "INBOUND"
	TargetExternal TargetKind = "EXTERNAL"
)

func ParseTargetKind(raw string) (TargetKind, error) {
	switch TargetKind(raw) {
	case TargetInbound:
		return TargetInbound, nil
	case TargetExternal:
		return TargetExternal, nil
	default:
		return "", fmt.Errorf("未知的落地去向 %q", raw)
	}
}

// Relay 是一条转发规则。
//
// 刻意只有 DisplayName 而没有内部名称:它会进订阅与门户,
// 而订阅是发到用户设备上的东西 —— 结构体里根本不存在内部名称,
// 就不可能有哪条代码路径不小心把它写进去。
type Relay struct {
	ID     int64 `json:"id"`
	NodeID int64 `json:"node_id"`
	// NodeName 与 NodeDisplayName 供管理页面展示,不进订阅。
	NodeName string `json:"node_name"`

	DisplayName string `json:"display_name"`

	// ListenPort 是 nginx 在 A 上实际监听的端口;
	// PublicPort 是客户端连接的公网端口,0 表示跟随 ListenPort。
	//
	// **0 要原样留着,不在这里解析。** 解析放在订阅生成时(EffectivePublicPort)
	// —— 写死成当时的监听端口之后,管理员再改监听端口,订阅条目会继续停在
	// 旧端口上,而他当初看到的是一个空输入框。
	ListenPort int `json:"listen_port"`
	PublicPort int `json:"public_port"`

	TargetKind       TargetKind `json:"target_kind"`
	TargetInboundID  int64      `json:"target_inbound_id"`
	TargetExternalID int64      `json:"target_external_id"`
	// TargetName 是落地的展示名,只给管理页面看。
	TargetName string `json:"target_name"`
	// TargetReady 表示落地当前确实能给出可用的协议参数。
	// 自建节点要求已成功部署过;外部代理要求未删除。
	//
	// 不用它拦住保存 —— 先建规则再部署落地是完全正常的顺序。
	// 但界面上必须显示出来,否则管理员会对着一条"配好了却不在订阅里"
	// 的线路找半天。
	TargetReady bool `json:"target_ready"`

	AccessTierID    int64  `json:"access_tier_id"`
	AccessTierCode  string `json:"access_tier_code"`
	AccessTierName  string `json:"access_tier_name"`
	AccessTierLevel int    `json:"access_tier_level"`

	SortOrder           int    `json:"sort_order"`
	SubscriptionEnabled bool   `json:"subscription_enabled"`
	PublicRemark        string `json:"public_remark"`
	// Enabled 为假时 nginx 里不渲染这个 server 块 —— 与软删除不同,
	// 配置还留着,重新打开不用重配等级与排序。
	Enabled bool `json:"enabled"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EffectivePublicPort 是客户端实际要连的端口。
func (r Relay) EffectivePublicPort() int {
	if r.PublicPort != 0 {
		return r.PublicPort
	}
	return r.ListenPort
}

// validateName 归一化并校验展示名称。
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("线路名称不能为空")
	}
	if len([]rune(name)) > 64 {
		return "", errors.New("线路名称不能超过 64 个字符")
	}
	// 换行与控制字符会把 URI 列表的行数搞乱,客户端解析出一个残缺条目 ——
	// 与外部代理的上游名称清洗是同一条道理。
	if strings.ContainsAny(name, "\r\n\t") {
		return "", errors.New("线路名称不能包含换行或制表符")
	}
	return name, nil
}

func validatePort(port int, what string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s必须在 1-65535 之间", what)
	}
	return nil
}
