#!/usr/bin/env bash
# temp_pg_start 的错误传播(G04):不起 Docker、不碰数据库,用一个本地函数替身代替 docker。
#
#   bash scripts/test/deploy/temp-pg-unit.sh
#
# 从 deploy/docker/litebox.sh 里原样取出 temp_pg_start / temp_pg_stop,在 `if temp_pg_start …` 里调用
# (与两个真实调用方一样,set -e 在那里不生效),逐个让网络、容器、就绪、复制、pg_restore 失败,
# 断言函数返回失败、TMP_DSN 没有被设上,并且报出了是哪一步。
set -euo pipefail
cd "$(dirname "$0")/../../.."
SRC=${SRC:-deploy/docker/litebox.sh}
PASS=0; FAIL=0
ok() { echo "  ✓ $*"; PASS=$((PASS + 1)); }
bad() { echo "  ✗ $*"; FAIL=$((FAIL + 1)); }

eval "$(sed -n '/^TMP_NET=""; TMP_PG=""; TMP_DSN=""$/,/^}$/p' "$SRC")"
eval "$(sed -n '/^temp_pg_stop() {$/,/^}$/p' "$SRC")"
pg_client_image() { echo postgres:17-alpine; }
sleep() { :; }
export LITEBOX_TEMP_PG_WAIT=3

FAIL_AT=""
docker() {
    case "$*" in
        "network create"*) [ "$FAIL_AT" = network ] && return 1 ;;
        "run -d"*) [ "$FAIL_AT" = run ] && return 125 ;;
        *"psql"*) [ "$FAIL_AT" = ready ] && return 2 ;;
        "cp "*) [ "$FAIL_AT" = cp ] && return 1 ;;
        *"pg_restore"*) [ "$FAIL_AT" = restore ] && { echo 'pg_restore: error: could not read from input file: end of file' >&2; return 42; } ;;
    esac
    return 0
}

for at in network run ready cp restore; do
    FAIL_AT="$at"
    err="$( { if temp_pg_start /does/not/exist.pgdump; then echo "RESULT=ok DSN=$TMP_DSN"; else echo "RESULT=fail DSN=$TMP_DSN"; fi; } 2>&1 )"
    case "$err" in
        *"RESULT=fail DSN="$'\n'*|*"RESULT=fail DSN=") ok "$at 失败 → 函数失败、没有连接串:$(echo "$err" | grep '临时 PG' | head -1)" ;;
        *) bad "$at 失败时函数没有失败:$err" ;;
    esac
    temp_pg_stop
done

FAIL_AT=""
if temp_pg_start /does/not/exist.pgdump && [ -n "$TMP_DSN" ]; then ok "全部成功 → 函数成功、设好连接串"; else bad "全部成功时函数失败"; fi
temp_pg_stop

echo
echo "[temp_pg_start] 通过 $PASS 项,失败 $FAIL 项"
[ "$FAIL" = 0 ]
