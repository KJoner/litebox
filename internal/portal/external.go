package portal

import (
	"context"
	"time"
)

// ExternalNode 是门户里的一条外部代理。
//
// 这是一份**显式白名单**,与 Node 同理:字段是一个一个加进来的,
// 不是"把完整记录挑几个清空" —— 后者在结构体加字段时会自动泄漏。
//
// 刻意没有的东西:
//   - 服务器地址与端口 —— 用户拿它做不了任何事;
//   - **来源(哪个机场)** —— 知道了没有用处,只会引出「那我能不能自己去买」
//     和「你加价了多少」;
//   - 任何流量数字 —— 外部代理的流量走的是上游的服务器,面板统计不到。
//     给一个 0 会被读成「我一点都没用过」,那比不显示更糟。
type ExternalNode struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	TierName    string `json:"tier_name"`
	TierCode    string `json:"tier_code"`
	// Status 只有三种对用户有意义的取值:normal / maintenance / disabled。
	Status             string `json:"status"`
	PublicRemark       string `json:"public_remark"`
	MaintenanceMessage string `json:"maintenance_message"`
	InSubscription     bool   `json:"in_subscription"`
}

// ExternalNodes 返回该用户可用的外部代理。
//
// 与自建节点分成两个列表返回,而不是混进同一个数组:混在一起的话
// 外部代理那几行的流量字段只能填 0,而 0 与「真的没用过」长得一模一样。
// 分开之后前端把它们渲染成两块,那一块干脆不出现流量列。
func (q *Querier) ExternalNodes(ctx context.Context, proxyUserID int64) ([]ExternalNode, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := q.db.QueryContext(ctx, `
		SELECT p.id, p.display_name, p.display_name_override, COALESCE(src.name_prefix, ''),
		       t.name, t.code, p.status, p.subscription_enabled,
		       p.public_remark, p.maintenance_message,
		       CASE WHEN p.expires_at IS NOT NULL AND p.expires_at != '' AND p.expires_at <= ?
		            THEN 1 ELSE 0 END AS self_expired,
		       CASE WHEN p.source_id IS NULL THEN 0
		            WHEN src.deleted_at IS NOT NULL OR src.enabled = 0 THEN 1
		            WHEN COALESCE(NULLIF(src.expires_at, ''), NULLIF(src.upstream_expires_at, ''))
		                 IS NOT NULL
		             AND COALESCE(NULLIF(src.expires_at, ''), NULLIF(src.upstream_expires_at, ''))
		                 <= ? THEN 1
		            ELSE 0 END AS source_down
		  FROM external_proxies p
		  JOIN access_tiers t ON t.id = p.access_tier_id
		  JOIN user_effective_external_proxies ep ON ep.external_proxy_id = p.id
		  LEFT JOIN proxy_sources src ON src.id = p.source_id
		 WHERE ep.proxy_user_id = ? AND p.deleted_at IS NULL AND p.status != 'EXCLUDED'
		 ORDER BY p.sort_order, p.id`, now, now, proxyUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ExternalNode, 0)
	for rows.Next() {
		var (
			n                             ExternalNode
			displayName, override, prefix string
			status                        string
			subEnabled                    bool
			selfExpired, srcDown          bool
		)
		if err := rows.Scan(&n.ID, &displayName, &override, &prefix,
			&n.TierName, &n.TierCode, &status, &subEnabled,
			&n.PublicRemark, &n.MaintenanceMessage, &selfExpired, &srcDown); err != nil {
			return nil, err
		}
		n.DisplayName = override
		if n.DisplayName == "" {
			n.DisplayName = prefix + displayName
		}
		n.InSubscription = subEnabled && status == "ACTIVE" && !selfExpired && !srcDown

		// 到期与源下线对用户都是「维护中」。不告诉他「机场到期了」——
		// 那是管理员的事,而且会引出他不该关心的问题。
		switch {
		case status == "DISABLED":
			n.Status = "disabled"
		case !n.InSubscription:
			n.Status = "maintenance"
		default:
			n.Status = "normal"
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
