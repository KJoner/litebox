#!/bin/bash
# Phase 0 步骤 3:验证两个 VLESS 用户均能连接,并能按用户读取上下行流量。
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
PROBE=$BASE/bin/statsprobe-linux-amd64

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

echo "=== 启动两个客户端 ==="
start_client 1
start_client 2
sleep 3
ss -tlnp | grep -E '21081|21082' | awk '{print "  监听 " $4}'
echo

echo "=== 用户1 (user_000001) 通过代理下载 3MB ==="
curl -s --socks5-hostname 127.0.0.1:21081 \
    "https://speed.cloudflare.com/__down?bytes=3000000" -o /dev/null \
    -w "  HTTP=%{http_code}  下载=%{size_download} 字节  耗时=%{time_total}s\n"

echo "=== 用户2 (user_000002) 通过代理下载 1MB ==="
curl -s --socks5-hostname 127.0.0.1:21082 \
    "https://speed.cloudflare.com/__down?bytes=1000000" -o /dev/null \
    -w "  HTTP=%{http_code}  下载=%{size_download} 字节  耗时=%{time_total}s\n"
echo

echo "=== 服务端日志(最近 6 行) ==="
journalctl -u litebox-phase0 -n 6 --no-pager -o cat
echo

echo "=== V2Ray Stats API:按用户读取流量 ==="
$PROBE query --api 127.0.0.1:28080
echo

echo "=== sing-box 运行时状态 ==="
$PROBE sysstats --api 127.0.0.1:28080
echo

echo "=== 进程内存占用 ==="
ps -o pid,rss,vsz,comm -p "$(systemctl show -p MainPID --value litebox-phase0)" | \
    awk 'NR==1{print "  " $0} NR==2{printf "  %s  RSS=%.1f MB\n", $0, $2/1024}'
