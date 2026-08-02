#!/bin/bash
# Phase 0 步骤 6:低配节点内存占用测试。
# 用 systemd MemoryMax=128M 模拟 128MB 节点(cgroup v2 硬上限,超限即 OOM kill)。
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
SBFULL=$BASE/bin/sing-box-linux-amd64
PROBE=$BASE/bin/statsprobe-linux-amd64
SVC=litebox-phase0
API=127.0.0.1:28080

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

mem_report() {  # $1=标签
    local pid rss
    pid=$(systemctl show -p MainPID --value $SVC)
    rss=$(awk '/^VmRSS/{print $2}' /proc/"$pid"/status 2>/dev/null)
    local cg
    cg=$(cat /sys/fs/cgroup/system.slice/$SVC.service/memory.current 2>/dev/null || echo 0)
    printf "  %-26s RSS=%6.1f MB   cgroup当前=%6.1f MB\n" \
        "$1" "$(echo "$rss/1024" | bc -l)" "$(echo "$cg/1048576" | bc -l)"
}

set_memory_limit() {  # $1=限制值或 infinity  $2=GOMEMLIMIT(可空)
    mkdir -p /etc/systemd/system/$SVC.service.d
    {
        echo "[Service]"
        echo "MemoryMax=$1"
        echo "MemoryAccounting=yes"
        [ -n "${2:-}" ] && echo "Environment=GOMEMLIMIT=$2"
        [ -n "${2:-}" ] && echo "Environment=GOGC=50"
    } > /etc/systemd/system/$SVC.service.d/mem.conf
    systemctl daemon-reload
    systemctl restart $SVC
    sleep 4
}

load_test() {  # $1=并发数 $2=每个请求字节数
    stop_clients
    start_client 1
    sleep 2
    local pids=()
    for _ in $(seq 1 "$1"); do
        curl -s --socks5-hostname 127.0.0.1:21081 \
            "https://speed.cloudflare.com/__down?bytes=$2" -o /dev/null &
        pids+=($!)
    done
    # 负载进行中采样内存峰值
    local peak=0 pid cur
    pid=$(systemctl show -p MainPID --value $SVC)
    for _ in $(seq 1 25); do
        cur=$(awk '/^VmRSS/{print $2}' /proc/"$pid"/status 2>/dev/null || echo 0)
        [ "${cur:-0}" -gt "$peak" ] && peak=$cur
        sleep 0.4
    done
    wait "${pids[@]}" 2>/dev/null
    printf "  %-26s 峰值RSS=%6.1f MB\n" "并发$1 x $(($2/1000000))MB" "$(echo "$peak/1024" | bc -l)"
}

echo "================ 1. 精简构建 / 无内存限制 ================"
rm -f /etc/systemd/system/$SVC.service.d/mem.conf 2>/dev/null
systemctl daemon-reload; systemctl restart $SVC; sleep 4
mem_report "空闲"
load_test 1 3000000
load_test 8 3000000
mem_report "负载后"
$PROBE sysstats --api $API | sed 's/^/  /'
echo

echo "================ 2. 精简构建 / MemoryMax=128M ================"
set_memory_limit 128M
mem_report "空闲(限128M)"
load_test 8 3000000
load_test 16 2000000
mem_report "负载后"
echo "  服务状态: $(systemctl is-active $SVC)"
echo "  OOM 次数: $(systemctl show -p NRestarts --value $SVC)"
$PROBE sysstats --api $API | sed 's/^/  /'
echo

echo "================ 3. 精简构建 / MemoryMax=128M + GOMEMLIMIT=96MiB ================"
set_memory_limit 128M 96MiB
mem_report "空闲"
load_test 16 2000000
mem_report "负载后"
echo "  服务状态: $(systemctl is-active $SVC)"
$PROBE sysstats --api $API | sed 's/^/  /'
echo

echo "================ 4. 完整标签构建对比(无限制) ================"
sed -i "s#$SB#$SBFULL#" /etc/systemd/system/$SVC.service
rm -f /etc/systemd/system/$SVC.service.d/mem.conf
systemctl daemon-reload; systemctl restart $SVC; sleep 4
mem_report "空闲(完整构建)"
load_test 8 3000000
mem_report "负载后"
sed -i "s#$SBFULL#$SB#" /etc/systemd/system/$SVC.service
systemctl daemon-reload; systemctl restart $SVC; sleep 3
echo

echo "================ 5. 二进制体积 ================"
ls -la "$SB" "$SBFULL" | awk '{printf "  %-46s %6.1f MB\n", $9, $5/1048576}'
echo

echo "================ 6. 系统整体 ================"
free -m | sed 's/^/  /'
