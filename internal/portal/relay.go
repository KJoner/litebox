package portal

import (
	"context"

	"github.com/litebox/litebox/internal/access"
)

// RelayNode 是门户里的一条中转线路。
//
// 与 ExternalNode 同理,这是一份**显式白名单**,不是"把完整记录挑几个清空"
// —— 后者在结构体加字段时会自动泄漏。
//
// 刻意没有的东西:
//   - 中转主机与落地的地址、端口、协议参数 —— 用户拿它做不了任何事;
//   - **落地是谁** —— 那是内部拓扑。知道了没有用处,只会引出
//     「那我能不能直接连落地」;
//   - 任何流量数字 —— 中转主机上跑的是 nginx,它不接 V2Ray API,
//     面板在那台机器上拿不到任何计数。给一个 0 会被读成
//     「我一点都没用过」,那比不显示更糟。
type RelayNode struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	TierName    string `json:"tier_name"`
	TierCode    string `json:"tier_code"`
	// Status 只有对用户有意义的两种:normal / maintenance。
	Status         string `json:"status"`
	PublicRemark   string `json:"public_remark"`
	InSubscription bool   `json:"in_subscription"`
}

// RelayNodes 返回该用户可用的中转线路。
//
// **单独一组返回,既不并进自建节点也不并进外部代理。**
//
// 不并进自建节点:那一组每行都带流量数字,而中转线路没有 —— 混进去只能填 0,
// 与「真的没用过」长得一模一样。
//
// 不并进外部代理:那一组是「买来的成品线路」,而中转线路的凭据是我们发的、
// 落地多半是我们自己的机器。合成一组之后,管理员在门户上看到的分类
// 与他在后台的心智模型对不上。
//
// 可见性走 access.EffectiveRelaysView —— 它已经把「用户在落地上确实有凭据」
// 这一层包含进去了(迁移 0018),这里不再重复判断。
func (q *Querier) RelayNodes(ctx context.Context, proxyUserID int64) ([]RelayNode, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT r.id, r.display_name, t.name, t.code,
		       r.subscription_enabled, r.enabled, r.public_remark,
		       a.status
		  FROM node_relays r
		  JOIN access_tiers t ON t.id = r.access_tier_id
		  JOIN `+access.EffectiveRelaysView+` er ON er.relay_id = r.id
		  JOIN nodes a ON a.id = r.node_id
		 WHERE er.proxy_user_id = ?
		   AND r.deleted_at IS NULL
		   AND a.deleted_at IS NULL
		 ORDER BY r.sort_order, r.id`, proxyUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RelayNode, 0)
	for rows.Next() {
		var (
			n              RelayNode
			subEnabled, up bool
			hostStatus     string
		)
		if err := rows.Scan(&n.ID, &n.DisplayName, &n.TierName, &n.TierCode,
			&subEnabled, &up, &n.PublicRemark, &hostStatus); err != nil {
			return nil, err
		}
		n.InSubscription = subEnabled && up && hostStatus != "DISABLED"
		// 停用与主机禁用对用户都是「维护中」。不区分是哪一种 ——
		// 两者他都做不了任何事,而多一种状态只会多一个要解释的词。
		n.Status = "normal"
		if !n.InSubscription {
			n.Status = "maintenance"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
