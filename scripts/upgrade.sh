#!/usr/bin/env bash
# LiteBox 主控升级脚本。
#
# 顺序刻意如此:先备份 → 再换二进制 → 再迁移 → 再启动 → 自检。
# 备份放在最前面,因为迁移是不可逆的 —— 一旦新版本的迁移执行完,
# 想回退到旧二进制就只能靠这份备份。
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

# ---- 1. 备份 ----
STAMP="$(date -u +%Y%m%d-%H%M%S)"
BACKUP_PATH="$(grep -oP '(?<=^  path: ").*(?=")' "$CONFIG_FILE" | head -1)"
BACKUP_DIR="$(dirname "${BACKUP_PATH:-/var/lib/litebox/litebox.db}")/backup"
BACKUP_FILE="$BACKUP_DIR/pre-upgrade-$STAMP.db"

log "升级前备份到 $BACKUP_FILE"
runas "$INSTALL_DIR/litebox" backup --config "$CONFIG_FILE" --output "$BACKUP_FILE" >/dev/null
[ -f "$BACKUP_FILE" ] || die "备份未生成,已中止升级"

# 旧二进制也留一份,回退时不必再去找发布包。
cp -a "$INSTALL_DIR/litebox" "$BACKUP_DIR/litebox-$STAMP.bin"
log "旧二进制已保存到 $BACKUP_DIR/litebox-$STAMP.bin"

# ---- 2. 停服并替换 ----
log "停止服务"
systemctl stop litebox

log "替换二进制"
# 先落到同目录再 rename:直接覆盖正在使用的文件会得到 text file busy,
# 而 rename 只是换 inode。
install -m 0755 "$BINARY" "$INSTALL_DIR/litebox.new"
mv "$INSTALL_DIR/litebox.new" "$INSTALL_DIR/litebox"

# ---- 3. 迁移 ----
log "执行数据库迁移"
if ! runas "$INSTALL_DIR/litebox" migrate --config "$CONFIG_FILE"; then
    log "迁移失败,回退到旧二进制"
    cp -a "$BACKUP_DIR/litebox-$STAMP.bin" "$INSTALL_DIR/litebox"
    systemctl start litebox
    die "迁移失败,已回退二进制并重启。数据库可能已被部分修改,
若服务异常请用以下命令恢复:
  scripts/restore.sh $BACKUP_FILE"
fi

# ---- 4. 启动并验证 ----
log "启动服务"
systemctl start litebox
sleep 3

if ! systemctl is-active --quiet litebox; then
    log "启动失败,回退到旧二进制"
    cp -a "$BACKUP_DIR/litebox-$STAMP.bin" "$INSTALL_DIR/litebox"
    systemctl restart litebox
    die "新版本启动失败,已回退。日志:journalctl -u litebox -n 50"
fi

# 健康检查要看接口是否真的可用,而不是只看 systemd active。
LISTEN="$(grep -oP '(?<=^  listen: ").*(?=")' "$CONFIG_FILE" | head -1)"
LISTEN="${LISTEN:-127.0.0.1:8080}"
if ! curl -fsS --max-time 10 "http://$LISTEN/api/health" >/dev/null 2>&1; then
    log "健康检查失败,回退到旧二进制"
    systemctl stop litebox
    cp -a "$BACKUP_DIR/litebox-$STAMP.bin" "$INSTALL_DIR/litebox"
    systemctl start litebox
    die "新版本健康检查未通过,已回退"
fi

log "数据库自检"
runas "$INSTALL_DIR/litebox" check --config "$CONFIG_FILE"

echo
log "升级完成:$OLD_VERSION → $NEW_VERSION"
echo "升级前备份:$BACKUP_FILE"
echo "旧二进制  :$BACKUP_DIR/litebox-$STAMP.bin"
