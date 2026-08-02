#!/bin/bash
# 在节点上用指定 UUID 起一个临时 sing-box 客户端并下载指定字节数,
# 用于验收时产生可归属到具体用户的真实流量。
#
# 用法: gen-traffic.sh <配置文件> <SOCKS端口> <字节数>
# 输出: 实际下载的字节数
set -uo pipefail

CONF="${1:?需要配置文件路径}"
PORT="${2:?需要 SOCKS 端口}"
BYTES="${3:?需要字节数}"
BIN=/opt/litebox/sing-box

nohup "$BIN" run -c "$CONF" >/tmp/lb-gen.log 2>&1 &
PID=$!
cleanup() {
    kill "$PID" 2>/dev/null
    wait "$PID" 2>/dev/null
    rm -f "$CONF"
}
trap cleanup EXIT

# 等待 SOCKS 端口就绪。
ready=0
for _ in $(seq 1 30); do
    if ss -tln 2>/dev/null | grep -q ":$PORT "; then ready=1; break; fi
    sleep 0.2
done
if [ "$ready" -ne 1 ]; then
    echo "0"
    echo "探测客户端未能监听 $PORT,日志:" >&2
    tail -5 /tmp/lb-gen.log >&2
    exit 1
fi

# --max-time 必不可少:VLESS 链路不通时 curl 会无限等待,
# 让调用方的 ssh 会话一起挂死。
curl -s --max-time 60 --socks5-hostname "127.0.0.1:$PORT" \
    "https://speed.cloudflare.com/__down?bytes=$BYTES" \
    -o /dev/null -w '%{size_download}'
echo
