#!/bin/bash
# Phase 0 收尾:移除测试服务与临时资源,保留验证数据供归档。
# 不触碰机器上原有的 /usr/local/bin/sing-box 及其服务。
set -uo pipefail
SVC=litebox-phase0

for i in 1 2 3; do
    [ -f /opt/litebox-phase0/logs/client$i.pid ] && kill "$(cat /opt/litebox-phase0/logs/client$i.pid)" 2>/dev/null
done
pkill -f 'litebox-phase0/conf/client' 2>/dev/null

systemctl stop $SVC 2>/dev/null
systemctl disable $SVC 2>/dev/null
rm -f /etc/systemd/system/$SVC.service
rm -rf /etc/systemd/system/$SVC.service.d
systemctl daemon-reload
systemctl reset-failed 2>/dev/null

echo "已移除 systemd 单元 $SVC"
echo
echo "验证数据保留在 /opt/litebox-phase0(可随时 rm -rf 删除):"
du -sh /opt/litebox-phase0
echo
echo "残留监听端口检查(应无 24443/28080/2108x):"
ss -tlnp | grep -E '24443|28080|2108' || echo "  无残留"
echo
echo "原有 sing-box 服务状态(应不受影响):"
ss -tlnp | grep 'sing-box' | head -3
echo
echo "系统内存:"
free -m | head -2
