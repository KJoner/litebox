// statsprobe 是 LiteBox Phase 0 的技术验证原型:
//   1. 通过 gRPC 读取 sing-box V2Ray Stats API 的用户级流量计数器;
//   2. 以"绝对计数器 + 基线差值"的方式把增量写入 SQLite traffic_ledger;
//   3. 验证节点重启后计数器归零时,累计流量不丢失、不回退。
//
// 注意:sing-box 服务端在 init() 中把 gRPC 服务名改注册为
// v2ray.core.app.stats.command.StatsService,而其自带客户端 stub 生成的
// 调用路径是 /experimental.v2rayapi.StatsService/...,两者不一致。
// 因此这里不使用生成的 client,而是用 conn.Invoke 显式指定正确路径。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "modernc.org/sqlite"

	"litebox/phase0/statsprobe/v2rayapi"
)

const (
	methodQueryStats  = "/v2ray.core.app.stats.command.StatsService/QueryStats"
	methodGetStats    = "/v2ray.core.app.stats.command.StatsService/GetStats"
	methodGetSysStats = "/v2ray.core.app.stats.command.StatsService/GetSysStats"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	api := fs.String("api", "127.0.0.1:28080", "V2Ray API gRPC 地址")
	dbPath := fs.String("db", "phase0.db", "SQLite 数据库路径")
	nodeID := fs.String("node", "node1", "节点标识")
	name := fs.String("name", "", "GetStats 计数器名称")
	destHost := fs.String("host", "", "REALITY 握手目标域名 / SSH 主机")
	destPort := fs.Int("port", 443, "REALITY 握手目标端口 / SSH 端口")
	sshUser := fs.String("user", "root", "SSH 用户名")
	sshKey := fs.String("key", "", "SSH 私钥路径")
	remoteAPI := fs.String("remote-api", "127.0.0.1:28080", "节点上 V2Ray API 的回环地址")
	fs.Parse(os.Args[2:])

	var err error
	switch cmd {
	case "destcheck":
		err = cmdDestCheck(*destHost, *destPort)
	case "tunnel":
		err = cmdTunnel(*destHost, *destPort, *sshUser, *sshKey, *remoteAPI)
	case "query":
		err = cmdQuery(*api)
	case "get":
		err = cmdGet(*api, *name)
	case "sysstats":
		err = cmdSysStats(*api)
	case "sync":
		err = cmdSync(*api, *dbPath, *nodeID)
	case "totals":
		err = cmdTotals(*dbPath)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法: statsprobe <命令> [选项]
命令:
  destcheck 验证 REALITY 握手目标是否满足要求 (--host)
  tunnel   通过 SSH 隧道读取节点 API (--host --port --user --key)
  query    读取所有 user>>> 计数器(不清零)
  get      读取单个计数器 (--name)
  sysstats 读取 sing-box 运行时内存/协程统计
  sync     同步流量到 SQLite ledger(基线差值法,幂等)
  totals   显示 SQLite 中的用户累计流量`)
}

func dial(api string) (*grpc.ClientConn, error) {
	return grpc.NewClient(api, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func queryUserCounters(conn *grpc.ClientConn) (map[string]int64, error) {
	req := &v2rayapi.QueryStatsRequest{Patterns: []string{"user>>>"}}
	resp := &v2rayapi.QueryStatsResponse{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, methodQueryStats, req, resp); err != nil {
		return nil, err
	}
	counters := make(map[string]int64)
	for _, stat := range resp.Stat {
		counters[stat.Name] = stat.Value
	}
	return counters, nil
}

func cmdQuery(api string) error {
	conn, err := dial(api)
	if err != nil {
		return err
	}
	defer conn.Close()
	counters, err := queryUserCounters(conn)
	if err != nil {
		return err
	}
	if len(counters) == 0 {
		fmt.Println("(暂无用户计数器 —— 尚未有流量经过)")
		return nil
	}
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-55s %d\n", name, counters[name])
	}
	return nil
}

func cmdGet(api, name string) error {
	if name == "" {
		return fmt.Errorf("需要 --name,例如 user>>>user_000001>>>traffic>>>uplink")
	}
	conn, err := dial(api)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := &v2rayapi.GetStatsRequest{Name: name}
	resp := &v2rayapi.GetStatsResponse{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, methodGetStats, req, resp); err != nil {
		return err
	}
	fmt.Printf("%s = %d\n", resp.Stat.Name, resp.Stat.Value)
	return nil
}

func cmdSysStats(api string) error {
	conn, err := dial(api)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := &v2rayapi.SysStatsRequest{}
	resp := &v2rayapi.SysStatsResponse{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, methodGetSysStats, req, resp); err != nil {
		return err
	}
	fmt.Printf("Uptime: %ds  Goroutines: %d  Alloc: %.1fMB  Sys: %.1fMB  NumGC: %d\n",
		resp.Uptime, resp.NumGoroutine,
		float64(resp.Alloc)/1024/1024, float64(resp.Sys)/1024/1024, resp.NumGC)
	return nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	schema := `
CREATE TABLE IF NOT EXISTS node_counters (
    node_id    TEXT NOT NULL,
    user_code  TEXT NOT NULL,
    direction  TEXT NOT NULL CHECK (direction IN ('uplink','downlink')),
    last_value INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, user_code, direction)
);
-- 记录节点 sing-box 进程的启动时刻(由 GetSysStats.Uptime 反推)。
-- 仅靠"计数器变小"判定重启是不够的:若重启后流量超过重启前的计数值,
-- 计数器不会变小,基线差值法会静默少算。必须独立判定进程是否换代。
CREATE TABLE IF NOT EXISTS node_instances (
    node_id       TEXT PRIMARY KEY,
    started_at    INTEGER NOT NULL,
    last_sync_at  INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS traffic_ledger (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id      TEXT NOT NULL,
    node_id       TEXT NOT NULL,
    user_code     TEXT NOT NULL,
    direction     TEXT NOT NULL,
    delta_bytes   INTEGER NOT NULL,
    counter_value INTEGER NOT NULL,
    created_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_batch
    ON traffic_ledger (batch_id, node_id, user_code, direction);
CREATE TABLE IF NOT EXISTS user_traffic_totals (
    user_code      TEXT PRIMARY KEY,
    total_uplink   INTEGER NOT NULL DEFAULT 0,
    total_downlink INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT NOT NULL
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// parseCounterName 解析 user>>>user_000001>>>traffic>>>uplink 形式的计数器名。
func parseCounterName(name string) (userCode, direction string, ok bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
		return "", "", false
	}
	if parts[3] != "uplink" && parts[3] != "downlink" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

func newBatchID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// cmdSync 是"定时同步 + 重启前强制同步"的核心原型:
// 读取绝对计数器 → 与基线比较求增量 → 单事务写 ledger 并推进基线。
// 计数器小于基线说明 sing-box 已重启,基线归零重建,累计值不受影响。
func cmdSync(api, dbPath, nodeID string) error {
	conn, err := dial(api)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 先取运行时状态,反推 sing-box 进程的启动时刻,用于判定是否换代。
	// 顺序很重要:必须在读计数器之前取,否则可能把重启后的计数器
	// 与重启前的 epoch 配对。
	sysReq := &v2rayapi.SysStatsRequest{}
	sysResp := &v2rayapi.SysStatsResponse{}
	sysCtx, sysCancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = conn.Invoke(sysCtx, methodGetSysStats, sysReq, sysResp)
	sysCancel()
	if err != nil {
		return fmt.Errorf("读取运行时状态失败(不修改数据库): %w", err)
	}
	startedAt := time.Now().Unix() - int64(sysResp.Uptime)

	counters, err := queryUserCounters(conn)
	if err != nil {
		return fmt.Errorf("读取统计失败(不修改数据库): %w", err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	// 判定进程是否已换代。两个独立信号,任一命中即认定重启:
	//   信号1 启动时刻前移:startedAt 由主控自己的时钟推算,两次同步之间
	//         不应发生位移。容差 driftTolerance 覆盖 Uptime 的秒级截断与网络往返。
	//   信号2 uptime 小于两次同步的间隔:若进程在上次同步时就已存在,
	//         则 uptime 必然 >= 间隔。
	// 阈值必须紧贴测量噪声。漏判的代价不是"几秒流量",而是整个重启前
	// 计数值被当作基线扣掉(见 45-restart-undercount 测试)。
	const driftTolerance = 3
	var prevStartedAt, prevSyncAt int64
	err = tx.QueryRow(`SELECT started_at, last_sync_at FROM node_instances WHERE node_id=?`, nodeID).
		Scan(&prevStartedAt, &prevSyncAt)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	nowUnix := time.Now().Unix()
	restarted := false
	if prevStartedAt != 0 {
		if startedAt-prevStartedAt > driftTolerance {
			fmt.Printf("检测到节点重启(启动时刻前移 %d 秒,uptime=%ds)\n", startedAt-prevStartedAt, sysResp.Uptime)
			restarted = true
		} else if prevSyncAt != 0 && int64(sysResp.Uptime) < nowUnix-prevSyncAt-driftTolerance {
			fmt.Printf("检测到节点重启(uptime=%ds 小于同步间隔 %ds)\n", sysResp.Uptime, nowUnix-prevSyncAt)
			restarted = true
		}
	}
	if restarted {
		if _, err := tx.Exec(`UPDATE node_counters SET last_value=0, updated_at=? WHERE node_id=?`, now, nodeID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO node_instances (node_id, started_at, last_sync_at, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(node_id) DO UPDATE SET started_at=excluded.started_at,
		     last_sync_at=excluded.last_sync_at, updated_at=excluded.updated_at`,
		nodeID, startedAt, nowUnix, now); err != nil {
		return err
	}
	batchID := newBatchID()
	var synced int

	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, counterName := range names {
		value := counters[counterName]
		userCode, direction, ok := parseCounterName(counterName)
		if !ok {
			continue
		}
		var baseline int64
		err := tx.QueryRow(
			`SELECT last_value FROM node_counters WHERE node_id=? AND user_code=? AND direction=?`,
			nodeID, userCode, direction).Scan(&baseline)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		delta := value - baseline
		if value < baseline {
			// 兜底:uptime 判定未命中但计数器仍然回退(例如时钟异常)。
			fmt.Printf("计数器回退 %s/%s(%d < %d),按重建基线处理\n", userCode, direction, value, baseline)
			delta = value
		}
		if delta > 0 {
			if _, err := tx.Exec(
				`INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
				 VALUES (?,?,?,?,?,?,?)`,
				batchID, nodeID, userCode, direction, delta, value, now); err != nil {
				return err
			}
			column := "total_uplink"
			if direction == "downlink" {
				column = "total_downlink"
			}
			if _, err := tx.Exec(fmt.Sprintf(
				`INSERT INTO user_traffic_totals (user_code, %s, updated_at) VALUES (?,?,?)
				 ON CONFLICT(user_code) DO UPDATE SET %s = %s + excluded.%s, updated_at = excluded.updated_at`,
				column, column, column, column),
				userCode, delta, now); err != nil {
				return err
			}
			fmt.Printf("入账 %s/%s: +%d 字节 (计数器=%d, 基线=%d)\n", userCode, direction, delta, value, baseline)
			synced++
		}
		if _, err := tx.Exec(
			`INSERT INTO node_counters (node_id, user_code, direction, last_value, updated_at) VALUES (?,?,?,?,?)
			 ON CONFLICT(node_id, user_code, direction) DO UPDATE SET last_value = excluded.last_value, updated_at = excluded.updated_at`,
			nodeID, userCode, direction, value, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("同步完成 batch=%s,共入账 %d 条增量\n", batchID, synced)
	return nil
}

func cmdTotals(dbPath string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT user_code, total_uplink, total_downlink, updated_at FROM user_traffic_totals ORDER BY user_code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("%-15s %15s %15s  %s\n", "用户", "上行(字节)", "下行(字节)", "更新时间")
	for rows.Next() {
		var user, updatedAt string
		var up, down int64
		if err := rows.Scan(&user, &up, &down, &updatedAt); err != nil {
			return err
		}
		fmt.Printf("%-15s %15d %15d  %s\n", user, up, down, updatedAt)
	}
	var ledgerCount int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM traffic_ledger`).Scan(&ledgerCount); err != nil {
		return err
	}
	fmt.Printf("traffic_ledger 共 %d 条记录\n", ledgerCount)
	return rows.Err()
}
