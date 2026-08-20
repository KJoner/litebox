package database

import (
	"database/sql"
	"testing"
)

// V8(多入站)的迁移 0019 把 nodes 上那十几列整体搬进 node_inbounds。
//
// 这是这一版里最容易出事、也最难发现的一处:数据搬错的表现不是报错,
// 而是"升级完之后订阅里少了几个节点""某个用户凭空多拿到一台机器"
// ——全链路不报任何错。所以升级前后这几件事必须逐条对上。
func TestV8UpgradeMovesInboundColumnsWithoutChangingBehaviour(t *testing.T) {
	db := openTestDB(t)
	// 0018 是 V7 的最后一个版本。
	migrateUpTo(t, db, 18)

	const ts = "2026-08-01T00:00:00Z"
	// 落地 A:普通组、VLESS、NAT 端口(公网 443 / 主机 20443)、已部署。
	mustExec(t, db, `
		INSERT INTO nodes (id, name, display_name, host, proxy_port, listen_port, api_port,
			ipv6_address, ipv6_proxy_port, reality_dest, reality_dest_port,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			handshake_max_record_size, handshake_checked_at,
			protocol, ss_method, ss_password_encrypted, tcp_fast_open,
			deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
			deployed_config_sha256, access_tier_id, subscription_enabled, role,
			created_at, updated_at)
		VALUES (1,'LAX-A','洛杉矶 01','192.0.2.1',443,20443,28080,
			'2602:fed2::1',8443,'www.fastly.com',443,'enc-priv','pub','abcd1234',
			4096,?, 'VLESS_REALITY','','',1,
			'VLESS_REALITY','',1,'deadbeef',1,1,'LANDING',?,?)`, ts, ts, ts)
	// 落地 B:VIP 组、Shadowsocks、已部署,并且被 A 用 user_nodes 额外授权过。
	mustExec(t, db, `
		INSERT INTO nodes (id, name, display_name, host, proxy_port, listen_port, api_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			protocol, ss_method, ss_password_encrypted,
			deployed_protocol, deployed_ss_method, deployed_config_sha256,
			access_tier_id, subscription_enabled, role, created_at, updated_at)
		VALUES (2,'TYO-B','东京 02','192.0.2.2',8388,8388,28080,
			'','','','', 'SHADOWSOCKS','2022-blake3-aes-128-gcm','enc-ss',
			'SHADOWSOCKS','2022-blake3-aes-128-gcm','cafebabe',2,1,'LANDING',?,?)`, ts, ts)
	// 落地 C:链式出站指向 B,带链路凭据。
	mustExec(t, db, `
		INSERT INTO nodes (id, name, display_name, host, proxy_port, listen_port, api_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			protocol, deployed_protocol, deployed_config_sha256,
			chain_target_kind, chain_target_node_id, chain_code,
			chain_uuid_encrypted, chain_ss_password_encrypted,
			access_tier_id, subscription_enabled, role, created_at, updated_at)
		VALUES (3,'SIN-C','新加坡 03','192.0.2.3',443,443,28080,
			'www.fastly.com','enc-priv3','pub3','beef1234',
			'VLESS_REALITY','VLESS_REALITY','f00d',
			'NODE',2,'chain_000001','enc-cuuid','enc-css',1,1,'LANDING',?,?)`, ts, ts)
	// 中转机 R:没有 sing-box,不该产生入站行。
	mustExec(t, db, `
		INSERT INTO nodes (id, name, display_name, host, proxy_port, listen_port, api_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			access_tier_id, subscription_enabled, role, created_at, updated_at)
		VALUES (4,'RELAY-R','中转 R','192.0.2.4',0,0,0,'','','','',1,1,'RELAY',?,?)`, ts, ts)
	// 中转 R 上一条指向落地 B 的转发规则。
	mustExec(t, db, `
		INSERT INTO node_relays (id, node_id, display_name, listen_port, public_port,
			target_kind, target_node_id, access_tier_id, created_at, updated_at)
		VALUES (1,4,'中转-东京',48080,10660,'NODE',2,1,?,?)`, ts, ts)

	// 一个普通用户,靠 user_nodes 被【显式授权】到 VIP 的 B 上。
	mustExec(t, db, `
		INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, access_tier_id, created_at, updated_at)
		VALUES (1,'user_000001','普通用户','enc-uuid','hash1',0,0,0,1,?,?)`, ts, ts)
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1,2,?)`, ts)

	// 升级前每个用户能用哪些机器,升级后必须一模一样。
	before := effectiveNodeIDs(t, db, 1)

	if err := Migrate(db, nil); err != nil {
		t.Fatalf("升级到 V8: %v", err)
	}

	// 1. 每台落地各一行入站,中转机零行。
	for nodeID, want := range map[int64]int{1: 1, 2: 1, 3: 1, 4: 0} {
		var got int
		scanWith(t, db, `SELECT COUNT(*) FROM node_inbounds WHERE node_id = ?`,
			[]any{nodeID}, &got)
		if got != want {
			t.Errorf("节点 %d 迁移出 %d 个入站,期望 %d", nodeID, got, want)
		}
	}

	// 2. tag 取存量值。填别的会让全部存量节点在升级后的第一次配置比对里
	//    出现差异,进而排队重启一遍 —— 而那次重启换不来任何配置变化。
	for nodeID, want := range map[int64]string{1: "vless-in", 2: "ss-in", 3: "vless-in"} {
		var got string
		scanWith(t, db, `SELECT tag FROM node_inbounds WHERE node_id = ?`, []any{nodeID}, &got)
		if got != want {
			t.Errorf("节点 %d 的入站 tag = %q,期望 %q", nodeID, got, want)
		}
	}

	// 3. 端口的三个位置各就各位。搬错的表现是 sing-box 监听在转发链路
	//    另一端的号码上:check、服务状态、端口监听全过,只有用户连不上。
	var listen, public, ipv6Port int
	mustScan(t, db,
		`SELECT listen_port, public_port, ipv6_public_port FROM node_inbounds WHERE node_id = 1`,
		&listen, &public, &ipv6Port)
	if listen != 20443 || public != 443 || ipv6Port != 8443 {
		t.Errorf("端口搬错了:listen=%d public=%d ipv6=%d,期望 20443/443/8443",
			listen, public, ipv6Port)
	}

	// 4. 协议、密钥、握手目标与已部署的那一套原样搬过去。
	var protocol, ssMethod, ssKey, deployedProtocol, realityDest, realityPriv string
	var maxRecord int
	mustScan(t, db, `SELECT protocol, ss_method, ss_password_encrypted, deployed_protocol,
		       reality_dest, reality_privkey_encrypted, handshake_max_record_size
		  FROM node_inbounds WHERE node_id = 2`,
		&protocol, &ssMethod, &ssKey, &deployedProtocol, &realityDest, &realityPriv, &maxRecord)
	if protocol != "SHADOWSOCKS" || ssMethod != "2022-blake3-aes-128-gcm" || ssKey != "enc-ss" {
		t.Errorf("Shadowsocks 参数没搬对:%q/%q/%q", protocol, ssMethod, ssKey)
	}
	if deployedProtocol != "SHADOWSOCKS" {
		t.Errorf("deployed_protocol 没搬对:%q —— 订阅只看这一列", deployedProtocol)
	}

	// 5. 链式落地从"那台机器"改成"那台机器上的入站",凭据一个字不动。
	var chainKind, chainCode, chainUUID string
	var chainTarget, targetInboundOfB int64
	mustScan(t, db, `SELECT chain_target_kind, chain_target_inbound_id, chain_code,
		       chain_uuid_encrypted FROM node_inbounds WHERE node_id = 3`,
		&chainKind, &chainTarget, &chainCode, &chainUUID)
	mustScan(t, db, `SELECT id FROM node_inbounds WHERE node_id = 2`, &targetInboundOfB)
	if chainKind != "INBOUND" || chainTarget != targetInboundOfB {
		t.Errorf("链式落地 = %s/#%d,期望 INBOUND/#%d", chainKind, chainTarget, targetInboundOfB)
	}
	if chainCode != "chain_000001" || chainUUID != "enc-cuuid" {
		t.Errorf("链路凭据被动过:%q/%q —— 换一次代码就等于在落地的 ledger 里"+
			"留下一行永远对不上的历史", chainCode, chainUUID)
	}

	// 6. 转发规则同样指向入站。指错的表现是流量进了管理员没打算用的那个入口。
	var relayKind string
	var relayTarget int64
	mustScan(t, db, `SELECT target_kind, target_inbound_id FROM node_relays WHERE id = 1`,
		&relayKind, &relayTarget)
	if relayKind != "INBOUND" || relayTarget != targetInboundOfB {
		t.Errorf("转发规则的落地 = %s/#%d,期望 INBOUND/#%d",
			relayKind, relayTarget, targetInboundOfB)
	}

	// 7. **入站等级一律是普通组,不继承节点的等级。**
	//
	//    继承的话,user_nodes 的额外授权会凭空作废:管理员显式把 VIP 的 B
	//    授权给普通用户,而 B 上唯一的入站等级是 VIP,于是这个用户在节点视图里
	//    有、在入站视图里没有 —— 授权作废而面板一个字都不说。
	var tier int64
	mustScan(t, db, `SELECT access_tier_id FROM node_inbounds WHERE node_id = 2`, &tier)
	if tier != 1 {
		t.Errorf("入站等级 = %d,存量行必须落在普通组(1),否则额外授权会失效", tier)
	}

	// 8. 升级前后"这个用户能用哪些机器"完全一致。
	after := effectiveNodeIDs(t, db, 1)
	if len(before) != len(after) {
		t.Fatalf("可用机器变了:升级前 %v,升级后 %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("可用机器变了:升级前 %v,升级后 %v", before, after)
		}
	}

	// 9. 有效入站视图落在【全部落地机器】上 —— 存量数据上这一层必须完全透明:
	//    每台落地正好一个入站,等级又一律是普通组,所以它一个人都不该筛掉。
	//
	//    中转机不在其中,而它在有效节点视图里 —— 那是对的:
	//    那台机器上没有任何入站,用户拿不到它的任何凭据。
	inboundNodes := queryIDs(t, db,
		`SELECT DISTINCT node_id FROM user_effective_inbounds
		  WHERE proxy_user_id = 1 ORDER BY node_id`)
	landing := queryIDs(t, db,
		`SELECT n.id FROM nodes n
		  JOIN user_effective_nodes en ON en.node_id = n.id
		 WHERE en.proxy_user_id = 1 AND n.role = 'LANDING' ORDER BY n.id`)
	if len(inboundNodes) != len(landing) {
		t.Errorf("有效入站落在 %v 台机器上,而有效落地是 %v —— 这一层不该改变任何存量可见性",
			inboundNodes, landing)
	}
}

// 升级必须可重复执行:一键脚本每次部署都会跑一遍迁移。
func TestV8UpgradeIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 18)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db, nil); err != nil {
		t.Fatalf("重复执行迁移失败: %v", err)
	}
}

// scanWith 是带参数版的 mustScan —— 共享的那个只收查询文本。
func scanWith(t *testing.T, db *sql.DB, query string, args []any, dest ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(dest...); err != nil {
		t.Fatalf("查询 %s: %v", query, err)
	}
}

func effectiveNodeIDs(t *testing.T, db *sql.DB, userID int64) []int64 {
	t.Helper()
	return queryIDs(t, db,
		`SELECT node_id FROM user_effective_nodes WHERE proxy_user_id = ? ORDER BY node_id`, userID)
}

func queryIDs(t *testing.T, db *sql.DB, query string, args ...any) []int64 {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}
