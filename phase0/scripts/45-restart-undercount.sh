#!/bin/bash
# Phase 0 步骤 4b:验证"重启后流量大于重启前计数值"这一漏算场景。
#
# 仅靠 value < baseline 判定重启是不够的:
#   同步 -> 计数器=1MB(基线=1MB) -> 重启 -> 产生 3MB -> 同步
#   此时计数器=3MB > 基线=1MB,不会触发回退判定,
#   增量被算成 3MB-1MB=2MB,少算 1MB。
# 引入 GetSysStats.Uptime 反推进程启动时刻后应算出完整的 3MB。
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
PROBE=$BASE/bin/statsprobe-linux-amd64
DB=$BASE/undercount.db
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

sum_user1() { sqlite3 "$DB" "SELECT COALESCE(total_downlink,0) FROM user_traffic_totals WHERE user_code='user_000001'"; }

systemctl restart litebox-phase0
sleep 4
start_client 1
sleep 3

echo "=== 阶段1: 产生 1MB 并同步 ==="
curl -s --socks5-hostname 127.0.0.1:21081 "https://speed.cloudflare.com/__down?bytes=1000000" -o /dev/null -w "  下载 %{size_download} 字节\n"
$PROBE sync --api "$API" --db "$DB" --node node-la | sed 's/^/  /'
S1=$(sum_user1)
echo "  入库下行累计: $S1"
echo

echo "=== 阶段2: 重启节点 ==="
stop_clients
systemctl restart litebox-phase0
sleep 4
start_client 1
sleep 3

echo "=== 阶段3: 重启后产生 4MB(远超重启前的 1MB) ==="
curl -s --socks5-hostname 127.0.0.1:21081 "https://speed.cloudflare.com/__down?bytes=4000000" -o /dev/null -w "  下载 %{size_download} 字节\n"
echo "  节点计数器:"
$PROBE query --api "$API" | sed 's/^/    /'
echo

echo "=== 阶段4: 同步 ==="
$PROBE sync --api "$API" --db "$DB" --node node-la | sed 's/^/  /'
S2=$(sum_user1)
echo "  入库下行累计: $S2"
DELTA=$((S2 - S1))
echo
echo "=== 判定 ==="
echo "  本轮入账增量 = $DELTA 字节"
echo "  期望值约 4.02MB(重启后的全部流量)"
if [ "$DELTA" -gt 3900000 ]; then
    echo "  结论: 正确 —— uptime 判定生效,未漏算"
else
    echo "  结论: 漏算 —— 增量仅 $DELTA,少算约 $((4020000 - DELTA)) 字节"
fi
