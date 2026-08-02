#!/bin/bash
# Phase 0 步骤 7:验证 experimental.v2ray_api.stats.users 与 inbound.users 不一致的后果。
#
# stats.go 中 countUser := user != "" && s.users[user],
# 即用户必须同时出现在 stats.users 白名单里才会被计数。
# 若配置生成时漏掉,该用户可以正常上网但完全不产生流量记录 ——
# 这是一个不会报错、不会告警的计费失效,必须在配置生成阶段就杜绝。
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
PROBE=$BASE/bin/statsprobe-linux-amd64
SVC=litebox-phase0
API=127.0.0.1:28080
LIVE=$BASE/conf/server.json

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

# 构造:inbound 有 user_000001 与 user_000002,但 stats.users 只列 user_000001
python3 - <<'PY'
import json
base = "/opt/litebox-phase0/conf"
cfg = json.load(open(f"{base}/server-rev1.json"))
cfg["experimental"]["v2ray_api"]["stats"]["users"] = ["user_000001"]
json.dump(cfg, open(f"{base}/server-missing-stats.json", "w"), indent=2)
print("  已生成 server-missing-stats.json:inbound 2 个用户,stats.users 只有 1 个")
PY

echo "  sing-box check 结果:"
if $SB check -c "$BASE/conf/server-missing-stats.json" 2>/dev/null; then
    echo "    通过 —— check 不会发现 stats.users 缺项"
else
    echo "    被拒绝"
fi
echo

cp "$BASE/conf/server-missing-stats.json" "$LIVE"
systemctl restart $SVC
sleep 4
start_client 1
start_client 2
sleep 3

echo "  两个用户各下载 2MB:"
curl -s --socks5-hostname 127.0.0.1:21081 "https://speed.cloudflare.com/__down?bytes=2000000" -o /dev/null -w "    user_000001 下载 %{size_download} 字节\n"
curl -s --socks5-hostname 127.0.0.1:21082 "https://speed.cloudflare.com/__down?bytes=2000000" -o /dev/null -w "    user_000002 下载 %{size_download} 字节\n"
echo

echo "  节点计数器:"
$PROBE query --api $API | sed 's/^/    /'
echo

if $PROBE query --api $API | grep -q user_000002; then
    echo "  结论: user_000002 有计数(与代码分析不符,需复核)"
else
    echo "  结论: user_000002 能正常上网但完全没有流量记录 —— 静默计费失效已复现"
fi
echo

echo "  恢复正确配置 (rev1)"
stop_clients
cp "$BASE/conf/server-rev1.json" "$LIVE"
systemctl restart $SVC
sleep 3
systemctl is-active $SVC
