package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// V16:一台机器多个订阅地址(host 之外),每个入口按地址各配端口与订阅名。
//
// node_addresses 是地址池,inbound_endpoints 是「哪个入口下发哪几条地址」。
// 两者都只影响订阅内容,一个字节都不进节点配置 —— 与 IPv6 那一套同类,
// 改它们既不置 SSHChanged 也不置 NeedsDeploy。

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

// EndpointInput 是保存入口地址条目时前端传来的一条。
type EndpointInput struct {
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
// 带 id 的更新、无 id 的新增、列表里没有的删除。返回值表示这次改动是否动了
// 中转/链式落地要用的那个地址(见下面对镜像列的说明)。
//
// **删除是有连带的**:inbound_endpoints.address_id 挂了 ON DELETE CASCADE ——
// 删掉一个地址,引用它的入口地址条目一起消失,那条地址就从所有订阅里撤下来。
// 这正是「把某个额外地址从机器上拿掉」的语义。所以不能整表删了重插(那会把
// 全部入口条目连带清掉再让新地址拿到新 id、条目变成孤儿),必须保住存量 id。
//
// **顺便把 nodes.sub_ipv4_address / ipv6_address 当成地址池首条的镜像维护起来。**
// 这两列不再由节点表单直接编辑(V16),但仍是一堆地方读的"这台机器对外的
// 主地址":链式/中转的落地(chainInboundTarget → SubscriptionIPv4)、列表与
// 详情里显示的 subscription_host。让地址池的第一条 V4 / V6 自动写回这两列,
// 这些读法就一个字都不用改,而且永远与池子一致。镜像变了要传播中转脏标记 ——
// 落地的对外落脚点换了地址,下游中转机的 proxy_pass 目标就跟着换了。
func (s *Store) ReplaceAddresses(ctx context.Context, nodeID int64, inputs []AddressInput) (relayTargetChanged bool, err error) {
	norm := make([]AddressInput, len(inputs))
	seenVal := make(map[string]bool, len(inputs))
	for i, in := range inputs {
		addr, e := normalizePoolAddress(in.Family, in.Address)
		if e != nil {
			return false, e
		}
		key := in.Family + "|" + addr
		if seenVal[key] {
			return false, fmt.Errorf("地址 %s 重复了", addr)
		}
		seenVal[key] = true
		norm[i] = AddressInput{ID: in.ID, Family: in.Family, Address: addr}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var oldV4, oldV6 string
	if err := tx.QueryRowContext(ctx,
		`SELECT sub_ipv4_address, ipv6_address FROM nodes WHERE id = ?`, nodeID).
		Scan(&oldV4, &oldV6); err != nil {
		return false, err
	}

	existing := make(map[int64]bool)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM node_addresses WHERE node_id = ?`, nodeID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		existing[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[int64]bool)
	for order, in := range norm {
		if in.ID > 0 && existing[in.ID] {
			if _, err := tx.ExecContext(ctx,
				`UPDATE node_addresses SET family = ?, address = ?, sort_order = ?, updated_at = ?
				  WHERE id = ? AND node_id = ?`,
				in.Family, in.Address, order, now, in.ID, nodeID); err != nil {
				return false, err
			}
			seen[in.ID] = true
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			nodeID, in.Family, in.Address, order, now, now); err != nil {
			return false, err
		}
	}
	for id := range existing {
		if !seen[id] {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM node_addresses WHERE id = ? AND node_id = ?`, id, nodeID); err != nil {
				return false, err
			}
		}
	}

	// 把地址池首条 V4 / V6 写回镜像列(没有则清空)。改动了 V4 或 V6 都要
	// 传播中转脏标记:落地对外的地址换了,下游中转机的 proxy_pass 目标就变了。
	newV4 := firstPoolAddress(norm, AddrFamilyV4)
	newV6 := firstPoolAddress(norm, AddrFamilyV6)
	if newV4 != oldV4 || newV6 != oldV6 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET sub_ipv4_address = ?, ipv6_address = ?, updated_at = ? WHERE id = ?`,
			newV4, newV6, now, nodeID); err != nil {
			return false, err
		}
		relayTargetChanged = true
	}
	return relayTargetChanged, tx.Commit()
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

// ReplaceEndpoints 用前端传来的整份列表替换一个入口的订阅地址条目。
//
// 整表删了重插(不像地址池要保 id):inbound_endpoints 没有任何东西引用它自己,
// id 不承载语义。isMieru 为真时端口按段校验(起点、终点),否则是单端口。
func (s *Store) ReplaceEndpoints(
	ctx context.Context, nodeID int64, kind string, entryID int64,
	inputs []EndpointInput, isMieru bool,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 地址池:endpoint 只能指向本机的地址(或 host=NULL)。指向别的机器的
	// 地址是一条不属于这台机器的订阅条目 —— 在这里拦住。
	owned := make(map[int64]bool)
	arows, err := tx.QueryContext(ctx, `SELECT id FROM node_addresses WHERE node_id = ?`, nodeID)
	if err != nil {
		return err
	}
	for arows.Next() {
		var id int64
		if err := arows.Scan(&id); err != nil {
			arows.Close()
			return err
		}
		owned[id] = true
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return err
	}

	for i, in := range inputs {
		if in.AddressID != nil && !owned[*in.AddressID] {
			return fmt.Errorf("第 %d 条地址不属于这台机器", i+1)
		}
		if err := validateEndpointPorts(in, isMieru); err != nil {
			return fmt.Errorf("第 %d 条:%w", i+1, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM inbound_endpoints WHERE entry_kind = ? AND entry_id = ?`,
		kind, entryID); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for order, in := range inputs {
		var addrID any
		if in.AddressID != nil {
			addrID = *in.AddressID
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO inbound_endpoints
			   (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
			    display_name, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, kind, entryID, addrID, in.PublicPort, in.PublicPortEnd,
			in.DisplayName, order, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
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
