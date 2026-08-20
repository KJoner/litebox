package node

import "fmt"

// Role 是节点角色。
//
// 两者的差别不是"配置不同",而是"这台机器上跑什么":
// LANDING 上跑 sing-box 服务,RELAY 上只跑 nginx。因此 RELAY 不渲染
// sing-box 配置、不同步流量、不做配置 diff,它的一半列本来就没有意义。
type Role string

const (
	// RoleLanding 落地节点。V7 之前的全部节点,行为一个字节不变。
	RoleLanding Role = "LANDING"
	// RoleRelay 纯中转机:不跑 sing-box 服务,只跑 nginx stream。
	//
	// 它上面仍然有一份 sing-box 二进制,但不安装服务、不常驻 ——
	// 部署健康检查必须包含真实拨测(本项目第一条铁律),而拨测需要一个客户端。
	// 28MB 磁盘换一次真实的端到端验证,内存开销是零。
	RoleRelay Role = "RELAY"
)

// ParseRole 解析角色,空串按 LANDING —— 与迁移里那一列的默认值一致。
func ParseRole(raw string) (Role, error) {
	switch Role(raw) {
	case "", RoleLanding:
		return RoleLanding, nil
	case RoleRelay:
		return RoleRelay, nil
	default:
		return "", fmt.Errorf("未知的节点角色 %q", raw)
	}
}

// IsRelay 表示这台机器上不跑 sing-box 服务。
func (r Role) IsRelay() bool { return r == RoleRelay }

// Label 是给人看的名字。
func (r Role) Label() string {
	if r == RoleRelay {
		return "中转"
	}
	return "落地"
}

// ChainTargetKind 是链式出站的去向种类。空串表示直连。
//
// V8 起 NODE 改称 INBOUND:落地是【一个入站】而不是一台机器。
// 改名而不是沿用旧值,是为了让每一处调用点都必须被重新看过一遍 ——
// 沿用的话,那些"按节点解析落地参数"的代码会继续编译通过,
// 而它取到的是那台机器上恰好排在前面的那个入站。
type ChainTargetKind string

const (
	ChainTargetNone     ChainTargetKind = ""
	ChainTargetInbound  ChainTargetKind = "INBOUND"
	ChainTargetExternal ChainTargetKind = "EXTERNAL"
)

// ParseChainTargetKind 解析链式去向种类。
func ParseChainTargetKind(raw string) (ChainTargetKind, error) {
	switch ChainTargetKind(raw) {
	case ChainTargetNone:
		return ChainTargetNone, nil
	case ChainTargetInbound:
		return ChainTargetInbound, nil
	case ChainTargetExternal:
		return ChainTargetExternal, nil
	default:
		return "", fmt.Errorf("未知的链式去向 %q", raw)
	}
}

// Enabled 表示这个入站的出站指向别处,而不是 direct。
func (k ChainTargetKind) Enabled() bool { return k != ChainTargetNone }
