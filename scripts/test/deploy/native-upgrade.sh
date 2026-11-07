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
hold_status() { su-exec litebox env "$(cat /etc/litebox/litebox.env)" /opt/litebox/litebox deploy-hold status --config /etc/litebox/litebox.yaml 2>&1 | tail -1; }
# setup_new:装好新版本(支持暂停自动下发)并启动 —— 从它往外的回滚要「先暂停再启动」。
setup_new() {
    setup
    BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 || { echo "装新版本失败"; tail -20 /tmp/u.log; exit 1; }
}

echo "### PG 配置:动任何东西之前就拒绝"
setup
before="$(ls /var/lib/litebox)"
printf '\ndatabase:\n  driver: "postgres"\n' >> /etc/litebox/litebox.yaml
BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "PG 配置不该升级" || ok "拒绝:$(grep 错误 /tmp/u.log | head -1)"
[ "$(ls /var/lib/litebox)" = "$before" ] && systemctl is-active && ok "没有备份、没有停服" || bad "已经动了东西"

for fault in migrate replace-binary; do
    echo "### 从不支持暂停的旧版本升级,$fault 失败:旧二进制 + 最终恢复点的库,主控保持停止"
    setup
    old_schema="$(schema)"
    if [ "$fault" = migrate ]; then
        BINARY=/t/broken-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "迁移失败的升级不该成功" || ok "回滚:$(grep 错误 /tmp/u.log | tail -1 | cut -c1-80)"
    else
        LITEBOX_FAULT=replace-binary BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "换二进制失败的升级不该成功" || ok "回滚:$(grep 错误 /tmp/u.log | tail -1 | cut -c1-80)"
    fi
    systemctl is-active && bad "回滚到不能暂停自动下发的版本之后不该自动启动" || ok "主控保持停止(旧版本不能暂停自动下发)"
    [ -f /opt/litebox/litebox.new ] && bad "暂存的新二进制没清掉" || ok "暂存的新二进制清掉了"
    [ "$(/opt/litebox/litebox version)" = "$(/t/old-litebox version)" ] && ok "二进制是旧的" || bad "二进制不是旧的"
    [ "$(schema)" = "$old_schema" ] && ok "库的结构版本回到 $old_schema(旧二进制能打开)" || bad "库的结构版本 = $(schema),原来 $old_schema"
    systemctl start && curl -fsS http://127.0.0.1:18090/api/health >/dev/null && ok "管理员手动启动之后健康" || bad "手动启动之后不健康"
done

echo "### 从支持暂停的版本升级失败:回滚之后先暂停自动下发再启动"
setup_new
BINARY=/t/broken-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && bad "迁移失败的升级不该成功" || ok "回滚:$(grep 错误 /tmp/u.log | tail -1 | cut -c1-80)"
systemctl is-active && ok "回滚之后在跑" || { bad "回滚之后没起来"; tail -20 /tmp/u.log; }
case "$(hold_status)" in *已暂停*) ok "自动下发处于暂停" ;; *) bad "没有暂停:$(hold_status)" ;; esac

echo "### 原生恢复:先暂停自动下发再启动;暂停写不进去时主控保持停止"
setup_new
su-exec litebox env "$(cat /etc/litebox/litebox.env)" /opt/litebox/litebox backup --config /etc/litebox/litebox.yaml --output /var/lib/litebox/backup/r.db >/dev/null
echo yes | bash /t/restore.sh /var/lib/litebox/backup/r.db > /tmp/r.log 2>&1 && ok "恢复成功" || { bad "恢复失败"; tail -15 /tmp/r.log; }
systemctl is-active && ok "恢复之后在跑" || bad "恢复之后没起来"
case "$(hold_status)" in *已暂停*) ok "恢复之后自动下发处于暂停" ;; *) bad "恢复之后没有暂停:$(hold_status)" ;; esac
echo yes | LITEBOX_FAULT=hold-write bash /t/restore.sh /var/lib/litebox/backup/r.db > /tmp/r.log 2>&1 && bad "暂停写不进去时恢复不该报成功" || ok "恢复报失败:$(grep 错误 /tmp/r.log | tail -1 | cut -c1-80)"
systemctl is-active && bad "暂停写不进去却启动了" || ok "主控保持停止"

echo "### 原生恢复:装的是不支持暂停的旧版本时主控保持停止"
setup
su-exec litebox env "$(cat /etc/litebox/litebox.env)" /opt/litebox/litebox backup --config /etc/litebox/litebox.yaml --output /var/lib/litebox/backup/r.db >/dev/null
echo yes | bash /t/restore.sh /var/lib/litebox/backup/r.db > /tmp/r.log 2>&1 && bad "旧版本恢复不该报成功(它不能暂停自动下发)" || ok "恢复报失败并说明:$(grep -E '没有「暂停' /tmp/r.log | head -1 | cut -c1-80)"
systemctl is-active && bad "旧版本恢复之后不该自动启动" || ok "主控保持停止"

echo "### 正常升级"
setup
BINARY=/t/new-litebox bash /t/upgrade.sh > /tmp/u.log 2>&1 && ok "升级成功" || { bad "升级失败"; tail -15 /tmp/u.log; }
[ "$(/opt/litebox/litebox version)" = "$(/t/new-litebox version)" ] && ok "二进制是新的" || bad "二进制不是新的"
ls /var/lib/litebox/backup/final-*.db >/dev/null 2>&1 && ok "留下了最终恢复点" || bad "没有最终恢复点"

pkill -f "litebox serve" 2>/dev/null
echo
echo "[native] 通过 $PASS 项,失败 $FAIL 项"
[ "$FAIL" = 0 ]
