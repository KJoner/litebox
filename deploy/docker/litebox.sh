#!/usr/bin/env bash
# LiteBox 主控的 Docker 一键部署与运维(VNext DEPLOY03 / DEPLOY06–08,R03)。
#
#   litebox.sh install  [--dir DIR] [--mode sqlite|postgres|external-postgres] [--version vX.Y.Z]
#                       [--base-url https://panel.example.com] [--port 8080] [--dsn-file FILE] [--pg-network NAME]
#   litebox.sh status   [--dir DIR]
#   litebox.sh backup   [--dir DIR]                 # 应用数据 + 配置 + 密钥,保留最近 3 份已验证的
#   litebox.sh verify-backup FILE [--dir DIR]       # 在隔离容器里真的恢复一遍:库能读、结构完整、密文能解
#   litebox.sh restore  FILE [--exact] [--dir DIR]  # 停主控 → 当前数据留一份 → 恢复 → 暂停自动下发 → 启动
#   litebox.sh update   [--version vX.Y.Z] [--dir DIR]
#   litebox.sh start    [--without-hold] [--dir DIR]   # 恢复 / 回滚之后主控停着时由管理员启动(先暂停自动下发)
#   litebox.sh logs     [--dir DIR]
#
# restore 的两种范围:
#   默认      只恢复应用数据与主密钥,保留当前的 .env(版本、站点根)、compose.override.yaml 与连接配置;
#   --exact   精确回到恢复点:再加上备份里的 .env、compose.override.yaml 与 SQLite 数据目录里的其他文件。
#   两种都**保留当前的数据库连接串与 PG 口令**:数据是恢复进当前这个实例的,口令在实例里,不在备份的库里。
#
# 规矩:
#   - 重复执行沿用已有的 .env、secrets/ 与 compose.override.yaml,**绝不重新生成主密钥、不覆盖数据**;
#   - 版本相同且健康时不重启(docker compose up -d 对没变的服务什么都不做);
#   - 已有 PG 模式不创建、不修改、不删除共享实例上的任何东西 —— 只动 LiteBox 应用库里应用账号自己的对象;
#   - 恢复之后**自动下发是暂停的**:库是一份旧快照,快照之后撤掉的凭据会被一起推回节点。核对完在面板上解除;
#   - 主控不用 privileged、不用 host 网络、不挂 Docker socket,以非 root(10001)运行。
set -euo pipefail

IMAGE_DEFAULT="kjoner/litebox"
DIR="/opt/litebox-docker"
MODE=""
VERSION=""
BASE_URL=""
PORT=""
DSN_FILE=""
PG_NETWORK=""
EXACT=0
WITHOUT_HOLD=0
KEEP_BACKUPS=3
# 快捷 PG 镜像里 postgres 用户的 uid(alpine 变体):初始化脚本以它的身份读应用口令文件(C03)。
PG_UID=70

die() { echo "错误:$*" >&2; exit 1; }
info() { echo "==> $*"; }
warn() { echo "!!  $*" >&2; }

cmd="${1:-}"; [ -n "$cmd" ] && shift || true
POS=()
while [ $# -gt 0 ]; do
    case "$1" in
        --dir) DIR="$2"; shift 2 ;;
        --mode) MODE="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --base-url) BASE_URL="$2"; shift 2 ;;
        --port) PORT="$2"; shift 2 ;;
        --dsn-file) DSN_FILE="$2"; shift 2 ;;
        --pg-network) PG_NETWORK="$2"; shift 2 ;;
        --exact) EXACT=1; shift ;;
        --without-hold) WITHOUT_HOLD=1; shift ;;
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
        *) POS+=("$1"); shift ;;
    esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "缺少 $1"; }
need docker
docker compose version >/dev/null 2>&1 || die "缺少 docker compose 插件(v2)"

envget() { [ -f "$DIR/.env" ] && sed -n "s/^$1=//p" "$DIR/.env" | tail -1 || true; }
envset() {
    local key="$1" val="$2"
    touch "$DIR/.env"
    if grep -q "^$key=" "$DIR/.env"; then
        # 值里可能有 / & 这些 sed 特殊字符:整行换掉而不是 s///。
        awk -v k="$key" -v v="$val" 'BEGIN{FS=OFS="="} $1==k{print k"="v; next} {print}' "$DIR/.env" > "$DIR/.env.tmp"
        mv "$DIR/.env.tmp" "$DIR/.env"
    else
        printf '%s=%s\n' "$key" "$val" >> "$DIR/.env"
    fi
    chmod 600 "$DIR/.env"
}

image_ref() { echo "$(envget LITEBOX_IMAGE || true)" | grep . || echo "$IMAGE_DEFAULT"; }
mode() { local m; m="$(envget LITEBOX_MODE)"; echo "${m:-sqlite}"; }

compose_files() {
    local files=(-f compose.yaml)
    case "$(mode)" in
        postgres) files+=(-f compose.postgres.yaml) ;;
        external-postgres)
            files+=(-f compose.external-postgres.yaml)
            [ -n "$(envget LITEBOX_PG_NETWORK)" ] && files+=(-f compose.pg-network.yaml)
            ;;
    esac
    [ -f "$DIR/compose.override.yaml" ] && files+=(-f compose.override.yaml)
    echo "${files[@]}"
}

dc() { (cd "$DIR" && docker compose $(compose_files) "$@"); }

# extract_deploy_files REF [DEST]:从指定版本的镜像里取出同版本的 Compose 文件与初始化脚本(默认放进 $DIR)。
# 每一步都显式判断:它会在 `if` 里被调用,那时 set -e 不生效(G04 同一类问题)。
DEPLOY_FILES="compose.yaml compose.postgres.yaml compose.external-postgres.yaml compose.pg-network.yaml pg-init/10-litebox.sh"
extract_deploy_files() {
    local ref="$1" dest="${2:-$DIR}" cid f
    cid="$(docker create "$ref")" || { echo "从 $ref 取部署文件失败:建不出临时容器" >&2; return 1; }
    mkdir -p "$dest/pg-init" || { docker rm "$cid" >/dev/null 2>&1; return 1; }
    for f in $DEPLOY_FILES; do
        if ! docker cp "$cid:/opt/litebox/deploy/$f" "$dest/$f" >/dev/null; then
            echo "从 $ref 取 $f 失败" >&2
            docker rm "$cid" >/dev/null 2>&1
            return 1
        fi
    done
    docker rm "$cid" >/dev/null 2>&1 || true
}

# copy_deploy_files SRC DEST:把一套部署文件整套复制过去(升级前暂存的、回滚时另存的)。
copy_deploy_files() {
    local src="$1" dest="$2" f
    mkdir -p "$dest/pg-init" || return 1
    for f in $DEPLOY_FILES; do
        [ -f "$src/$f" ] || { echo "部署文件里缺 $f($src)" >&2; return 1; }
        { cp "$src/$f" "$dest/$f.new" && mv "$dest/$f.new" "$dest/$f"; } || { echo "放置 $f 失败" >&2; return 1; }
    done
}

# fault_point NAME:只给故障注入回归用(scripts/test/deploy/)—— LITEBOX_FAULT=NAME 时在这一点上失败。
fault_point() {
    if [ "${LITEBOX_FAULT:-}" = "$1" ]; then
        echo "故障注入:$1" >&2
        return 1
    fi
    return 0
}

random_secret() { head -c 24 /dev/urandom | base64 | tr -d '/+=\n' | head -c 32; }

# workdir:临时目录建在部署目录里(700),不放 /tmp —— 里面有备份解开的主密钥与整份库,
# /tmp 常常是谁都能列的、或者是装不下一个大库的 tmpfs;而且要拿去做绑定挂载的路径必须在宿主上。
workdir() {
    mkdir -p "$DIR/.tmp"
    chmod 700 "$DIR/.tmp"
    mktemp -d "$DIR/.tmp/w.XXXXXX"
}

# readable_by_app FILE:让容器里的主控(10001)读得到一份临时拷贝出来的密钥。
readable_by_app() { chown 10001:10001 "$1" 2>/dev/null || chmod a+r "$1"; }

wait_healthy() {
    local i state cid
    for i in $(seq 1 60); do
        cid="$(dc ps -q litebox 2>/dev/null || true)"
        state="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || true)"
        [ "$state" = "healthy" ] && return 0
        sleep 3
    done
    return 1
}

# fix_secret_perms 按组件给最小的读权限(C03)。不改成全局可读:
#   master_key / db_dsn       主控(10001)读;
#   pg_app_password           快捷 PG 的初始化脚本以 postgres(uid 70)读;
#   pg_admin_password         PG 镜像入口在降权之前以 root 读,属主留 root。
# 换了快捷 PG 镜像(LITEBOX_PG_IMAGE)且它的 postgres 不是 uid 70 时,用 LITEBOX_PG_UID 指定。
fix_secret_perms() {
    local s="$DIR/secrets" uid="${LITEBOX_PG_UID:-$PG_UID}" f
    [ -d "$s" ] || return 0
    chmod 700 "$s"
    for f in master_key db_dsn; do
        [ -f "$s/$f" ] || continue
        chmod 400 "$s/$f"; chown 10001:10001 "$s/$f" 2>/dev/null || true
    done
    if [ -f "$s/pg_app_password" ]; then chmod 400 "$s/pg_app_password"; chown "$uid:$uid" "$s/pg_app_password" 2>/dev/null || true; fi
    if [ -f "$s/pg_admin_password" ]; then chmod 400 "$s/pg_admin_password"; chown 0:0 "$s/pg_admin_password" 2>/dev/null || true; fi
    return 0
}

# ---------------------------------------------------------------- PG 客户端

# pg_client 在一个临时的 PG 客户端容器里跑命令;连接串在容器里的 /lbsecrets/db_dsn(只读)。
# 连接串里引用的 CA / 客户端证书要能在容器里读到:默认把整个 secrets/ 挂在 /lbsecrets,
# 另外的挂载用 LITEBOX_PG_CLIENT_ARGS 给(例如 "-v /etc/pki/pg-ca.crt:/lbsecrets/pg-ca.crt:ro")。
pg_client_image() {
    if [ "$(mode)" = "postgres" ]; then echo "${LITEBOX_PG_IMAGE:-postgres:17-alpine}"; else echo "${LITEBOX_PG_CLIENT_IMAGE:-postgres:17-alpine}"; fi
}
pg_client() {
    local net=() mounts=() f
    [ -n "$(envget LITEBOX_PG_NETWORK)" ] && net=(--network "$(envget LITEBOX_PG_NETWORK)")
    # 每个秘密文件再按 Compose 的命名约定挂到 /run/secrets/litebox_<文件名>(C03):连接串里写的
    # sslrootcert / sslcert / sslkey 是主控容器里的路径,临时客户端里同一个路径要能读到同一个文件。
    for f in "$DIR"/secrets/*; do
        [ -f "$f" ] && mounts+=(-v "$f:/run/secrets/litebox_$(basename "$f"):ro")
    done
    # shellcheck disable=SC2086
    docker run --rm -i "${net[@]}" -v "$DIR/secrets:/lbsecrets:ro" "${mounts[@]}" ${LITEBOX_PG_CLIENT_ARGS:-} \
        "$(pg_client_image)" sh -c "$1"
}

# pg_client_check:已有 PG 模式的备份 / 恢复要求 pg_dump 的主版本不低于服务端(C03 的版本相容预检)。
pg_client_check() {
    pg_client 'set -e
        dsn="$(cat /lbsecrets/db_dsn)"
        cv=$(pg_dump --version | sed -E "s/[^0-9]*([0-9]+).*/\1/")
        sv=$(psql "$dsn" -tAc "SHOW server_version_num")
        sv=$((sv / 10000))
        if [ "$cv" -lt "$sv" ]; then echo "pg_dump $cv 比服务端 $sv 旧:设 LITEBOX_PG_CLIENT_IMAGE=postgres:$sv-alpine" >&2; exit 1; fi
        echo "PG 客户端 $cv / 服务端 $sv"'
}

# temp_pg_start DUMP:在一个 --internal 网络(不通外网)里起一个临时 PG,把 DUMP 恢复进去。
# 设好 TMP_NET / TMP_PG / TMP_DSN;用完调 temp_pg_stop。
TMP_NET=""; TMP_PG=""; TMP_DSN=""
# 它在调用方的 `if temp_pg_start …` 里跑,set -e 在函数里不生效(G04):每一步都必须显式判断,
# 否则 pg_restore 失败之后后面那行赋值照样成功,一份只恢复了一半的备份会被报告成「可用」。
temp_pg_start() {
    local dump="$1" i ready=0 rc=0
    TMP_NET="litebox-verify-$$-$RANDOM"
    TMP_PG="$TMP_NET-pg"
    TMP_DSN=""
    docker network create --internal "$TMP_NET" >/dev/null || { echo "临时 PG:建隔离网络失败" >&2; return 1; }
    docker run -d --name "$TMP_PG" --network "$TMP_NET" -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=litebox \
        "$(pg_client_image)" >/dev/null || { echo "临时 PG:容器起不来" >&2; return 1; }
    # 入口脚本初始化时会先起一个只听 Unix socket 的临时实例:等到 TCP 也通了才算就绪。
    for i in $(seq 1 "${LITEBOX_TEMP_PG_WAIT:-60}"); do
        if docker exec "$TMP_PG" psql -h 127.0.0.1 -U postgres -d litebox -tAc 'SELECT 1' >/dev/null 2>&1; then
            ready=1
            break
        fi
        sleep 1
    done
    [ "$ready" = 1 ] || { echo "临时 PG:${LITEBOX_TEMP_PG_WAIT:-60} 秒内没有就绪" >&2; return 1; }
    docker cp "$dump" "$TMP_PG:/tmp/restore.pgdump" >/dev/null || { echo "临时 PG:把备份复制进去失败" >&2; return 1; }
    docker exec "$TMP_PG" pg_restore -U postgres -d litebox --no-owner --no-privileges --exit-on-error /tmp/restore.pgdump || rc=$?
    if [ "$rc" != 0 ]; then
        echo "临时 PG:pg_restore 失败(退出码 $rc)—— 这份备份恢复不完整,结构可能在、数据不全" >&2
        return 1
    fi
    TMP_DSN="postgres://postgres:verify@$TMP_PG:5432/litebox?sslmode=disable"
}
temp_pg_stop() {
    if [ -n "$TMP_PG" ]; then docker rm -f "$TMP_PG" >/dev/null 2>&1 || true; fi
    if [ -n "$TMP_NET" ]; then docker network rm "$TMP_NET" >/dev/null 2>&1 || true; fi
    TMP_NET=""; TMP_PG=""; TMP_DSN=""
}

# ---------------------------------------------------------------- 应用库的导出与恢复

# dump_db WORK [REF]:把应用库导出到 WORK 目录(litebox.db 或 litebox.pgdump)。
# 主控在跑时 SQLite 走它自己的一致性快照(VACUUM INTO);主控停着时用一次性容器做同样的事(REF 是那个容器用的版本)。
dump_db() {
    local work="$1" ref="${2:-}" ts
    [ -n "$ref" ] || ref="$(image_ref):$(envget LITEBOX_VERSION)"
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    case "$(mode)" in
        sqlite)
            mkdir -p "$DIR/data/backup"
            chown 10001:10001 "$DIR/data/backup" 2>/dev/null || true
            if [ -n "$(dc ps -q --status running litebox 2>/dev/null || true)" ]; then
                dc exec -T litebox litebox backup --config /data/litebox.yaml --output "/data/backup/snap-$ts.db" >/dev/null
            else
                docker run --rm --network none -v "$DIR/data:/data" -v "$DIR/secrets/master_key:/run/secrets/k:ro" \
                    -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k "$ref" backup --config /data/litebox.yaml \
                    --output "/data/backup/snap-$ts.db" >/dev/null
            fi
            mv "$DIR/data/backup/snap-$ts.db" "$work/litebox.db"
            # 数据目录里库以外的文件(运行配置等)一起带上;备份目录、锁文件、WAL 不带。
            mkdir -p "$work/data-extra"
            (cd "$DIR/data" && find . -mindepth 1 -maxdepth 1 \
                ! -name backup ! -name 'litebox.db' ! -name 'litebox.db-*' ! -name '*.lock' \
                -exec cp -a {} "$work/data-extra/" \;)
            ;;
        postgres)
            dc exec -T postgres pg_dump -U postgres -d litebox -Fc > "$work/litebox.pgdump"
            ;;
        external-postgres)
            # 只备份应用库,不碰共享集群的其他库。
            pg_client 'pg_dump -Fc "$(cat /lbsecrets/db_dsn)"' > "$work/litebox.pgdump"
            ;;
    esac
}

# make_backup [REF]:做一份完整部署备份,带清单,打印归档路径。不验证、不删旧的 —— 调用方决定。
make_backup() {
    local ref="${1:-}" ts work out
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    work="$(workdir)"
    dump_db "$work" "$ref"
    cp "$DIR/.env" "$work/env"
    cp -r "$DIR/secrets" "$work/secrets"
    if [ -f "$DIR/compose.override.yaml" ]; then cp "$DIR/compose.override.yaml" "$work/"; fi
    {
        echo "version=$(envget LITEBOX_VERSION)"
        echo "mode=$(mode)"
        echo "created_at=$ts"
        (cd "$work" && find . -type f ! -name manifest.txt | sort)
    } > "$work/manifest.txt"
    out="$DIR/backups/litebox-$ts.tar.gz"
    mkdir -p "$DIR/backups"; chmod 700 "$DIR/backups"
    # 经 stdout 写:路径里带冒号时 GNU tar 会把它当成远程主机。
    tar -C "$work" -czf - . > "$out"
    chmod 600 "$out"
    rm -rf "$work"
    echo "$out"
}

# restore_into_place FILE:把备份恢复到当前部署里(调用方保证主控已停)。当前数据先留一份在旁边。
restore_into_place() {
    local file="$1" ts work
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    work="$(workdir)"
    tar -C "$work" -xzf - < "$file"
    case "$(mode)" in
        sqlite)
            # 复制一份而不是挪走:数据目录里还有运行配置等文件,挪走之后新目录里只剩一个库。
            if [ -d "$DIR/data" ]; then cp -a "$DIR/data" "$DIR/data.before-restore-$ts"; fi
            mkdir -p "$DIR/data"
            rm -f "$DIR/data/litebox.db" "$DIR/data/litebox.db-wal" "$DIR/data/litebox.db-shm"
            cp "$work/litebox.db" "$DIR/data/litebox.db"
            if [ "$EXACT" = 1 ] && [ -d "$work/data-extra" ]; then
                cp -a "$work/data-extra/." "$DIR/data/"
            fi
            chown -R 10001:10001 "$DIR/data" 2>/dev/null || true
            info "原数据目录留在 data.before-restore-$ts"
            ;;
        postgres)
            dc exec -T postgres sh -c 'dropdb -U postgres --if-exists litebox_before_restore && psql -v ON_ERROR_STOP=1 -U postgres -c "ALTER DATABASE litebox RENAME TO litebox_before_restore" && createdb -U postgres -O litebox litebox'
            dc exec -T postgres pg_restore -U postgres -d litebox --no-owner --role=litebox --exit-on-error < "$work/litebox.pgdump"
            info "原库留在快捷 PG 里的 litebox_before_restore"
            ;;
        external-postgres)
            # 应用库级恢复:先把当前状态导出一份放在 backups/,再删掉应用账号自己在当前 schema 里的视图与表,
            # 最后 pg_restore。共享实例上别的库、别的账号的对象、库本身、账号本身一样都不碰。
            local safety="$DIR/backups/before-restore-$ts.pgdump"
            pg_client 'pg_dump -Fc "$(cat /lbsecrets/db_dsn)"' > "$safety"
            chmod 600 "$safety"
            info "当前应用库先导出了一份:$safety"
            pg_client 'set -e
                dsn="$(cat /lbsecrets/db_dsn)"
                psql "$dsn" -v ON_ERROR_STOP=1 -q <<SQL
DO \$\$ DECLARE r record; BEGIN
  FOR r IN SELECT format('"'"'DROP VIEW IF EXISTS %I.%I CASCADE'"'"', schemaname, viewname) AS q
             FROM pg_views WHERE schemaname = current_schema() AND viewowner = current_user LOOP EXECUTE r.q; END LOOP;
  FOR r IN SELECT format('"'"'DROP TABLE IF EXISTS %I.%I CASCADE'"'"', schemaname, tablename) AS q
             FROM pg_tables WHERE schemaname = current_schema() AND tableowner = current_user LOOP EXECUTE r.q; END LOOP;
END \$\$;
SQL'
            pg_client 'pg_restore --no-owner --no-privileges --exit-on-error -d "$(cat /lbsecrets/db_dsn)"' < "$work/litebox.pgdump"
            ;;
    esac
    # 主密钥永远跟着数据走:库里的密文只有它解得开。连接串与 PG 口令永远保留当前的(见文件头)。
    cp "$work/secrets/master_key" "$DIR/secrets/master_key"
    if [ "$EXACT" = 1 ]; then
        cp "$work/env" "$DIR/.env"
        if [ -f "$work/compose.override.yaml" ]; then cp "$work/compose.override.yaml" "$DIR/"; else rm -f "$DIR/compose.override.yaml"; fi
    fi
    fix_secret_perms
    rm -rf "$work"
}

# hold_deploys REASON:恢复之后、启动之前暂停自动下发(主控以外的一次性容器,用与主控相同的配置与网络)。
# 返回 0 = 已暂停;2 = 这个版本没有这个开关(比 v23.1 更早);1 = 写不进去。
# 后两种**都不自动启动主控**(G03):它会照库里那份旧快照下发,快照之后撤掉的凭据会被推回节点。
hold_deploys() {
    local out=""
    if fault_point hold-write && out="$(dc run --rm --no-deps -T litebox deploy-hold on --reason "$1" --config /data/litebox.yaml 2>&1)"; then
        echo "$out"
        return 0
    fi
    if echo "$out" | grep -q '未知命令 "deploy-hold"'; then
        return 2
    fi
    [ -z "$out" ] || echo "$out" >&2
    return 1
}

# start_after_recovery WHAT:恢复 / 回滚之后启动主控 —— 只有暂停写进去了才启动。
# 返回 0 = 已暂停并启动且 healthy;2 = 版本不支持暂停,主控保持停止;1 = 暂停写不进去,主控保持停止;
# 3 = 暂停写进去了但主控起不来。
start_after_recovery() {
    local what="$1" rc=0
    hold_deploys "$what" || rc=$?
    case "$rc" in
        0) ;;
        2) warn "$what:这个版本没有「暂停自动下发」的开关,主控保持停止(启动之后它会照这份旧快照往节点下发)"
           warn "核对用户与撤权之后,用 litebox.sh start --without-hold 启动"
           return 2 ;;
        *) warn "$what:暂停自动下发没写进去,主控保持停止。排查之后用 litebox.sh start 重试"
           return 1 ;;
    esac
    if ! dc up -d; then
        dc logs --tail=80 litebox || true
        warn "$what:主控启动失败(自动下发处于暂停状态)"
        return 3
    fi
    if ! wait_healthy; then
        warn "$what:主控没有变成 healthy(自动下发处于暂停状态)"
        return 3
    fi
    return 0
}

# ---------------------------------------------------------------- 命令

cmd_install() {
    mkdir -p "$DIR/secrets" "$DIR/data" "$DIR/backups"
    chmod 700 "$DIR/secrets" "$DIR/backups"
    [ -n "$MODE" ] || MODE="$(envget LITEBOX_MODE)"
    [ -n "$MODE" ] || MODE="sqlite"
    case "$MODE" in sqlite|postgres|external-postgres) ;; *) die "--mode 只认 sqlite / postgres / external-postgres" ;; esac
    if [ -n "$(envget LITEBOX_MODE)" ] && [ "$(envget LITEBOX_MODE)" != "$MODE" ]; then
        die "这个目录已经按 $(envget LITEBOX_MODE) 模式装过;不要直接改模式 —— 换存储引擎要停掉主控、在新目录按新模式装好,再用 litebox db import / verify 把数据离线搬过去"
    fi
    [ -n "$VERSION" ] || VERSION="$(envget LITEBOX_VERSION)"
    [ -n "$VERSION" ] || die "第一次安装要给 --version(例如 v1.0.0);不提供 latest,版本必须明确"
    [ -n "$BASE_URL" ] || BASE_URL="$(envget LITEBOX_BASE_URL)"
    [ -n "$BASE_URL" ] || die "第一次安装要给 --base-url(订阅地址的站点根,例如 https://panel.example.com)"

    envset LITEBOX_MODE "$MODE"
    envset LITEBOX_VERSION "$VERSION"
    envset LITEBOX_BASE_URL "$BASE_URL"
    if [ -n "$PORT" ]; then envset LITEBOX_PORT "$PORT"; fi
    [ -n "$(envget LITEBOX_IMAGE)" ] || envset LITEBOX_IMAGE "${LITEBOX_IMAGE:-$IMAGE_DEFAULT}"
    local ref; ref="$(image_ref):$VERSION"

    if [ "${LITEBOX_SKIP_PULL:-}" = "1" ]; then
        info "使用本地镜像 $ref(LITEBOX_SKIP_PULL=1)"
    else
        info "拉取镜像 $ref"
        docker pull "$ref"
    fi
    extract_deploy_files "$ref"

    if [ ! -s "$DIR/secrets/master_key" ]; then
        info "生成主密钥(只在第一次安装时生成;丢了它库里的密文就再也解不开)"
        docker run --rm "$ref" genkey 2>/dev/null > "$DIR/secrets/master_key"
    else
        info "沿用已有的主密钥"
    fi

    case "$MODE" in
        postgres)
            [ -s "$DIR/secrets/pg_admin_password" ] || random_secret > "$DIR/secrets/pg_admin_password"
            [ -s "$DIR/secrets/pg_app_password" ] || random_secret > "$DIR/secrets/pg_app_password"
            # 口令只含字母数字,连接串里不用再做 URL 编码。
            if [ ! -s "$DIR/secrets/db_dsn" ]; then
                printf 'postgres://litebox:%s@postgres:5432/litebox?sslmode=disable\n' "$(cat "$DIR/secrets/pg_app_password")" > "$DIR/secrets/db_dsn"
            fi
            mkdir -p "$DIR/pgdata"
            ;;
        external-postgres)
            if [ -n "$DSN_FILE" ]; then
                install -m 600 "$DSN_FILE" "$DIR/secrets/db_dsn"
            fi
            [ -s "$DIR/secrets/db_dsn" ] || die "已有 PG 模式要 --dsn-file <一行连接串的文件>(库与应用账号由管理员预先建好)"
            if [ -n "$PG_NETWORK" ]; then envset LITEBOX_PG_NETWORK "$PG_NETWORK"; fi
            ;;
    esac
    fix_secret_perms
    # 容器里是 10001:数据目录要让它写得进去。
    chown -R 10001:10001 "$DIR/data" 2>/dev/null || info "提示:无法把 $DIR/data 的属主改成 10001,请用 root 执行或手工 chown"

    info "启动($MODE 模式)"
    if ! dc up -d; then
        dc logs --tail=80 || true
        die "启动失败,日志在上面"
    fi
    if wait_healthy; then
        info "主控已就绪:$(dc exec -T litebox litebox version 2>/dev/null || true)"
        echo "第一次启动时的管理员口令在日志里:litebox.sh logs | grep 初始密码"
    else
        dc logs --tail=80 || true
        die "主控没有在 3 分钟内变成 healthy,日志在上面"
    fi
}

cmd_status() {
    dc ps
    echo
    echo "版本:$(envget LITEBOX_VERSION)  模式:$(mode)  镜像:$(image_ref)"
    docker inspect -f '健康:{{.State.Health.Status}}  启动于:{{.State.StartedAt}}' "$(dc ps -q litebox)" 2>/dev/null || true
}

# cmd_backup 做一份完整部署备份:应用库(SQLite 用 VACUUM INTO 一致性快照;PG 用 pg_dump,只备份应用库)、
# 数据目录里库以外的文件、.env、secrets、compose.override.yaml,外加清单。验证通过之后才删旧的,保留最近 $KEEP_BACKUPS 份。
cmd_backup() {
    if [ "$(mode)" = "external-postgres" ]; then pg_client_check; fi
    info "备份应用数据库"
    local out; out="$(make_backup)"
    if cmd_verify_backup "$out"; then
        info "备份已验证:$out"
        # 新备份验证通过之后才删最旧的。
        ls -1t "$DIR"/backups/litebox-*.tar.gz | tail -n +$((KEEP_BACKUPS + 1)) | xargs -r rm -f
    else
        # 改名而不是删:留着给人看为什么没过,但不算进「最近 3 份」。
        mv "$out" "$out.unverified"
        die "新备份没有通过验证,旧备份一份都没删:$out.unverified"
    fi
}

# cmd_verify_backup 在隔离环境里**真的恢复一遍**再检查(R03):不联网、不跑调度。
#   SQLite:断网容器里打开快照,完整性 + 外键 + 结构版本 + 用备份里的主密钥抽查解密;
#   PG    :--internal 网络里起一个临时 PG,pg_restore 进去,再对它做同样的检查 —— 只列归档目录(pg_restore -l)
#           证明不了这份备份恢复得出来。
cmd_verify_backup() {
    local file="${1:-${POS[0]:-}}"
    [ -f "$file" ] || die "要验证的备份文件不存在:$file"
    local work rc=0 ref
    work="$(workdir)"
    tar -C "$work" -xzf - < "$file"
    [ -s "$work/secrets/master_key" ] || { echo "备份里没有主密钥" >&2; rm -rf "$work"; return 1; }
    ref="$(image_ref):$(sed -n 's/^LITEBOX_VERSION=//p' "$work/env" | tail -1)"
    readable_by_app "$work/secrets/master_key"
    if [ -f "$work/litebox.db" ]; then
        mkdir -p "$work/data" && mv "$work/litebox.db" "$work/data/litebox.db"
        chown -R 10001:10001 "$work/data" 2>/dev/null || chmod -R a+rwX "$work/data"
        docker run --rm --network none -v "$work/data:/data" -v "$work/secrets/master_key:/run/secrets/k:ro" \
            -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k "$ref" check --config /data/litebox.yaml || rc=1
    elif [ -f "$work/litebox.pgdump" ]; then
        if temp_pg_start "$work/litebox.pgdump"; then
            docker run --rm --network "$TMP_NET" -v "$work/secrets/master_key:/run/secrets/k:ro" \
                -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k -e LITEBOX_DB_DRIVER=postgres -e LITEBOX_DB_DSN="$TMP_DSN" \
                "$ref" check --config /tmp/litebox.yaml || rc=1
        else
            echo "备份恢复不进一个空的 PG" >&2; rc=1
        fi
        temp_pg_stop
    else
        echo "备份里没有数据库" >&2; rc=1
    fi
    rm -rf "$work"
    return $rc
}

# backup_version / backup_mode FILE:备份是哪个版本、哪种模式做的。
backup_version() { tar -xzOf "$1" ./env 2>/dev/null | sed -n 's/^LITEBOX_VERSION=//p' | tail -1; }
backup_mode() { tar -xzOf "$1" ./manifest.txt 2>/dev/null | sed -n 's/^mode=//p' | tail -1; }

# newer_than A B:版本 A 比 B 新(按 sort -V)。
newer_than() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

cmd_restore() {
    local file="${POS[0]:-}" bver cur bmode
    [ -f "$file" ] || die "用法:litebox.sh restore <备份文件> [--exact]"
    # 预检全部放在停主控之前(R03):不支持或不配套的组合,在动任何东西之前就拒绝。
    bmode="$(backup_mode "$file")"
    if [ -n "$bmode" ] && [ "$bmode" != "$(mode)" ]; then
        die "备份来自 $bmode 模式,这个目录是 $(mode) 模式;换存储引擎请用 litebox db import"
    fi
    bver="$(backup_version "$file")"; cur="$(envget LITEBOX_VERSION)"
    if [ "$EXACT" != 1 ] && [ -n "$bver" ] && newer_than "$bver" "$cur"; then
        die "备份来自更新的版本 $bver,当前是 $cur:当前版本打不开它。用 --exact 回到 $bver,或先升级到 $bver"
    fi
    if [ "$(mode)" = "external-postgres" ]; then pg_client_check; fi
    cmd_verify_backup "$file" || die "备份没有通过验证,不恢复(主控没有动)"
    # --exact 要用恢复点那个版本的部署文件:在停主控之前取到暂存目录,停了之后只剩复制。
    local stage=""
    if [ "$EXACT" = 1 ]; then
        stage="$DIR/.deploy-restore-$bver"; rm -rf "$stage"
        extract_deploy_files "$(image_ref):$bver" "$stage" || die "取 $bver 的部署文件失败,主控没有动"
    fi

    info "停主控(恢复主控库不会回滚节点上的东西)"
    dc stop litebox
    if [ -n "$stage" ]; then
        if ! copy_deploy_files "$stage" "$DIR"; then
            # 数据还没动:按原样重新启动是安全的(不是旧快照)。
            dc up -d || true
            die "放置 $bver 的部署文件失败,数据没有动,已按原样重新启动"
        fi
        rm -rf "$stage"
    fi
    # 从这里起数据开始被替换:中途失败时 set -e 会停下,主控保持停止(不会带着半份数据启动)。
    trap 'warn "恢复中途失败:主控保持停止。原数据留在旁边(data.before-restore-* / litebox_before_restore / backups/before-restore-*)"' ERR
    restore_into_place "$file"
    trap - ERR
    if [ "$EXACT" = 1 ]; then info "已回到恢复点的版本 $(envget LITEBOX_VERSION) 与配置"; fi
    start_after_recovery "从备份恢复了主控库($(basename "$file"))" || die "数据已恢复;主控没有启动或没有健康(原因见上面)"
    info "已恢复并启动。自动下发已暂停:核对用户、撤权与节点配置版本之后在面板顶部恢复"
}

# cmd_start:恢复 / 回滚之后主控停着时由管理员显式启动。先暂停自动下发;版本太旧没有这个开关时
# 要 --without-hold 才启动 —— 那是管理员核对过节点之后的显式决定,不由脚本替他做。
cmd_start() {
    local rc=0
    start_after_recovery "管理员手动启动(恢复或回滚之后)" || rc=$?
    case "$rc" in
        0) info "已启动。自动下发已暂停:核对之后在面板顶部恢复" ;;
        2) [ "$WITHOUT_HOLD" = 1 ] || die "没有启动:这个版本不支持暂停自动下发,核对节点之后加 --without-hold"
           warn "按 --without-hold 启动:没有暂停自动下发,它会照库里的配置下发"
           dc up -d || die "启动失败"
           wait_healthy || die "没有变成 healthy"
           info "已启动(没有暂停自动下发)" ;;
        *) die "没有启动或没有健康(原因见上面)" ;;
    esac
}

# rehearse_migration REF:在当前数据的副本上用 REF 跑一遍迁移,线上一个字节都不动。
rehearse_migration() {
    local ref="$1" rehearsal rc=0
    rehearsal="$(workdir)"
    dump_db "$rehearsal"
    case "$(mode)" in
        sqlite)
            mkdir -p "$rehearsal/data" && mv "$rehearsal/litebox.db" "$rehearsal/data/litebox.db"
            chown -R 10001:10001 "$rehearsal/data" 2>/dev/null || chmod -R a+rwX "$rehearsal/data"
            docker run --rm --network none -v "$rehearsal/data:/data" -v "$DIR/secrets/master_key:/run/secrets/k:ro" \
                -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k "$ref" migrate --config /data/litebox.yaml || rc=1
            ;;
        *)
            if temp_pg_start "$rehearsal/litebox.pgdump"; then
                docker run --rm --network "$TMP_NET" -v "$DIR/secrets/master_key:/run/secrets/k:ro" \
                    -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k -e LITEBOX_DB_DRIVER=postgres -e LITEBOX_DB_DSN="$TMP_DSN" \
                    "$ref" migrate --config /tmp/litebox.yaml || rc=1
            else
                rc=1
            fi
            temp_pg_stop
            ;;
    esac
    rm -rf "$rehearsal"
    return $rc
}

# cmd_update 是一个状态机(R03),每一个失败点都走同一个回滚:
#   预检(拉镜像、版本可执行、这个模式回得去)→ 数据副本上演练迁移 → 进入维护(停主控)→ 最终恢复点(停写之后备份并验证)
#   → 换版本 → 启动 → 就绪检查;启动命令失败、没变成 healthy,都换回旧版本并恢复最终恢复点(--exact)。
cmd_update() {
    local old new ref oldref final
    old="$(envget LITEBOX_VERSION)"
    new="${VERSION:-}"
    [ -n "$new" ] || die "要给 --version(不提供 latest)"
    if [ "$old" = "$new" ] && wait_healthy; then
        info "已经是 $new 且健康,什么都不做"
        return 0
    fi
    ref="$(image_ref):$new"; oldref="$(image_ref):$old"
    if [ "${LITEBOX_SKIP_PULL:-}" != "1" ]; then
        info "拉取 $ref"
        docker pull "$ref"
    fi
    docker run --rm "$ref" version || die "新版本镜像不能执行,没有动任何东西"
    # 回得去才往前走:已有 PG 模式要确认 PG 客户端与服务端相容(备份与恢复都靠它)。
    if [ "$(mode)" = "external-postgres" ]; then
        pg_client_check || die "已有 PG 模式的回滚条件不满足,没有动任何东西"
    fi

    info "在数据副本上演练迁移"
    rehearse_migration "$ref" || die "新版本在数据副本上迁移失败,没有动线上的任何东西"

    # 能在停服之前做完的准备都放在这里:新版本的部署文件先取到暂存目录,当前这一套另存一份 ——
    # 回滚时直接用它,不再依赖「从旧镜像里再取一次」也能成功。
    STAGE_NEW="$DIR/.deploy-$new"; SAVED_OLD="$DIR/.deploy-$old"
    rm -rf "$STAGE_NEW" "$SAVED_OLD"
    extract_deploy_files "$ref" "$STAGE_NEW" || die "取新版本的部署文件失败,没有动线上的任何东西"
    copy_deploy_files "$DIR" "$SAVED_OLD" || die "另存当前部署文件失败,没有动线上的任何东西"

    info "进入维护:停主控,做最终恢复点"
    dc stop litebox
    if ! final="$(make_backup "$oldref")"; then
        dc up -d || true
        die "最终恢复点没做成,已按原版本重新启动"
    fi
    if ! cmd_verify_backup "$final"; then
        mv "$final" "$final.unverified"
        dc up -d || true
        die "最终恢复点没有通过验证($final.unverified),已按原版本重新启动,没有升级"
    fi
    info "最终恢复点:$final"

    # 从这里起任何一步失败都走同一个回滚。
    if ! { fault_point apply-files && copy_deploy_files "$STAGE_NEW" "$DIR" && envset LITEBOX_VERSION "$new"; }; then
        update_rollback "$old" "$final" "换版本失败(部署文件或 .env 没写进去)"
    fi
    if ! dc up -d; then
        update_rollback "$old" "$final" "启动命令失败"
    fi
    if ! wait_healthy; then
        update_rollback "$old" "$final" "没有在 3 分钟内变成 healthy"
    fi
    rm -rf "$STAGE_NEW" "$SAVED_OLD"
    ls -1t "$DIR"/backups/litebox-*.tar.gz | tail -n +$((KEEP_BACKUPS + 1)) | xargs -r rm -f
    info "已升级到 $new(升级前的恢复点:$final)"
}

update_rollback() {
    local old="$1" final="$2" why="$3" rc=0
    warn "新版本$why,回滚到 $old 并恢复最终恢复点 $final"
    dc logs --tail=80 litebox || true
    dc stop litebox || true
    # 部署文件用升级前另存的那一套(回滚不该依赖再从镜像里取一次也成功)。
    if ! copy_deploy_files "$SAVED_OLD" "$DIR"; then
        warn "另存的部署文件放不回去,改从 $old 的镜像里取"
        extract_deploy_files "$(image_ref):$old" || die "回滚失败:部署文件放不回去。数据在 $final,需要人工处理"
    fi
    envset LITEBOX_VERSION "$old" || die "回滚失败:.env 写不进去。数据在 $final,需要人工处理"
    rm -rf "${STAGE_NEW:-}" "${SAVED_OLD:-}"
    EXACT=1
    restore_into_place "$final"
    start_after_recovery "升级失败回滚到 $old,恢复了升级前的主控库" || rc=$?
    case "$rc" in
        0) die "升级失败($why),已回滚到 $old;自动下发已暂停,核对之后在面板顶部恢复" ;;
        2) die "升级失败($why),已回滚到 $old 的文件与升级前的库;主控保持停止(这个版本不能暂停自动下发),核对之后 litebox.sh start --without-hold" ;;
        1) die "升级失败($why),已回滚到 $old 的文件与升级前的库;暂停自动下发写不进去,主控保持停止,排查之后 litebox.sh start" ;;
        *) die "回滚之后旧版本 $old 也没有起来。数据在 $final,原数据留在旁边(见上面的提示),需要人工处理" ;;
    esac
}

case "$cmd" in
    install) cmd_install ;;
    status) cmd_status ;;
    backup) cmd_backup ;;
    verify-backup) cmd_verify_backup "${POS[0]:-}" && info "备份可用" ;;
    restore) cmd_restore ;;
    update) cmd_update ;;
    start) cmd_start ;;
    logs) dc logs -f --tail=200 litebox ;;
    *) sed -n '2,25p' "$0"; exit 1 ;;
esac
