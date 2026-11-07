#!/usr/bin/env bash
# R03 / C03 端到端故障注入(由 run.sh 调用;在 docker:cli 容器里跑,数据目录在卷的宿主真实路径上 —— 真实的 Linux 权限语义)。
# 用法:r03test.sh <sqlite|postgres|external-postgres>
set -uo pipefail
MODE="$1"
ROOT=/var/lib/docker/volumes/lbr03/_data
DIR="$ROOT/$MODE"
SH="$ROOT/litebox.sh"
OLD=v23.0.0-rc.1
NEW=v23.1.0-test
export LITEBOX_SKIP_PULL=1
PASS=0; FAIL=0
ok() { echo "  ✓ $*"; PASS=$((PASS + 1)); }
bad() { echo "  ✗ $*"; FAIL=$((FAIL + 1)); }
step() { echo; echo "### $*"; }
lb() { bash "$SH" "$@" --dir "$DIR"; }
env_version() { sed -n 's/^LITEBOX_VERSION=//p' "$DIR/.env" | tail -1; }
healthy() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$(cd "$DIR" && docker compose $(compose_args) ps -q litebox)" 2>/dev/null)" = healthy ]; }
compose_args() {
    local a=(-f compose.yaml)
    case "$MODE" in postgres) a+=(-f compose.postgres.yaml) ;; external-postgres) a+=(-f compose.external-postgres.yaml -f compose.pg-network.yaml) ;; esac
    echo "${a[@]}"
}
dc() { (cd "$DIR" && docker compose $(compose_args) "$@"); }
running() { [ -n "$(dc ps -q --status running litebox 2>/dev/null)" ]; }
held() { api http://litebox:8080/api/sidebar-counts | grep -q '"held":true'; }

COOKIE=/tmp/cj-$MODE
api() { curl -s -b "$COOKIE" -c "$COOKIE" -H 'Content-Type: application/json' "$@"; }
login() {
    local pw
    pw="$(dc logs litebox 2>/dev/null | sed -n 's/.*初始密码:\(.*\)/\1/p' | tail -1 | tr -d ' \r')"
    [ -n "$pw" ] || pw="$(cat "$ROOT/pw-$MODE" 2>/dev/null)"
    echo "$pw" > "$ROOT/pw-$MODE"
    rm -f "$COOKIE"
    api -o /dev/null -w '%{http_code}' -X POST http://litebox:8080/api/auth/login -d "{\"username\":\"admin\",\"password\":\"$pw\"}"
}
user_names() { api http://litebox:8080/api/users | sed 's/"display_name":"/\n/g' | sed -n '2,$p' | cut -d'"' -f1 | sort | tr '\n' ' '; }
connect_net() { docker network connect litebox_default "$(hostname)" 2>/dev/null || true; }

cleanup() {
    (cd "$DIR" 2>/dev/null && docker compose $(compose_args) down -v >/dev/null 2>&1) || true
    rm -rf "$DIR"
}

step "[$MODE] 准备"
cleanup
mkdir -p "$DIR"
EXTRA=()
if [ "$MODE" = external-postgres ]; then
    docker rm -f sharedpg >/dev/null 2>&1; docker network rm sharedpg-net >/dev/null 2>&1
    docker network create sharedpg-net >/dev/null
    docker run -d --name sharedpg --network sharedpg-net -e POSTGRES_PASSWORD=adminpw postgres:17-alpine >/dev/null
    for i in $(seq 1 60); do docker exec sharedpg psql -h 127.0.0.1 -U postgres -tAc 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done
    # 共享实例上另一个应用的库:恢复时一个字节都不能碰它。
    docker exec sharedpg psql -U postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE otherapp LOGIN PASSWORD 'o'" -c "CREATE DATABASE otherapp OWNER otherapp" >/dev/null
    docker exec sharedpg psql -U otherapp -d otherapp -c "CREATE TABLE keep_me (id int); INSERT INTO keep_me VALUES (42)" >/dev/null
    docker exec sharedpg psql -U postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE lbapp LOGIN PASSWORD 'lbpw' NOSUPERUSER NOCREATEDB" -c "CREATE DATABASE lbapp OWNER lbapp" >/dev/null
    echo "postgres://lbapp:lbpw@sharedpg:5432/lbapp?sslmode=disable" > "$ROOT/dsn-ext"
    EXTRA=(--dsn-file "$ROOT/dsn-ext" --pg-network sharedpg-net)
fi

# 测试里走明文 HTTP:Secure 的会话 Cookie 不会被 curl 送回去。
echo "LITEBOX_SECURE_COOKIE=false" > "$DIR/.env"

step "[$MODE] 安装 $OLD"
if lb install --mode "$MODE" --version "$OLD" --base-url https://panel.example.com --port 18080 "${EXTRA[@]}" >/tmp/install.log 2>&1; then
    ok "安装成功"
else
    bad "安装失败:"; tail -20 /tmp/install.log
    exit 1
fi
if [ "$MODE" = postgres ]; then
    perm="$(stat -c '%u %a' "$DIR/secrets/pg_app_password")"
    [ "$perm" = "70 400" ] && ok "pg_app_password 属主 70、权限 400(C03)" || bad "pg_app_password = $perm"
    perm="$(stat -c '%u %a' "$DIR/secrets/master_key")"
    [ "$perm" = "10001 400" ] && ok "master_key 属主 10001、权限 400" || bad "master_key = $perm"
fi
connect_net
[ "$(login)" = 200 ] && ok "登录" || bad "登录失败"
api -o /dev/null -X POST http://litebox:8080/api/users -d '{"display_name":"甲","quota_bytes":0,"reset_cycle":"NONE"}'
[[ "$(user_names)" == *甲* ]] && ok "建了用户甲" || bad "建用户甲失败"

# 升级源 rc.1 没有「暂停自动下发」:停服之后才失败的那几种回滚,主控**保持停止**(G03),
# 由管理员核对之后 start --without-hold;迁移演练失败那一种在停服之前就拒绝,主控一直在跑。
for broken in broken-migrate broken-up broken-health apply-files; do
    target="v23.1.0-$broken"; fault=""
    if [ "$broken" = apply-files ]; then target="$NEW"; fault=apply-files; fi
    step "[$MODE] 升级到 $target(故障注入:$broken)"
    LITEBOX_FAULT="$fault" lb update --version "$target" >/tmp/up.log 2>&1 && bad "注入了故障的升级不该成功" || ok "升级被拒绝或回滚:$(grep -E '错误' /tmp/up.log | tail -1)"
    [ "$(env_version)" = "$OLD" ] && ok ".env 版本回到 $OLD" || bad ".env 版本 = $(env_version)"
    if [ "$broken" = broken-migrate ]; then
        healthy && ok "演练失败在停服之前:旧版本一直 healthy" || { bad "旧版本没有 healthy"; tail -30 /tmp/up.log; }
    else
        running && bad "回滚到不支持暂停的 $OLD 之后不该自动启动" || ok "回滚之后主控保持停止(旧版本不能暂停自动下发)"
        grep -q 'start --without-hold' /tmp/up.log && ok "提示了下一步:start --without-hold" || bad "没有提示下一步"
        lb start >/tmp/st.log 2>&1 && bad "不带 --without-hold 不该启动" || ok "不带 --without-hold 拒绝启动"
        running && bad "被拒绝之后主控却在跑" || ok "仍然停着"
        lb start --without-hold >/tmp/st.log 2>&1 && ok "start --without-hold 启动" || { bad "start --without-hold 失败"; tail -20 /tmp/st.log; }
        healthy && ok "旧版本 healthy" || bad "旧版本没有 healthy"
    fi
    [ -d "$DIR/.deploy-$target" ] && bad "暂存的部署文件没清掉" || true
    connect_net
    [ "$(login)" = 200 ] && ok "回滚后还能登录(口令与凭据一致)" || bad "回滚后登录失败"
    [[ "$(user_names)" == *甲* ]] && ok "用户甲还在" || bad "用户甲丢了"
done
if [ "$MODE" = external-postgres ]; then
    [ "$(docker exec sharedpg psql -U otherapp -d otherapp -tAc 'SELECT id FROM keep_me')" = 42 ] && ok "共享实例上别的应用的库没被碰" || bad "别的应用的库被改了"
fi

step "[$MODE] 正常升级到 $NEW"
if lb update --version "$NEW" >/tmp/up.log 2>&1; then ok "升级成功"; else bad "升级失败"; tail -30 /tmp/up.log; fi
[ "$(env_version)" = "$NEW" ] && ok ".env 版本 $NEW" || bad ".env 版本 = $(env_version)"
connect_net
[ "$(login)" = 200 ] && ok "升级后能登录" || bad "升级后登录失败"
[[ "$(user_names)" == *甲* ]] && ok "用户甲还在" || bad "用户甲丢了"

step "[$MODE] 从 $NEW 升级到坏版本:回滚到支持暂停的 $NEW,先暂停再自动启动"
lb update --version v23.1.0-broken-health >/tmp/up.log 2>&1 && bad "坏版本的升级不该成功" || ok "回滚:$(grep -E '错误' /tmp/up.log | tail -1)"
[ "$(env_version)" = "$NEW" ] && ok ".env 版本回到 $NEW" || bad ".env 版本 = $(env_version)"
healthy && ok "$NEW healthy" || { bad "$NEW 没有 healthy"; tail -30 /tmp/up.log; }
connect_net
[ "$(login)" = 200 ] && ok "回滚后能登录" || bad "回滚后登录失败"
held && ok "回滚之后自动下发处于暂停" || bad "回滚之后没有暂停自动下发"
api -o /dev/null -X DELETE http://litebox:8080/api/deploy-hold
held && bad "解除之后仍是暂停" || ok "解除暂停"

step "[$MODE] 备份并验证"
if lb backup >/tmp/bk.log 2>&1; then ok "备份并验证通过:$(grep 已验证 /tmp/bk.log | tail -1)"; else bad "备份失败"; tail -20 /tmp/bk.log; fi
BK="$(ls -1t "$DIR"/backups/litebox-*.tar.gz | head -1)"
tar -tzf "$BK" > /tmp/bk.list 2>&1
grep -q manifest.txt /tmp/bk.list && ok "备份里有清单($(tr '
' ' ' < /tmp/bk.list | head -c 200))" || bad "备份里没有清单:$(head -5 /tmp/bk.list)"

step "[$MODE] 建用户乙之后恢复备份"
api -o /dev/null -X POST http://litebox:8080/api/users -d '{"display_name":"乙","quota_bytes":0,"reset_cycle":"NONE"}'
[[ "$(user_names)" == *乙* ]] && ok "建了用户乙" || bad "建用户乙失败"
if lb restore "$BK" >/tmp/rs.log 2>&1; then ok "恢复成功"; else bad "恢复失败"; tail -20 /tmp/rs.log; fi
connect_net
[ "$(login)" = 200 ] && ok "恢复后能登录" || bad "恢复后登录失败"
names="$(user_names)"
[[ "$names" == *甲* && "$names" != *乙* ]] && ok "回到恢复点:甲在、乙不在" || bad "恢复后用户 = $names"
held && ok "恢复之后自动下发处于暂停" || bad "恢复之后没有暂停自动下发"
api -o /dev/null -w '%{http_code}' -X DELETE http://litebox:8080/api/deploy-hold | grep -q 200 && ok "解除暂停" || bad "解除暂停失败"
held && bad "解除之后仍是暂停" || ok "解除之后恢复正常"

step "[$MODE] 恢复时暂停写不进去:数据恢复了,主控保持停止(G03)"
LITEBOX_FAULT=hold-write lb restore "$BK" >/tmp/rs.log 2>&1 && bad "暂停写不进去时恢复不该报成功" || ok "恢复报失败:$(grep 错误 /tmp/rs.log | tail -1)"
running && bad "暂停写不进去却启动了主控" || ok "主控保持停止"
lb start >/tmp/st.log 2>&1 && ok "排查之后 start:先暂停再启动" || { bad "start 失败"; tail -20 /tmp/st.log; }
connect_net
[ "$(login)" = 200 ] && ok "启动后能登录" || bad "启动后登录失败"
held && ok "start 之后自动下发处于暂停" || bad "start 之后没有暂停自动下发"
api -o /dev/null -X DELETE http://litebox:8080/api/deploy-hold
if [ "$MODE" = external-postgres ]; then
    [ "$(docker exec sharedpg psql -U otherapp -d otherapp -tAc 'SELECT id FROM keep_me')" = 42 ] && ok "恢复没碰共享实例上别的应用的库" || bad "别的应用的库被改了"
    ls "$DIR"/backups/before-restore-*.pgdump >/dev/null 2>&1 && ok "恢复前把当前应用库导出了一份" || bad "没有恢复前的安全导出"
fi

step "[$MODE] 数据只恢复了一半的备份:验证失败,停主控之前就拒绝(G04)"
half="$(mktemp -d -p "$ROOT")"
tar -C "$half" -xzf - < "$BK"
for f in litebox.pgdump litebox.db; do
    if [ -f "$half/$f" ]; then
        # 结构在前、数据在后:截掉后半截就是「结构大体在、数据不全」。
        sz="$(stat -c %s "$half/$f")"; head -c $((sz * 2 / 3)) "$half/$f" > "$half/$f.cut" && mv "$half/$f.cut" "$half/$f"
    fi
done
tar -C "$half" -czf - . > "$DIR/backups/litebox-half.tar.gz"; rm -rf "$half"
lb verify-backup "$DIR/backups/litebox-half.tar.gz" >/tmp/vb.log 2>&1 && bad "只恢复了一半的备份不该通过验证" || ok "验证失败:$(grep -E 'pg_restore|临时 PG|错误|Error|error' /tmp/vb.log | head -1 | cut -c1-120)"
lb restore "$DIR/backups/litebox-half.tar.gz" >/tmp/rs.log 2>&1 && bad "只恢复了一半的备份不该恢复" || ok "恢复被拒绝"
healthy && ok "主控仍然 healthy(没被停)" || bad "主控被停了"
connect_net
[ "$(login)" = 200 ] && [[ "$(user_names)" == *甲* ]] && ok "生产库没被动:还能登录、用户甲还在" || bad "生产库被动了"

step "[$MODE] 损坏的备份:停主控之前就拒绝"
head -c 2000 "$BK" > "$DIR/backups/litebox-broken.tar.gz"
lb restore "$DIR/backups/litebox-broken.tar.gz" >/tmp/rs.log 2>&1 && bad "损坏的备份不该恢复" || ok "拒绝:$(tail -1 /tmp/rs.log)"
healthy && ok "主控仍然 healthy(没被停)" || bad "主控被停了"

echo
echo "[$MODE] 通过 $PASS 项,失败 $FAIL 项"
cleanup
[ "$MODE" = external-postgres ] && { docker rm -f sharedpg >/dev/null 2>&1; docker network rm sharedpg-net >/dev/null 2>&1; }
[ "$FAIL" = 0 ]
