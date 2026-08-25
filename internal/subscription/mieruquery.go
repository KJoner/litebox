package subscription

import (
	"context"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/mieru"
)

// mieruFor 返回该用户订阅中应当出现的 Mieru 入口。
//
// 过滤条件与 nodesFor 逐条对应,而且**必须逐条对应** —— 两处分叉的表现是
// 一台机器进了维护、它的 sing-box 入口从订阅里消失了,而 Mieru 入口
// 还留着继续被使用。
//
// 【参数取 deployed_* 而不是期望值】:管理员改传输层或端口段到部署成功之间
// 存在一个窗口 —— 可能二十秒,也可能是部署失败之后的永远。按期望值渲染的话,
// 这个窗口里用户拉到的是一批还没人监听的端口,而数据库、节点、面板三方
// 都是"对的",只有订阅站在中间说了假话。
//
// deployed_transport 非空同时也是「这个入口真的上过节点」的判据 ——
// 节点级的部署状态答不了这个问题:一台部署过很多次的机器上,
// 刚加的这个入口仍然还不存在。
func (s *Service) mieruFor(ctx context.Context, userID int64) ([]MieruNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.sort_order, n.id, m.sort_order, m.id,
		       m.display_name, n.host, n.sub_ipv4_address, n.ipv6_address,
		       m.public_port_start, m.public_port_end,
		       m.deployed_listen_port_start, m.deployed_listen_port_end,
		       m.ipv6_public_port_start, m.ipv6_public_port_end,
		       m.ipv6_enabled, m.ipv6_display_name,
		       m.deployed_transport, m.deployed_multiplexing, m.deployed_mtu
		  FROM node_mieru_inbounds m
		  JOIN nodes n ON n.id = m.node_id
		  JOIN `+access.EffectiveMieruInboundsView+` em ON em.mieru_inbound_id = m.id
		 WHERE em.proxy_user_id = ?
		   AND m.deleted_at IS NULL
		   AND n.deleted_at IS NULL
		   AND n.status != 'DISABLED'
		   AND n.subscription_enabled = 1
		   AND m.subscription_enabled = 1
		   AND m.enabled = 1
		   AND m.deployed_transport != ''
		 ORDER BY n.sort_order, n.id, m.sort_order, m.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	physical := make([]PhysicalMieru, 0)
	for rows.Next() {
		var p PhysicalMieru
		var deployedListen mieru.PortRange
		p.Order.Kind = OrderMieru
		if err := rows.Scan(&p.Order.NodeSort, &p.Order.NodeID, &p.Order.Sort, &p.Order.ID,
			&p.DisplayName, &p.Host, &p.SubIPv4Address, &p.IPv6Address,
			&p.Ports.Start, &p.Ports.End,
			&deployedListen.Start, &deployedListen.End,
			&p.IPv6Ports.Start, &p.IPv6Ports.End,
			&p.IPv6Enabled, &p.IPv6Name,
			&p.Transport, &p.Multiplexing, &p.MTU); err != nil {
			return nil, err
		}
		// 公网端口段留空表示「跟随监听段」,在这里解析而不是写库时固化 ——
		// 固化之后管理员再改监听段,订阅条目会继续停在旧号码上,
		// 而他当初看到的是两个空输入框。
		//
		// 回落到【已生效的】监听段:回落到期望值会在改端口的窗口里
		// 下发一批还没人监听的号码。
		if p.Ports.Empty() {
			p.Ports = deployedListen
		}
		physical = append(physical, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ExpandAllMieru(physical), nil
}
