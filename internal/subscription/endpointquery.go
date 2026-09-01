package subscription

import (
	"context"
	"database/sql"
	"strings"
)

// endpointsByEntry 读出一批入口的订阅地址条目(V16)。
//
// kind 是 SINGBOX / MIERU / NGINX / REALM;ids 是这一类入口的 id。address_id
// 为 NULL 的条目指向管理 IP(host),用 hostFor 里那台机器的 host 填进去 ——
// endpoint 表里不重复存 host,那会与 nodes.host 分叉。返回 entry_id → 条目列表,
// 已按 sort_order 排好。
//
// 一条 endpoint 都没有的入口不会出现在返回值里 —— 调用方据此回落到旧的
// 「IPv4 + 可选 IPv6」逻辑(Expand 里那一段),保证入口不会因为没配地址而消失。
func (s *Service) endpointsByEntry(
	ctx context.Context, kind string, ids []int64, hostFor map[int64]string,
) (map[int64][]Endpoint, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, kind)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.entry_id, e.address_id,
		       COALESCE(a.family, ''), COALESCE(a.address, ''),
		       e.public_port, e.public_port_end, e.display_name
		  FROM inbound_endpoints e
		  LEFT JOIN node_addresses a ON a.id = e.address_id
		 WHERE e.entry_kind = ? AND e.entry_id IN (`+strings.Join(ph, ",")+`)
		 ORDER BY e.entry_id, e.sort_order, e.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64][]Endpoint)
	for rows.Next() {
		var entryID int64
		var addrID sql.NullInt64
		var fam, addr, name string
		var port, portEnd int
		if err := rows.Scan(&entryID, &addrID, &fam, &addr, &port, &portEnd, &name); err != nil {
			return nil, err
		}
		ep := Endpoint{Port: port, PortEnd: portEnd, NameOverride: name}
		if !addrID.Valid {
			// 管理 IP:族恒为 V4(host 是唯一的管理通道,只走 IPv4)。
			ep.Family = FamilyV4
			ep.Address = hostFor[entryID]
		} else {
			ep.Family = fam
			ep.Address = addr
		}
		out[entryID] = append(out[entryID], ep)
	}
	return out, rows.Err()
}
