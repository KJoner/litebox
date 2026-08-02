#!/bin/bash
# Phase 7:128MB 低配节点压测。
#
# 用 systemd MemoryMax=128M(cgroup v2 硬上限,超限即 OOM kill)模拟低配 VPS,
# 在 10 个用户、多并发下载的负载下测量内存与稳定性。
#
# 关注 cgroup 的 memory.current 而不是 RSS:RSS 里包含 mmap 进来的二进制页面
# (二进制本身就 28MB),而决定是否被 OOM kill 的是 memory.current。
set -uo pipefail
BASE=/opt/litebox
SB=$BASE/sing-box
SVC=litebox-singbox
CONF=$BASE/config.json

log() { printf '\n\033[36m=== %s ===\033[0m\n' "$*"; }

cleanup_clients() {
    pkill -f '/tmp/stress-client' 2>/dev/null
    rm -f /tmp/stress-client-*.json
}
trap cleanup_clients EXIT

set_limit() {
    mkdir -p /etc/systemd/system/$SVC.service.d
    {
        echo "[Service]"
        echo "MemoryMax=$1"
        echo "MemoryAccounting=yes"
    } > /etc/systemd/system/$SVC.service.d/stress.conf
    systemctl daemon-reload
    systemctl restart $SVC
    sleep 4
}

metrics() {
    local pid rss cg
    pid=$(systemctl show -p MainPID --value $SVC)
    rss=$(awk '/^VmRSS/{print $2}' /proc/"$pid"/status 2>/dev/null || echo 0)
    cg=$(cat /sys/fs/cgroup/system.slice/$SVC.service/memory.current 2>/dev/null || echo 0)
    printf '%s|%s' "$rss" "$cg"
}

report() {
    local label=$1 m rss cg
    m=$(metrics); rss=${m%|*}; cg=${m#*|}
    printf '  %-30s RSS=%6.1f MB  cgroup=%6.1f MB\n' \
        "$label" "$(echo "$rss/1024" | bc -l)" "$(echo "$cg/1048576" | bc -l)"
}

# 从节点配置里读出全部用户与 REALITY 参数,为每个用户起一个客户端。
build_clients() {
    python3 - <<'PY'
import json
cfg = json.load(open('/opt/litebox/config.json'))
inb = cfg['inbounds'][0]
reality = inb['tls']['reality']
dest = inb['tls']['server_name']
port = inb['listen_port']

# 公钥不在服务端配置里,压测只需要连得上即可 —— 用服务端私钥推导。
import base64, hashlib
try:
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    raw = base64.urlsafe_b64decode(reality['private_key'] + '==')
    priv = X25519PrivateKey.from_private_bytes(raw)
    pub = priv.public_key().public_bytes(
        encoding=serialization.Encoding.Raw,
        format=serialization.PublicFormat.Raw)
    pubkey = base64.urlsafe_b64encode(pub).decode().rstrip('=')
except Exception as e:
    print('NEEDPUBKEY')
    raise SystemExit(0)

for i, u in enumerate(inb['users']):
    conf = {
        "log": {"level": "error"},
        "inbounds": [{"type": "mixed", "tag": "in", "listen": "127.0.0.1",
                      "listen_port": 40000 + i}],
        "outbounds": [{
            "type": "vless", "tag": "out", "server": "127.0.0.1", "server_port": port,
            "uuid": u['uuid'], "flow": "xtls-rprx-vision",
            "tls": {"enabled": True, "server_name": dest,
                    "utls": {"enabled": True, "fingerprint": "chrome"},
                    "reality": {"enabled": True, "public_key": pubkey,
                                "short_id": reality['short_id'][0]}}}]
    }
    json.dump(conf, open(f'/tmp/stress-client-{i}.json', 'w'))
print(len(inb['users']))
PY
}

log "准备压测客户端"
COUNT=$(build_clients)
if [ "$COUNT" = "NEEDPUBKEY" ] || [ -z "$COUNT" ]; then
    echo "需要 python3-cryptography 才能从私钥推导公钥:"
    echo "  apt-get install -y python3-cryptography"
    exit 1
fi
echo "  节点上有 $COUNT 个用户,已生成对应客户端配置"

run_load() {  # $1=并发轮数 $2=每次字节数
    local rounds=$1 bytes=$2 i pids=()
    for i in $(seq 0 $((COUNT - 1))); do
        nohup "$SB" run -c "/tmp/stress-client-$i.json" >/dev/null 2>&1 &
    done
    # 等所有客户端端口就绪
    for i in $(seq 1 40); do
        local ready=1
        for j in $(seq 0 $((COUNT - 1))); do
            ss -tln 2>/dev/null | grep -q ":$((40000 + j)) " || ready=0
        done
        [ "$ready" -eq 1 ] && break
        sleep 0.25
    done

    local peak_rss=0 peak_cg=0
    for _ in $(seq 1 "$rounds"); do
        for i in $(seq 0 $((COUNT - 1))); do
            curl -s --max-time 45 --socks5-hostname "127.0.0.1:$((40000 + i))" \
                "https://speed.cloudflare.com/__down?bytes=$bytes" -o /dev/null &
            pids+=($!)
        done
    done

    # 负载进行中持续采样峰值
    local pid m rss cg
    pid=$(systemctl show -p MainPID --value $SVC)
    for _ in $(seq 1 60); do
        rss=$(awk '/^VmRSS/{print $2}' /proc/"$pid"/status 2>/dev/null || echo 0)
        cg=$(cat /sys/fs/cgroup/system.slice/$SVC.service/memory.current 2>/dev/null || echo 0)
        [ "${rss:-0}" -gt "$peak_rss" ] && peak_rss=$rss
        [ "${cg:-0}" -gt "$peak_cg" ] && peak_cg=$cg
        sleep 0.3
    done
    wait "${pids[@]}" 2>/dev/null
    pkill -f '/tmp/stress-client' 2>/dev/null

    printf '  %-30s 峰值RSS=%6.1f MB  峰值cgroup=%6.1f MB\n' \
        "$COUNT 用户 × $rounds 轮 × $((bytes/1000000))MB" \
        "$(echo "$peak_rss/1024" | bc -l)" "$(echo "$peak_cg/1048576" | bc -l)"
}

log "基线:无内存限制"
rm -f /etc/systemd/system/$SVC.service.d/stress.conf 2>/dev/null
systemctl daemon-reload; systemctl restart $SVC; sleep 4
report "空闲"
run_load 1 2000000
report "负载后"

log "限制 MemoryMax=128M"
set_limit 128M
report "空闲(限 128M)"
RESTARTS_BEFORE=$(systemctl show -p NRestarts --value $SVC)
run_load 2 3000000
report "负载后"
run_load 3 2000000
report "更高并发后"
RESTARTS_AFTER=$(systemctl show -p NRestarts --value $SVC)

echo
echo "  服务状态    : $(systemctl is-active $SVC)"
echo "  重启次数    : $RESTARTS_BEFORE -> $RESTARTS_AFTER"
OOM=$(journalctl -u $SVC --since '10 min ago' --no-pager 2>/dev/null | grep -ci 'out of memory\|oom' || true)
echo "  OOM 相关日志: $OOM 条"

log "限制 MemoryMax=64M(探底)"
set_limit 64M
if systemctl is-active --quiet $SVC; then
    report "空闲(限 64M)"
    run_load 2 2000000
    report "负载后"
    echo "  服务状态: $(systemctl is-active $SVC)"
else
    echo "  64M 下无法启动"
fi

log "恢复无限制"
rm -f /etc/systemd/system/$SVC.service.d/stress.conf
systemctl daemon-reload
systemctl restart $SVC
sleep 3
echo "  服务状态: $(systemctl is-active $SVC)"

log "系统整体"
free -m | sed 's/^/  /'
