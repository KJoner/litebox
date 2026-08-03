package database

import (
	"database/sql"
	"testing"
)

// migrateUpTo 只应用到指定版本,用来模拟"线上跑着旧版本的库"。
func migrateUpTo(t *testing.T, db *sql.DB, version int) {
	t.Helper()
	if err := ensureMigrationTable(db); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Version > version {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("应用迁移 %d: %v", m.Version, err)
		}
	}
}

// V2 的迁移必须能直接升级线上库,而且升级前后每个用户可用的节点集合完全一致。
//
// 这是最容易出事又最难发现的一处:等级默认值填错,存量用户会在管理员
// 毫不知情的情况下集体多拿或少拿节点,而系统一个错都不报。
func TestV2UpgradePreservesExistingAccess(t *testing.T) {
	db := openTestDB(t)
	// 0006 是 V1 的最后一个版本。
	migrateUpTo(t, db, 6)

	const ts = "2026-08-01T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, created_at, updated_at)
		VALUES (1,'user_000001','老用户','enc-uuid','hash1',1024,10,20,?,?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, proxy_port, listen_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES (1,'洛杉矶-A','192.0.2.1',443,443,'www.cloudflare.com','enc','pub','sid',?,?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, proxy_port, listen_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES (2,'东京-B','192.0.2.2',443,443,'www.cloudflare.com','enc','pub','sid',?,?)`, ts, ts)
	// 老库里用户只被分配了 1 号节点。
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1,1,?)`, ts)

	if err := Migrate(db, nil); err != nil {
		t.Fatalf("升级到 V2: %v", err)
	}

	// 1. 存量用户与存量节点都落在普通组。
	var userTier, nodeTier int64
	mustScan(t, db, `SELECT access_tier_id FROM proxy_users WHERE id = 1`, &userTier)
	mustScan(t, db, `SELECT access_tier_id FROM nodes WHERE id = 1`, &nodeTier)
	if userTier != 1 || nodeTier != 1 {
		t.Errorf("升级后等级 = 用户 %d / 节点 %d,期望都是 1(普通组)", userTier, nodeTier)
	}

	// 2. 展示名称复制自内部名称。留空会让客户端把节点当成新条目重复添加。
	var displayName string
	mustScan(t, db, `SELECT display_name FROM nodes WHERE id = 1`, &displayName)
	if displayName != "洛杉矶-A" {
		t.Errorf("展示名称 = %q,期望复制内部名称", displayName)
	}

	// 3. 默认下发订阅。默认关掉会让所有人的订阅在升级后瞬间清空。
	var subEnabled int
	mustScan(t, db, `SELECT subscription_enabled FROM nodes WHERE id = 1`, &subEnabled)
	if subEnabled != 1 {
		t.Error("升级后节点默认不下发订阅,存量用户的订阅会被清空")
	}

	// 4. 可用节点集合不变。存量用户与存量节点同为普通组,
	//    等级继承本身就会给出这两个节点 —— 关键是不能少给,也不能因为
	//    额外授权与继承重复而出现两次。
	rows, err := db.Query(
		`SELECT node_id FROM user_effective_nodes WHERE proxy_user_id = 1 ORDER BY node_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("升级后的有效节点 = %v,期望 [1 2]", got)
	}

	// 5. 流量与订阅 Token 不受影响。
	var up, down int64
	var tokenHash string
	mustScan(t, db, `SELECT used_uplink, used_downlink FROM proxy_users WHERE id = 1`, &up, &down)
	mustScan(t, db, `SELECT sub_token_hash FROM proxy_users WHERE id = 1`, &tokenHash)
	if up != 10 || down != 20 || tokenHash != "hash1" {
		t.Errorf("升级动了存量数据:up=%d down=%d token=%q", up, down, tokenHash)
	}

	// 6. 额外授权一行不少。
	var grants int
	mustScan(t, db, `SELECT COUNT(*) FROM user_nodes`, &grants)
	if grants != 1 {
		t.Errorf("user_nodes 行数 = %d,期望 1", grants)
	}
}

// 升级必须可重复执行:一键脚本每次部署都会跑一遍迁移。
func TestV2UpgradeIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 6)
	for i := 0; i < 3; i++ {
		if err := Migrate(db, nil); err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}
	}
	var tiers int
	mustScan(t, db, `SELECT COUNT(*) FROM access_tiers`, &tiers)
	if tiers != 3 {
		t.Errorf("等级数 = %d,期望 3(重复迁移不应重复插入)", tiers)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("执行 %s: %v", query, err)
	}
}

func mustScan(t *testing.T, db *sql.DB, query string, dest ...any) {
	t.Helper()
	if err := db.QueryRow(query).Scan(dest...); err != nil {
		t.Fatalf("查询 %s: %v", query, err)
	}
}
