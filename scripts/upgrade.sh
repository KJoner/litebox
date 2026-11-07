#!/usr/bin/env bash
# LiteBox 主控升级脚本(原生 systemd 安装)。
#
# 顺序刻意如此:预检 → 在线预备备份 → 停服 → 最终恢复点(停写之后备份并验证)→ 换二进制 → 迁移 → 启动 → 健康检查。
# 迁移是不可逆的:新版本的迁移哪怕只跑了一半,旧二进制也会因为「库的版本比程序新」拒绝启动。
# 所以迁移失败、启动失败、健康检查失败**都要把库一起恢复到最终恢复点**,不能只换回旧二进制(R03)。
#
# 支持范围:只支持 SQLite。配置成 PostgreSQL 的部署在动任何东西之前就拒绝 —— 这个脚本没有
# 恢复 PG 应用库的办法;PG 部署请用 deploy/docker/litebox.sh,或先自己 pg_dump 应用库再手工换二进制。
set -euo pipefail

LITEBOX_USER="${LITEBOX_USER:-litebox}"
INSTALL_DIR="${INSTALL_DIR:-/opt/litebox}"
CONFIG_DIR="${CONFIG_DIR:-/etc/litebox}"
CONFIG_FILE="$CONFIG_DIR/litebox.yaml"
ENV_FILE="$CONFIG_DIR/litebox.env"
BINARY="${BINARY:-}"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请以 root 执行"
[ -f "$CONFIG_FILE" ] || die "找不到 $CONFIG_FILE,请先执行 install.sh"

if [ -z "$BINARY" ]; then
    ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
    for candidate in \
        "$(dirname "$0")/../bin/litebox-linux-$ARCH" \
        "$(dirname "$0")/../bin/litebox" \
        "./litebox"; do
        if [ -x "$candidate" ]; then BINARY="$candidate"; break; fi
    done
fi
[ -n "$BINARY" ] && [ -x "$BINARY" ] || die "找不到新版 litebox 二进制,请用 BINARY=... 指定"

OLD_VERSION="$("$INSTALL_DIR/litebox" version 2>/dev/null || echo '未知')"
NEW_VERSION="$("$BINARY" version 2>/dev/null || echo '未知')"
log "当前版本:$OLD_VERSION"
log "目标版本:$NEW_VERSION"

runas() {
    sudo -u "$LITEBOX_USER" env \
        "LITEBOX_MASTER_KEY=$(grep '^LITEBOX_MASTER_KEY=' "$ENV_FILE" | cut -d= -f2-)" "$@"
}

# ---- 0. 预检:不支持的组合在备份、停服、替换之前就拒绝 ----
if grep -qE '^[[:space:]]*driver:[[:space:]]*"?postgres' "$CONFIG_FILE" || grep -qE '^LITEBOX_DB_DRIVER=postgres' "$ENV_FILE" 2>/dev/null; then
    die "这台机器的主控配置的是 PostgreSQL:原生升级脚本只能自动回滚 SQLite。
请先用 pg_dump 备份 LiteBox 应用库,再手工替换二进制;或改用 Docker 部署(deploy/docker/litebox.sh)。
这台机器上什么都没有动。"
fi
"$BINARY" version >/dev/null 2>&1 || die "新版二进制不能执行,什么都没有动"

DB_PATH="$(grep -oP '(?<=^  path: ").*(?=")' "$CONFIG_FILE" | head -1)"
DB_PATH="${DB_PATH:-/var/lib/litebox/litebox.db}"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
BACKUP_DIR="$(dirname "$DB_PATH")/backup"
install -d -m 0750 -o "$LITEBOX_USER" -g "$LITEBOX_USER" "$BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/pre-upgrade-$STAMP.db"
FINAL_FILE="$BACKUP_DIR/final-$STAMP.db"
OLD_BIN="$BACKUP_DIR/litebox-$STAMP.bin"

# check_copy FILE:在一份临时副本上跑只读自检(check 不迁移、不改被检查的那份)。
check_copy() {
    local tmp; tmp="$(mktemp -d)"
    cp "$1" "$tmp/check.db"
    sed "s#^  path: .*#  path: \"$tmp/check.db\"#" "$CONFIG_FILE" > "$tmp/cfg.yaml"
    chown -R "$LITEBOX_USER:$LITEBOX_USER" "$tmp"
    if runas "$2" check --config "$tmp/cfg.yaml" >/dev/null; then rm -rf "$tmp"; return 0; fi
    rm -rf "$tmp"; return 1
}

# ---- 1. 在线预备备份(额外的一份,主控还在跑)----
log "升级前在线备份到 $BACKUP_FILE"
runas "$INSTALL_DIR/litebox" backup --config "$CONFIG_FILE" --output "$BACKUP_FILE" >/dev/null
[ -f "$BACKUP_FILE" ] || die "备份未生成,已中止升级(什么都没有动)"

# 旧二进制也留一份,回退时不必再去找发布包。
cp -a "$INSTALL_DIR/litebox" "$OLD_BIN"
log "旧二进制已保存到 $OLD_BIN"

# ---- 2. 进入维护:停服,做最终恢复点 ----
log "停止服务"
systemctl stop litebox
log "最终恢复点(停写之后的一致性快照):$FINAL_FILE"
if ! runas "$OLD_BIN" backup --config "$CONFIG_FILE" --output "$FINAL_FILE" >/dev/null || ! check_copy "$FINAL_FILE" "$OLD_BIN"; then
    systemctl start litebox || true
    die "最终恢复点没做成或没有通过自检,已按原版本重新启动,没有升级"
fi

# rollback WHY:换回旧二进制、把库恢复到最终恢复点、暂停自动下发、启动并确认。
rollback() {
    log "$1,回滚:旧二进制 + 最终恢复点的库"
    systemctl stop litebox || true
    install -m 0755 "$OLD_BIN" "$INSTALL_DIR/litebox.new"
    mv "$INSTALL_DIR/litebox.new" "$INSTALL_DIR/litebox"
    rm -f "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"
    cp "$FINAL_FILE" "$DB_PATH"
    chown "$LITEBOX_USER:$LITEBOX_USER" "$DB_PATH"
    chmod 0600 "$DB_PATH"
    # 恢复的是升级前的库:暂停自动下发,核对之后在面板上解除(旧版本没有这个命令时只能提醒)。
    if ! runas "$INSTALL_DIR/litebox" deploy-hold on --reason "升级失败回滚" --config "$CONFIG_FILE" >/dev/null 2>&1; then
        log "旧版本不支持暂停自动下发:启动之后先核对用户与撤权"
    fi
    systemctl start litebox
    sleep 3
    if systemctl is-active --quiet litebox; then
        die "$1;已回滚到 $OLD_VERSION,库回到了升级前的状态($FINAL_FILE)。日志:journalctl -u litebox -n 50"
    fi
    die "$1;回滚之后旧版本也没有起来,需要人工处理。最终恢复点:$FINAL_FILE,旧二进制:$OLD_BIN"
}

# ---- 3. 替换二进制 ----
log "替换二进制"
# 先落到同目录再 rename:直接覆盖正在使用的文件会得到 text file busy,
# 而 rename 只是换 inode。
install -m 0755 "$BINARY" "$INSTALL_DIR/litebox.new"
mv "$INSTALL_DIR/litebox.new" "$INSTALL_DIR/litebox"

# ---- 4. 迁移 ----
log "执行数据库迁移"
runas "$INSTALL_DIR/litebox" migrate --config "$CONFIG_FILE" || rollback "迁移失败"

# ---- 5. 启动并验证 ----
log "启动服务"
systemctl start litebox || rollback "新版本启动命令失败"
sleep 3
systemctl is-active --quiet litebox || rollback "新版本启动失败"

# 健康检查要看接口是否真的可用,而不是只看 systemd active。
LISTEN="$(grep -oP '(?<=^  listen: ").*(?=")' "$CONFIG_FILE" | head -1)"
LISTEN="${LISTEN:-127.0.0.1:8080}"
healthy=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS --max-time 10 "http://$LISTEN/api/health" >/dev/null 2>&1; then healthy=1; break; fi
    sleep 3
done
[ "$healthy" = 1 ] || rollback "新版本健康检查未通过"

log "数据库自检"
runas "$INSTALL_DIR/litebox" check --config "$CONFIG_FILE"

echo
log "升级完成:$OLD_VERSION → $NEW_VERSION"
echo "在线预备备份:$BACKUP_FILE"
echo "最终恢复点  :$FINAL_FILE"
echo "旧二进制    :$OLD_BIN"
