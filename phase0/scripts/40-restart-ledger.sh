#!/bin/bash
# Phase 0 步骤 4:验证"重启前强制同步 + 计数器归零后累计流量不丢失"。
#
# 场景顺序:
#   A. 产生流量 → 同步 → 落库
#   B. 再产生流量 → 同步 → 只入账增量(验证基线差值法)
#   C. 空跑同步两次 → 不产生新记录(验证幂等)
#   D. 同步 → 重启 sing-box → 同步 → 累计值不回退(核心)
#   E. 重启后再产生流量 → 同步 → 基线从 0 重建,增量正确
#   F. 不同步就重启 → 量化未同步窗口的数据损失
#   G. API 不可达时同步 → 数据库不被破坏
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
PROBE=$BASE/bin/statsprobe-linux-amd64
DB=$BASE/phase0.db
API=127.0.0.1:28080

rm -f "$DB" "$DB"-wal "$DB"-shm

start_client() {
    nohup "$SB" run -c "$BASE/conf/client$1.json" > "$BASE/logs/client$1.log" 2>&1 &
    echo $! > "$BASE/logs/client$1.pid"
}
stop_clients() {
    for i in 1 2 3; do
        [ -f "$BASE/logs/client$i.pid" ] && kill "$(cat "$BASE/logs/client$i.pid")" 2>/dev/null
        rm -f "$BASE/logs/client$i.pid"
    done
}
trap stop_clients EXIT

traffic() {  # $1=客户端端口 $2=字节数
    curl -s --socks5-hostname "127.0.0.1:$1" \
        "https://speed.cloudflare.com/__down?bytes=$2" -o /dev/null \
        -w "    下载 %{size_download} 字节 (HTTP %{http_code})\n"
}

sync_now() { $PROBE sync --api "$API" --db "$DB" --node node-la | sed 's/^/    /'; }
totals()   { $PROBE totals --db "$DB" | sed 's/^/    /'; }
raw()      { $PROBE query --api "$API" | sed 's/^/    /'; }

start_client 1
start_client 2
sleep 3

echo "================ A. 首次流量与落库 ================"
traffic 21081 2000000
traffic 21082 1000000
echo "  节点当前计数器:"; raw
echo "  同步:"; sync_now
echo "  数据库累计:"; totals
echo

echo "================ B. 增量同步(基线差值) ================"
traffic 21081 1000000
echo "  同步:"; sync_now
echo "  数据库累计(user_000001 应约 3MB):"; totals
echo

echo "================ C. 幂等性:空跑同步两次 ================"
echo "  第一次:"; sync_now
echo "  第二次:"; sync_now
echo "  数据库累计(应不变):"; totals
echo

echo "================ D. 重启前同步 → 重启 → 计数器归零 ================"
BEFORE_UP=$(sqlite3 "$DB" "SELECT COALESCE(SUM(total_uplink+total_downlink),0) FROM user_traffic_totals" 2>/dev/null || echo "?")
echo "  重启前强制同步:"; sync_now
BEFORE=$(sqlite3 "$DB" "SELECT COALESCE(SUM(total_uplink+total_downlink),0) FROM user_traffic_totals" 2>/dev/null || echo "?")
echo "  重启前累计总量: $BEFORE 字节"
stop_clients
systemctl restart litebox-phase0
sleep 4
echo "  重启后节点计数器(应为空):"; raw
echo "  重启后同步:"; sync_now
AFTER=$(sqlite3 "$DB" "SELECT COALESCE(SUM(total_uplink+total_downlink),0) FROM user_traffic_totals" 2>/dev/null || echo "?")
echo "  重启后累计总量: $AFTER 字节"
if [ "$BEFORE" = "$AFTER" ]; then
    echo "  结论: 累计流量未丢失、未回退"
else
    echo "  结论: 累计流量发生变化($BEFORE -> $AFTER),需排查"
fi
echo

echo "================ E. 重启后基线重建 ================"
start_client 1
start_client 2
sleep 3
traffic 21081 1500000
echo "  节点计数器(从 0 重新累计):"; raw
echo "  同步:"; sync_now
echo "  数据库累计(user_000001 应约 4.5MB):"; totals
echo

echo "================ F. 未同步就重启的损失量化 ================"
traffic 21082 2000000
echo "  刚产生 2MB 但不同步,直接重启"
LOST_BEFORE=$(sqlite3 "$DB" "SELECT COALESCE(total_uplink+total_downlink,0) FROM user_traffic_totals WHERE user_code='user_000002'")
stop_clients
systemctl restart litebox-phase0
sleep 4
sync_now > /dev/null
LOST_AFTER=$(sqlite3 "$DB" "SELECT COALESCE(total_uplink+total_downlink,0) FROM user_traffic_totals WHERE user_code='user_000002'")
echo "  user_000002 重启前入库=$LOST_BEFORE 重启后入库=$LOST_AFTER"
echo "  丢失字节数 = $((LOST_BEFORE == LOST_AFTER ? 2000000 : 0)) 左右(未同步窗口内的流量不可恢复)"
echo

echo "================ G. API 不可达时的失败安全 ================"
SAFE_BEFORE=$(sqlite3 "$DB" "SELECT COALESCE(SUM(total_uplink+total_downlink),0) FROM user_traffic_totals")
systemctl stop litebox-phase0
sleep 1
echo "  对已停止的节点执行同步:"
$PROBE sync --api "$API" --db "$DB" --node node-la 2>&1 | sed 's/^/    /'
SAFE_AFTER=$(sqlite3 "$DB" "SELECT COALESCE(SUM(total_uplink+total_downlink),0) FROM user_traffic_totals")
echo "  同步前=$SAFE_BEFORE 同步后=$SAFE_AFTER"
if [ "$SAFE_BEFORE" = "$SAFE_AFTER" ]; then
    echo "  结论: 同步失败未污染已有统计"
else
    echo "  结论: 数据被破坏,严重问题"
fi
systemctl start litebox-phase0
sleep 3
echo

echo "================ 汇总 ================"
totals
echo "  ledger 明细(最近 10 条):"
sqlite3 -header -column "$DB" \
  "SELECT user_code, direction, delta_bytes, counter_value, substr(created_at,12,8) AS t
   FROM traffic_ledger ORDER BY id DESC LIMIT 10" | sed 's/^/    /'
