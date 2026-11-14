package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/chaingraph"
)

// V16:一台机器多个订阅地址(host 之外),每个入口按地址各配端口与订阅名。
//
// node_addresses 是地址池,inbound_endpoints 是「哪个入口可以从哪几条地址连进来」。
// 它们本身一个字节都不进**这台**机器的配置 —— 改它们既不置 SSHChanged 也不置 NeedsDeploy。
//
// **v23.3 起地址条目同时是链路的目的地址**:链式出站与转发规则精确地选目标入口的某一条条目(稳定 id)。
// 于是两件事变了:条目按 id 增量保存(改名、排序、原样保存都不能换 id),被选中的条目不能删、
// 它所在的地址池项也不能删(删除会级联掉它);被选中的条目改了地址或端口,选它的上游要重新下发。
// 「可连接地址」与「进不进用户订阅」是两回事:条目在不在订阅里仍由入口自己的规则决定
// (指定地址转发不进订阅),链路选它与订阅无关。

// AddrFamilyV4 / AddrFamilyV6 是地址族。
const (
	AddrFamilyV4 = "V4"
	AddrFamilyV6 = "V6"
)

// EndpointKind 是订阅地址条目挂在哪一类入口上。与订阅生成侧
// (inbound_endpoints.entry_kind)取值一致。
const (
	EndpointKindSingBox = "SINGBOX"
	EndpointKindMieru   = "MIERU"
	EndpointKindNginx   = "NGINX"
	EndpointKindRealm   = "REALM"
)

// NodeAddress 是地址池里的一条(host 之外的额外 IPv4 / IPv6)。
type NodeAddress struct {
	ID        int64  `json:"id"`
	NodeID    int64  `json:"node_id"`
	Family    string `json:"family"`
	Address   string `json:"address"`
	SortOrder int    `json:"sort_order"`
}

// AddressInput 是保存地址池时前端传来的一条。ID 为 0 表示新增。
type AddressInput struct {
	ID      int64  `json:"id"`
	Family  string `json:"family"`
	Address string `json:"address"`
}

// Endpoint 是一个入口在订阅里下发的一条地址。
type Endpoint struct {
	ID            int64  `json:"id"`
	AddressID     *int64 `json:"address_id"` // nil = 管理 IP(host)
	PublicPort    int    `json:"public_port"`
	PublicPortEnd int    `json:"public_port_end"`
	DisplayName   string `json:"display_name"`
	SortOrder     int    `json:"sort_order"`
}

// EndpointInput 是保存入口地址条目时前端传来的一条。ID 为 0 表示新增(v23.3)。
type EndpointInput struct {
	ID            int64  `json:"id"`
	AddressID     *int64 `json:"address_id"`
	PublicPort    int    `json:"public_port"`
	PublicPortEnd int    `json:"public_port_end"`
	DisplayName   string `json:"display_name"`
}

// EntryNode 解析一个入口(订阅地址条目挂在它上面)属于哪台机器,以及它是不是
// Mieru(端口要按段校验)。找不到就报错 —— 保存地址条目前要先确认入口真的存在,
// 而且地址只能指向它自己那台机器。
func (s *Store) EntryNode(ctx context.Context, kind string, entryID int64) (nodeID int64, isMieru bool, err error) {
	switch kind {
	case EndpointKindSingBox:
		err = s.db.QueryRowContext(ctx,
			`SELECT node_id FROM node_inbounds WHERE id = ? AND deleted_at IS NULL`, entryID).Scan(&nodeID)
	case EndpointKindMieru:
		isMieru = true
		err = s.db.QueryRowContext(ctx,
			`SELECT node_id FROM node_mieru_inbounds WHERE id = ? AND deleted_at IS NULL`, entryID).Scan(&nodeID)
	case EndpointKindNginx, EndpointKindRealm:
		var engine string
		err = s.db.QueryRowContext(ctx,
			`SELECT node_id, engine FROM node_relays WHERE id = ? AND deleted_at IS NULL`,
			entryID).Scan(&nodeID, &engine)
		if err == nil && engine != kind {
			// 引擎与种类对不上 —— 订阅生成侧按引擎查条目,种类填错会让这些条目
			// 谁都查不到,而这里能一眼拦住。
			return 0, false, fmt.Errorf("这条转发规则的引擎是 %s,不是 %s", engine, kind)
		}
	default:
		return 0, false, fmt.Errorf("未知的入口种类 %q", kind)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("入口 %s#%d 不存在", kind, entryID)
	}
	return nodeID, isMieru, err
}

// AddressesForNode 读出一台机器的地址池,按 sort_order 排。
func (s *Store) AddressesForNode(ctx context.Context, nodeID int64) ([]NodeAddress, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, node_id, family, address, sort_order
		   FROM node_addresses WHERE node_id = ? ORDER BY sort_order, id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeAddress, 0)
	for rows.Next() {
		var a NodeAddress
		if err := rows.Scan(&a.ID, &a.NodeID, &a.Family, &a.Address, &a.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReplaceAddresses 用前端传来的整份列表替换一台机器的地址池,按 id 做增量:
// 带 id 的更新、无 id 的新增、列表里没有的删除。返回地址真的变了、而且被链路选中的那些地址条目 ——
// 选它们的上游要重新下发(调用方传给 PropagateEndpointChange)。
//
// **删除是有连带的**:inbound_endpoints.address_id 挂了 ON DELETE CASCADE ——
// 删掉一个地址,引用它的入口地址条目一起消失,那条地址就从所有订阅里撤下来。
// 所以不能整表删了重插(那会把全部入口条目连带清掉再让新地址拿到新 id、条目变成孤儿),必须保住存量 id。
// 被链路选中的条目会被这样连带删掉时,**整个保存拒绝**并说清是谁在用(v23.3)。
//
// **顺便把 nodes.sub_ipv4_address / ipv6_address 当成地址池首条的镜像维护起来。**
// 这两列不再由节点表单直接编辑(V16),只剩列表与详情里显示的 subscription_host、以及没有地址条目的入口在
// 订阅里的回落在读它们。v23.3 起链路的目的地址不再读这一栏 —— 地址池怎么重排,已经选定的链路目的地址都不变。
func (s *Store) ReplaceAddresses(ctx context.Context, nodeID int64, inputs []AddressInput) (changedEndpoints []int64, err error) {
	norm := make([]AddressInput, len(inputs))
	seenVal := make(map[string]bool, len(inputs))
	for i, in := range inputs {
		addr, e := normalizePoolAddress(in.Family, in.Address)
		if e != nil {
			return nil, e
		}
		key := in.Family + "|" + addr
		if seenVal[key] {
			return nil, fmt.Errorf("地址 %s 重复了", addr)
		}
		seenVal[key] = true
		norm[i] = AddressInput{ID: in.ID, Family: in.Family, Address: addr}
	}

	existing := map[int64]NodeAddress{}
	cur, err := s.AddressesForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, a := range cur {
		existing[a.ID] = a
	}
	keptIDs := map[int64]bool{}
	var changedAddrIDs []int64
	for _, in := range norm {
		if old, ok := existing[in.ID]; in.ID > 0 && ok {
			keptIDs[in.ID] = true
			if old.Address != in.Address || old.Family != in.Family {
				changedAddrIDs = append(changedAddrIDs, in.ID)
			}
		}
	}
	// 要删的地址会连带删掉引用它的条目:有条目被链路选中就拒绝。
	var removed []int64
	for id := range existing {
		if !keptIDs[id] {
			removed = append(removed, id)
		}
	}
	if refs, err := s.referrersOfAddresses(ctx, removed); err != nil {
		return nil, err
	} else if len(refs) > 0 {
		return nil, &ReferencedError{What: "要删除的地址上的地址条目", Referrers: refs}
	}
	if changedEndpoints, err = s.referencedEndpointsOfAddresses(ctx, changedAddrIDs); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	for order, in := range norm {
		if in.ID > 0 && keptIDs[in.ID] {
			if _, err := tx.ExecContext(ctx,
				`UPDATE node_addresses SET family = ?, address = ?, sort_order = ?, updated_at = ?
				  WHERE id = ? AND node_id = ?`,
				in.Family, in.Address, order, now, in.ID, nodeID); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			nodeID, in.Family, in.Address, order, now, now); err != nil {
			return nil, err
		}
	}
	for _, id := range removed {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_addresses WHERE id = ? AND node_id = ?`, id, nodeID); err != nil {
			return nil, err
		}
	}

	// 把地址池首条 V4 / V6 写回镜像列(没有则清空)。只影响显示与没有条目的入口在订阅里的回落。
	newV4 := firstPoolAddress(norm, AddrFamilyV4)
	newV6 := firstPoolAddress(norm, AddrFamilyV6)
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET sub_ipv4_address = ?, ipv6_address = ?, updated_at = ? WHERE id = ?
		    AND (sub_ipv4_address != ? OR ipv6_address != ?)`,
		newV4, newV6, now, nodeID, newV4, newV6); err != nil {
		return nil, err
	}
	return changedEndpoints, tx.Commit()
}

// referrersOfAddresses 列出选中了「挂在这些地址上的条目」的入口。
func (s *Store) referrersOfAddresses(ctx context.Context, addressIDs []int64) ([]chaingraph.Referrer, error) {
	eps, err := s.endpointsOnAddresses(ctx, addressIDs)
	if err != nil {
		return nil, err
	}
	var out []chaingraph.Referrer
	for _, id := range eps {
		refs, err := chaingraph.EndpointReferrers(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func (s *Store) referencedEndpointsOfAddresses(ctx context.Context, addressIDs []int64) ([]int64, error) {
	eps, err := s.endpointsOnAddresses(ctx, addressIDs)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, id := range eps {
		refs, err := chaingraph.EndpointReferrers(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Store) endpointsOnAddresses(ctx context.Context, addressIDs []int64) ([]int64, error) {
	var out []int64
	for _, aid := range addressIDs {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM inbound_endpoints WHERE address_id = ?`, aid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// firstPoolAddress 取归一化后列表里某个族的第一条(按列表顺序,即 sort_order)。
func firstPoolAddress(list []AddressInput, family string) string {
	for _, a := range list {
		if a.Family == family {
			return a.Address
		}
	}
	return ""
}

func normalizePoolAddress(family, raw string) (string, error) {
	switch family {
	case AddrFamilyV4:
		addr, err := normalizeIPv4(raw)
		if err != nil {
			return "", fmt.Errorf("额外 IPv4 地址:%w", err)
		}
		if addr == "" {
			return "", errors.New("额外 IPv4 地址不能为空")
		}
		return addr, nil
	case AddrFamilyV6:
		addr, err := normalizeIPv6(raw)
		if err != nil {
			return "", err
		}
		if addr == "" {
			return "", errors.New("IPv6 地址不能为空")
		}
		return addr, nil
	default:
		return "", fmt.Errorf("未知的地址族 %q", family)
	}
}

// EndpointsForEntry 读出一个入口的订阅地址条目,按 sort_order 排。
func (s *Store) EndpointsForEntry(ctx context.Context, kind string, entryID int64) ([]Endpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, address_id, public_port, public_port_end, display_name, sort_order
		   FROM inbound_endpoints WHERE entry_kind = ? AND entry_id = ?
		  ORDER BY sort_order, id`, kind, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Endpoint, 0)
	for rows.Next() {
		var e Endpoint
		var addrID sql.NullInt64
		if err := rows.Scan(&e.ID, &addrID, &e.PublicPort, &e.PublicPortEnd,
			&e.DisplayName, &e.SortOrder); err != nil {
			return nil, err
		}
		if addrID.Valid {
			e.AddressID = &addrID.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ErrEndpointsNeedIDs 旧版页面提交的地址条目没有带 id,而这个入口的条目正被链路选中、又对不上唯一一条。
var ErrEndpointsNeedIDs = errors.New("这个入口的地址条目正被链路选作目的地址,而这次提交没有带上条目编号、也对不上唯一一条 —— 请刷新页面后再保存")

// ReplaceEndpoints 用前端传来的整份列表保存一个入口的地址条目,**按 id 增量**(v23.3):
// 带 id 的更新(改名、排序、改地址或端口都不换 id)、无 id 的新增、列表里没有的删除。
// 在此之前是整表删了重插 —— 那时条目没有任何引用者,id 不承载语义;现在链路按 id 选目的地址,
// 重插一次就会让所有选了它的链路指向一个不存在的条目。
//
// 返回地址或端口真的变了、而且被链路选中的那些条目(调用方传给 PropagateEndpointChange):
// 选它们的上游要重新下发;没被选中的、只改了名字或顺序的,什么都不用下发。
//
// 旧版页面不带 id:能把每一条被选中的条目按 (地址, 端口段) 唯一对上就沿用它的 id,对不上就拒绝(ErrEndpointsNeedIDs),
// 不静默重建。被选中的条目不能删(ReferencedError)。isMieru 为真时端口按段校验(起点、终点),否则是单端口。
func (s *Store) ReplaceEndpoints(
	ctx context.Context, nodeID int64, kind string, entryID int64,
	inputs []EndpointInput, isMieru bool,
) (changedReferenced []int64, err error) {
	// 地址池:endpoint 只能指向本机的地址(或 host=NULL)。指向别的机器的
	// 地址是一条不属于这台机器的条目 —— 在这里拦住。
	owned := make(map[int64]bool)
	addrs, err := s.AddressesForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		owned[a.ID] = true
	}
	for i, in := range inputs {
		if in.AddressID != nil && !owned[*in.AddressID] {
			return nil, fmt.Errorf("第 %d 条地址不属于这台机器", i+1)
		}
		if err := validateEndpointPorts(in, isMieru); err != nil {
			return nil, fmt.Errorf("第 %d 条:%w", i+1, err)
		}
	}

	existing, err := s.EndpointsForEntry(ctx, kind, entryID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]Endpoint{}
	referenced := map[int64][]chaingraph.Referrer{}
	for _, e := range existing {
		byID[e.ID] = e
		refs, err := chaingraph.EndpointReferrers(ctx, s.db, e.ID)
		if err != nil {
			return nil, err
		}
		if len(refs) > 0 {
			referenced[e.ID] = refs
		}
	}

	hasIDs := false
	for _, in := range inputs {
		if in.ID != 0 {
			hasIDs = true
		}
	}
	work := make([]EndpointInput, len(inputs))
	copy(work, inputs)
	if !hasIDs && len(existing) > 0 {
		// 旧版页面:按 (地址, 端口段) 对回原来的 id,一对一,对上几条算几条。
		used := map[int64]bool{}
		for i, in := range work {
			match := int64(0)
			for _, e := range existing {
				if used[e.ID] || !sameAddrPtr(e.AddressID, in.AddressID) ||
					e.PublicPort != in.PublicPort || e.PublicPortEnd != in.PublicPortEnd {
					continue
				}
				if match != 0 {
					match = -1 // 歧义
					break
				}
				match = e.ID
			}
			if match > 0 {
				work[i].ID = match
				used[match] = true
			}
		}
		for id := range referenced {
			if !used[id] {
				return nil, ErrEndpointsNeedIDs
			}
		}
	}

	seen := map[int64]bool{}
	for i, in := range work {
		if in.ID == 0 {
			continue
		}
		if _, ok := byID[in.ID]; !ok {
			return nil, fmt.Errorf("第 %d 条的编号 #%d 不是这个入口的地址条目(请刷新页面)", i+1, in.ID)
		}
		if seen[in.ID] {
			return nil, fmt.Errorf("地址条目 #%d 重复提交了", in.ID)
		}
		seen[in.ID] = true
	}
	var removed []int64
	var blocked []chaingraph.Referrer
	for _, e := range existing {
		if !seen[e.ID] {
			removed = append(removed, e.ID)
			blocked = append(blocked, referenced[e.ID]...)
		}
	}
	if len(blocked) > 0 {
		return nil, &ReferencedError{What: "要删除的地址条目", Referrers: blocked}
	}
	for _, in := range work {
		old, ok := byID[in.ID]
		if !ok || len(referenced[in.ID]) == 0 {
			continue
		}
		if !sameAddrPtr(old.AddressID, in.AddressID) || old.PublicPort != in.PublicPort || old.PublicPortEnd != in.PublicPortEnd {
			changedReferenced = append(changedReferenced, in.ID)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for order, in := range work {
		var addrID any
		if in.AddressID != nil {
			addrID = *in.AddressID
		}
		if in.ID != 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE inbound_endpoints SET address_id = ?, public_port = ?, public_port_end = ?,
				       display_name = ?, sort_order = ?, updated_at = ?
				 WHERE id = ? AND entry_kind = ? AND entry_id = ?`,
				addrID, in.PublicPort, in.PublicPortEnd, in.DisplayName, order, now, in.ID, kind, entryID); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inbound_endpoints
			   (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
			    display_name, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, kind, entryID, addrID, in.PublicPort, in.PublicPortEnd,
			in.DisplayName, order, now, now); err != nil {
			return nil, err
		}
	}
	for _, id := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbound_endpoints WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return changedReferenced, tx.Commit()
}

func sameAddrPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func validateEndpointPorts(in EndpointInput, isMieru bool) error {
	if in.PublicPort < 0 || in.PublicPort > 65535 ||
		in.PublicPortEnd < 0 || in.PublicPortEnd > 65535 {
		return errors.New("端口必须在 0–65535")
	}
	if !isMieru {
		// 单端口类:PortEnd 无意义,必须为 0,免得前端误填后订阅端口对不上。
		if in.PublicPortEnd != 0 {
			return errors.New("这类入口是单端口,不该有端口段终点")
		}
		return nil
	}
	// Mieru:段。0/0 = 跟随;否则起点终点都要给,且终点 ≥ 起点。
	if in.PublicPort == 0 && in.PublicPortEnd == 0 {
		return nil
	}
	if in.PublicPort == 0 || in.PublicPortEnd == 0 {
		return errors.New("端口段要么两端都留空(跟随监听段),要么都填")
	}
	if in.PublicPortEnd < in.PublicPort {
		return errors.New("端口段终点不能小于起点")
	}
	return nil
}
