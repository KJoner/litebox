package portal

import (
	"context"

	"github.com/litebox/litebox/internal/access"
)

// ProbeMachine 是门户「探针」页上的一台机器:这个用户经等级继承或额外授权,确实能用到它上面的入口。
//
// 它只是事实,不直接序列化给门户 —— 门户看到的卡片在 httpapi 里另外按白名单拼
// (只有展示名、地区、标签、资源百分比与卡片上那几个探测项)。NodeID 只在服务端用来关联
// 资源采样与探测序列,门户里不出现内部标识。
type ProbeMachine struct {
	NodeID        int64
	DisplayName   string
	Status        string
	HostChangedAt string
}

// ProbeMachines 列出这个用户能看到探针数据的机器,按订阅里的先后排。
//
// 范围与「我的节点」同一个来源:sing-box 入口的有效性视图加上 Mieru 入口的有效性视图
// (后者不得借道 user_effective_nodes —— 那一层只认 sing-box,只有 Mieru 入口的机器会对
// 所有人消失,见 CLAUDE.md)。两张视图里已经带着等级继承、整机授权、入口启用与软删除的判断,
// 这里不再自己拼等级条件:四处各写一份迟早分叉,而分叉的表现是用户看得到一台自己用不了的机器。
func (q *Querier) ProbeMachines(ctx context.Context, proxyUserID int64) ([]ProbeMachine, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT n.id, n.display_name, n.status, n.host_changed_at
		  FROM nodes n
		 WHERE n.deleted_at IS NULL
		   AND n.id IN (SELECT node_id FROM `+access.EffectiveNodesView+` WHERE proxy_user_id = ?
		                UNION
		                SELECT node_id FROM `+access.EffectiveMieruInboundsView+` WHERE proxy_user_id = ?)
		 ORDER BY n.sort_order, n.id`, proxyUserID, proxyUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ProbeMachine, 0)
	for rows.Next() {
		var m ProbeMachine
		if err := rows.Scan(&m.NodeID, &m.DisplayName, &m.Status, &m.HostChangedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
