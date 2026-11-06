#!/usr/bin/env bash
# LiteBox 主控的 Docker 一键部署与运维(VNext DEPLOY03 / DEPLOY06–08)。
#
#   litebox.sh install  [--dir DIR] [--mode sqlite|postgres|external-postgres] [--version vX.Y.Z]
#                       [--base-url https://panel.example.com] [--port 8080] [--dsn-file FILE] [--pg-network NAME]
#   litebox.sh status   [--dir DIR]
#   litebox.sh backup   [--dir DIR]                 # 应用数据 + 配置 + 密钥,保留最近 3 份已验证的
#   litebox.sh verify-backup FILE [--dir DIR]       # 在隔离容器里打开备份:库能读、密文能解
#   litebox.sh restore  FILE [--dir DIR]            # 停主控 → 当前数据挪到一边 → 恢复 → 启动
#   litebox.sh update   [--version vX.Y.Z] [--dir DIR]
#   litebox.sh logs     [--dir DIR]
#
# 规矩:
#   - 重复执行沿用已有的 .env、secrets/ 与 compose.override.yaml,**绝不重新生成主密钥、不覆盖数据**;
#   - 版本相同且健康时不重启(docker compose up -d 对没变的服务什么都不做);
#   - 已有 PG 模式不创建、不修改、不删除共享实例上的任何东西;
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
KEEP_BACKUPS=3

die() { echo "错误:$*" >&2; exit 1; }
info() { echo "==> $*"; }

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
        -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
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

compose_files() {
    local m; m="$(envget LITEBOX_MODE)"
    local files=(-f compose.yaml)
    case "$m" in
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

# extract_deploy_files 从指定版本的镜像里取出同版本的 Compose 文件与初始化脚本。
extract_deploy_files() {
    local ref="$1"
    local cid
    cid="$(docker create "$ref")"
    for f in compose.yaml compose.postgres.yaml compose.external-postgres.yaml compose.pg-network.yaml; do
        docker cp "$cid:/opt/litebox/deploy/$f" "$DIR/$f" >/dev/null
    done
    mkdir -p "$DIR/pg-init"
    docker cp "$cid:/opt/litebox/deploy/pg-init/10-litebox.sh" "$DIR/pg-init/10-litebox.sh" >/dev/null
    docker rm "$cid" >/dev/null
}

random_secret() { head -c 24 /dev/urandom | base64 | tr -d '/+=\n' | head -c 32; }

wait_healthy() {
    local i state
    for i in $(seq 1 60); do
        state="$(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q litebox)" 2>/dev/null || true)"
        [ "$state" = "healthy" ] && return 0
        sleep 3
    done
    return 1
}

cmd_install() {
    mkdir -p "$DIR/secrets" "$DIR/data" "$DIR/backups"
    chmod 700 "$DIR/secrets" "$DIR/backups"
    [ -n "$MODE" ] || MODE="$(envget LITEBOX_MODE)"
    [ -n "$MODE" ] || MODE="sqlite"
    case "$MODE" in sqlite|postgres|external-postgres) ;; *) die "--mode 只认 sqlite / postgres / external-postgres" ;; esac
    if [ -n "$(envget LITEBOX_MODE)" ] && [ "$(envget LITEBOX_MODE)" != "$MODE" ]; then
        die "这个目录已经按 $(envget LITEBOX_MODE) 模式装过;换存储引擎请看 docs/vnext/migration.md,不要直接改模式"
    fi
    [ -n "$VERSION" ] || VERSION="$(envget LITEBOX_VERSION)"
    [ -n "$VERSION" ] || die "第一次安装要给 --version(例如 v1.0.0);不提供 latest,版本必须明确"
    [ -n "$BASE_URL" ] || BASE_URL="$(envget LITEBOX_BASE_URL)"
    [ -n "$BASE_URL" ] || die "第一次安装要给 --base-url(订阅地址的站点根,例如 https://panel.example.com)"

    envset LITEBOX_MODE "$MODE"
    envset LITEBOX_VERSION "$VERSION"
    envset LITEBOX_BASE_URL "$BASE_URL"
    [ -n "$PORT" ] && envset LITEBOX_PORT "$PORT"
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
        chmod 600 "$DIR/secrets/master_key"
    else
        info "沿用已有的主密钥"
    fi

    case "$MODE" in
        postgres)
            [ -s "$DIR/secrets/pg_admin_password" ] || random_secret > "$DIR/secrets/pg_admin_password"
            [ -s "$DIR/secrets/pg_app_password" ] || random_secret > "$DIR/secrets/pg_app_password"
            # 口令只含字母数字,连接串里不用再做 URL 编码。
            printf 'postgres://litebox:%s@postgres:5432/litebox?sslmode=disable\n' "$(cat "$DIR/secrets/pg_app_password")" > "$DIR/secrets/db_dsn"
            mkdir -p "$DIR/pgdata"
            ;;
        external-postgres)
            if [ -n "$DSN_FILE" ]; then
                install -m 600 "$DSN_FILE" "$DIR/secrets/db_dsn"
            fi
            [ -s "$DIR/secrets/db_dsn" ] || die "已有 PG 模式要 --dsn-file <一行连接串的文件>(库与应用账号由管理员预先建好)"
            [ -n "$PG_NETWORK" ] && envset LITEBOX_PG_NETWORK "$PG_NETWORK"
            ;;
    esac
    chmod 600 "$DIR"/secrets/* 2>/dev/null || true
    # 容器里是 10001;secrets 由 Compose 以只读方式挂进去,数据目录要让 10001 写得进去。
    chown -R 10001:10001 "$DIR/data" 2>/dev/null || info "提示:无法把 $DIR/data 的属主改成 10001,请用 root 执行或手工 chown"
    chown 10001:10001 "$DIR"/secrets/* 2>/dev/null || true

    info "启动($MODE 模式)"
    dc up -d
    if wait_healthy; then
        info "主控已就绪:$(dc exec -T litebox litebox version 2>/dev/null || true)"
        echo "第一次启动时的管理员口令在日志里:litebox.sh logs | grep 初始密码"
    else
        dc logs --tail=80 litebox || true
        die "主控没有在 3 分钟内变成 healthy,日志在上面"
    fi
}

cmd_status() {
    dc ps
    echo
    echo "版本:$(envget LITEBOX_VERSION)  模式:$(envget LITEBOX_MODE)  镜像:$(image_ref)"
    docker inspect -f '健康:{{.State.Health.Status}}  启动于:{{.State.StartedAt}}' "$(dc ps -q litebox)" 2>/dev/null || true
}

# cmd_backup 做一份完整部署备份:应用库(SQLite 用 VACUUM INTO 一致性快照;PG 用同主版本的 pg_dump,只备份应用库)、
# .env、secrets、compose.override.yaml。验证通过之后才删旧的,保留最近 $KEEP_BACKUPS 份。
cmd_backup() {
    local ts out work
    ts="$(date -u +%Y%m%dT%H%M%SZ)"
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' RETURN
    info "备份应用数据库"
    case "$(envget LITEBOX_MODE)" in
        sqlite|"")
            dc exec -T litebox litebox backup --config /data/litebox.yaml --output "/data/backup/snap-$ts.db" >/dev/null
            mv "$DIR/data/backup/snap-$ts.db" "$work/litebox.db"
            ;;
        postgres)
            dc exec -T postgres pg_dump -U postgres -d litebox -Fc > "$work/litebox.pgdump"
            ;;
        external-postgres)
            local net=""
            [ -n "$(envget LITEBOX_PG_NETWORK)" ] && net="--network $(envget LITEBOX_PG_NETWORK)"
            # 用与服务端同一个主版本的 pg_dump(LITEBOX_PG_CLIENT_IMAGE 可改)。只备份应用库,不碰共享集群的其他库。
            docker run --rm $net -v "$DIR/secrets/db_dsn:/dsn:ro" "${LITEBOX_PG_CLIENT_IMAGE:-postgres:17-alpine}" \
                sh -c 'pg_dump -Fc "$(cat /dsn)"' > "$work/litebox.pgdump"
            ;;
    esac
    cp "$DIR/.env" "$work/env"
    cp -r "$DIR/secrets" "$work/secrets"
    [ -f "$DIR/compose.override.yaml" ] && cp "$DIR/compose.override.yaml" "$work/"
    out="$DIR/backups/litebox-$ts.tar.gz"
    # 经 stdout 写:路径里带冒号时 GNU tar 会把它当成远程主机。
    tar -C "$work" -czf - . > "$out"
    chmod 600 "$out"
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

# cmd_verify_backup 在隔离容器里打开备份:不联网(--network none)、不跑调度,只做完整性检查与密文抽查。
cmd_verify_backup() {
    local file="${1:-${POS[0]:-}}"
    [ -f "$file" ] || die "要验证的备份文件不存在:$file"
    local work; work="$(mktemp -d)"
    tar -C "$work" -xzf - < "$file"
    [ -s "$work/secrets/master_key" ] || { echo "备份里没有主密钥" >&2; rm -rf "$work"; return 1; }
    local ref; ref="$(image_ref):$(sed -n 's/^LITEBOX_VERSION=//p' "$work/env" | tail -1)"
    if [ -f "$work/litebox.db" ]; then
        mkdir -p "$work/data" && mv "$work/litebox.db" "$work/data/litebox.db"
        chown -R 10001:10001 "$work/data" 2>/dev/null || chmod -R a+rwX "$work/data"
        if ! docker run --rm --network none -v "$work/data:/data" -v "$work/secrets/master_key:/run/secrets/k:ro" \
              -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k "$ref" check --config /data/litebox.yaml; then
            rm -rf "$work"; return 1
        fi
    elif [ -f "$work/litebox.pgdump" ]; then
        # 逻辑备份:确认它是一份完整、可读的 pg_dump 归档(列出目录不报错)。
        if ! docker run --rm --network none -v "$work/litebox.pgdump:/b:ro" "${LITEBOX_PG_CLIENT_IMAGE:-postgres:17-alpine}" \
              pg_restore -l /b >/dev/null; then
            rm -rf "$work"; return 1
        fi
    else
        echo "备份里没有数据库" >&2; rm -rf "$work"; return 1
    fi
    rm -rf "$work"
    return 0
}

cmd_restore() {
    local file="${POS[0]:-}"
    [ -f "$file" ] || die "用法:litebox.sh restore <备份文件>"
    cmd_verify_backup "$file" || die "备份没有通过验证,不恢复"
    local ts work; ts="$(date -u +%Y%m%dT%H%M%SZ)"; work="$(mktemp -d)"
    tar -C "$work" -xzf - < "$file"
    info "停主控(恢复主控库不会回滚节点上的东西 —— 恢复之后先核对配置版本与撤权,再恢复调度)"
    dc stop litebox
    case "$(envget LITEBOX_MODE)" in
        sqlite|"")
            mv "$DIR/data" "$DIR/data.before-restore-$ts"
            mkdir -p "$DIR/data" && cp "$work/litebox.db" "$DIR/data/litebox.db"
            chown -R 10001:10001 "$DIR/data" 2>/dev/null || true
            ;;
        postgres)
            dc exec -T postgres sh -c 'dropdb -U postgres --if-exists litebox_before_restore; psql -U postgres -c "ALTER DATABASE litebox RENAME TO litebox_before_restore" && createdb -U postgres -O litebox litebox'
            dc exec -T postgres pg_restore -U postgres -d litebox --no-owner --role=litebox < "$work/litebox.pgdump"
            ;;
        external-postgres)
            die "已有 PG 模式不由本脚本恢复(不碰共享实例):请管理员用 pg_restore 把 $work/litebox.pgdump 恢复进一个空的应用库"
            ;;
    esac
    cp "$work/secrets/master_key" "$DIR/secrets/master_key"
    rm -rf "$work"
    dc up -d
    wait_healthy && info "已恢复并启动。原数据留在一边(SQLite:data.before-restore-$ts;快捷 PG:库 litebox_before_restore)" \
        || die "恢复之后主控没有变成 healthy"
}

# cmd_update:拉新镜像 → 在数据副本上演练迁移 → 备份(维护边界上的最终恢复点)→ 换版本启动 → 就绪检查;
# 失败时换回旧版本并恢复刚做的备份。迁移不重启任何节点核心。
cmd_update() {
    local old new ref
    old="$(envget LITEBOX_VERSION)"
    new="${VERSION:-}"
    [ -n "$new" ] || die "要给 --version(不提供 latest)"
    if [ "$old" = "$new" ] && wait_healthy; then
        info "已经是 $new 且健康,什么都不做"
        return 0
    fi
    ref="$(image_ref):$new"
    if [ "${LITEBOX_SKIP_PULL:-}" != "1" ]; then
        info "拉取 $ref"
        docker pull "$ref"
    fi
    docker run --rm "$ref" version
    if [ "$(envget LITEBOX_MODE)" = "sqlite" ]; then
        info "在数据副本上演练迁移"
        local rehearsal; rehearsal="$(mktemp -d)"
        dc exec -T litebox litebox backup --config /data/litebox.yaml --output /data/backup/rehearsal.db >/dev/null
        mkdir -p "$rehearsal/data" && mv "$DIR/data/backup/rehearsal.db" "$rehearsal/data/litebox.db"
        chown -R 10001:10001 "$rehearsal/data" 2>/dev/null || chmod -R a+rwX "$rehearsal/data"
        docker run --rm --network none -v "$rehearsal/data:/data" -v "$DIR/secrets/master_key:/run/secrets/k:ro" \
            -e LITEBOX_MASTER_KEY_FILE=/run/secrets/k "$ref" migrate --config /data/litebox.yaml \
            || { rm -rf "$rehearsal"; die "新版本在数据副本上迁移失败,没有动线上的任何东西"; }
        rm -rf "$rehearsal"
    fi
    info "升级前备份"
    cmd_backup
    local last; last="$(ls -1t "$DIR"/backups/litebox-*.tar.gz | head -1)"
    extract_deploy_files "$ref"
    envset LITEBOX_VERSION "$new"
    dc up -d
    if wait_healthy; then
        info "已升级到 $new"
        return 0
    fi
    echo "新版本没有变成 healthy,回滚到 $old 并恢复升级前的备份 $last" >&2
    dc logs --tail=80 litebox || true
    envset LITEBOX_VERSION "$old"
    extract_deploy_files "$(image_ref):$old"
    POS=("$last"); cmd_restore
    die "升级失败,已回滚到 $old"
}

case "$cmd" in
    install) cmd_install ;;
    status) cmd_status ;;
    backup) cmd_backup ;;
    verify-backup) cmd_verify_backup "${POS[0]:-}" && info "备份可用" ;;
    restore) cmd_restore ;;
    update) cmd_update ;;
    logs) dc logs -f --tail=200 litebox ;;
    *) sed -n '2,20p' "$0"; exit 1 ;;
esac
