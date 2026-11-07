#!/usr/bin/env bash
# 原生 scripts/upgrade.sh 的故障注入(alpine 容器里,systemctl / sudo 用替身)。
set -uo pipefail
apk add -q --no-cache bash coreutils curl grep su-exec >/dev/null
adduser -D -H -s /sbin/nologin litebox
PASS=0; FAIL=0
ok() { echo "  ✓ $*"; PASS=$((PASS + 1)); }
bad() { echo "  ✗ $*"; FAIL=$((FAIL + 1)); }

# ---- 替身 ----
cat > /usr/local/bin/sudo <<'EOF'
#!/bin/sh
[ "$1" = "-u" ] || exit 2
u="$2"; shift 2
exec su-exec "$u" "$@"
EOF
cat > /usr/local/bin/systemctl <<'EOF'
#!/bin/sh
# 只认 start / stop / restart / is-active litebox。
pidf=/run/litebox.pid
case "$1" in
  start)
    . /etc/litebox/litebox.env; export LITEBOX_MASTER_KEY
    su-exec litebox /opt/litebox/litebox serve --config /etc/litebox/litebox.yaml >>/var/log/litebox.log 2>&1 &
    echo $! > $pidf; sleep 2; kill -0 "$(cat $pidf)" 2>/dev/null ;;
  stop) [ -f $pidf ] && kill "$(cat $pidf)" 2>/dev/null; sleep 1; rm -f $pidf; exit 0 ;;
  restart) "$0" stop; "$0" start ;;
  is-active) [ -f $pidf ] && kill -0 "$(cat $pidf)" 2>/dev/null ;;
esac
EOF
chmod +x /usr/local/bin/sudo /usr/local/bin/systemctl

setup() {
    pkill -f "litebox serve" 2>/dev/null; rm -f /run/litebox.pid
    rm -rf /opt/litebox /etc/litebox /var/lib/litebox
    mkdir -p /opt/litebox /etc/litebox /var/lib/litebox
    cp /t/old-litebox /opt/litebox/litebox
    chown -R litebox:litebox /var/lib/litebox
    key="$(/opt/litebox/litebox genkey 2>/dev/null)"
    echo "LITEBOX_MASTER_KEY=$key" > /etc/litebox/litebox.env
    cat > /etc/litebox/litebox.yaml <<EOF
http:
  listen: "127.0.0.1:18090"
  base_url: "http://127.0.0.1:18090"
  secure_cookie: false

database:
  path: "/var/lib/litebox/litebox.db"
EOF
    systemctl start
    for _ in $(seq 1 20); do curl -fsS http://127.0.0.1:18090/api/health >/dev/null 2>&1 && return 0; sleep 1; done
    echo "旧版本没起来"; cat /var/log/litebox.log; exit 1
}
schema() { su-exec litebox env "$(cat /etc/litebox/litebox.env)" /opt/litebox/litebox check --config /etc/litebox/litebox.yaml 2>/dev/null | sed -n 's/架构版本 *: *//p'; }

echo "### PG 配置:动任何东西之前就拒绝"
setup
before="$(ls /var/lib/litebox)"
printf '\ndatabase:\n  driver: "postgres"\n' >> /etc/litebox/litebox.yaml
BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "PG 配置不该升级" || ok "拒绝:$(grep 错误 /tmp/u.log | head -1)"
[ "$(ls /var/lib/litebox)" = "$before" ] && systemctl is-active && ok "没有备份、没有停服" || bad "已经动了东西"

echo "### 新版本迁移失败:旧二进制 + 最终恢复点的库"
setup
old_schema="$(schema)"
BINARY=/t/broken-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "迁移失败的升级不该成功" || ok "回滚:$(grep 错误 /tmp/u.log | tail -1 | cut -c1-80)"
systemctl is-active && ok "旧版本在跑" || bad "旧版本没起来"
[ "$(/opt/litebox/litebox version)" = "$(/t/old-litebox version)" ] && ok "二进制是旧的" || bad "二进制不是旧的"
[ "$(schema)" = "$old_schema" ] && ok "库的结构版本回到 $old_schema(旧二进制能打开)" || bad "库的结构版本 = $(schema),原来 $old_schema"
curl -fsS http://127.0.0.1:18090/api/health >/dev/null && ok "健康检查通过" || bad "健康检查失败"

echo "### 正常升级"
setup
BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && ok "升级成功" || { bad "升级失败"; tail -15 /tmp/u.log; }
[ "$(/opt/litebox/litebox version)" = "$(/t/new-litebox version)" ] && ok "二进制是新的" || bad "二进制不是新的"
ls /var/lib/litebox/backup/final-*.db >/dev/null 2>&1 && ok "留下了最终恢复点" || bad "没有最终恢复点"

pkill -f "litebox serve" 2>/dev/null
echo
echo "[native] 通过 $PASS 项,失败 $FAIL 项"
[ "$FAIL" = 0 ]
