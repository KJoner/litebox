#!/usr/bin/env bash
# 从备份恢复 LiteBox 数据库。
#
# 用法:./restore.sh /var/lib/litebox/backup/pre-upgrade-20260802-120000.db
#
# 恢复只换数据库文件,不动主密钥 —— 两者必须配套:
# 用 A 密钥加密的数据库配 B 密钥,所有 UUID 与节点私钥都解不开。
set -euo pipefail

LITEBOX_USER="${LITEBOX_USER:-litebox}"
INSTALL_DIR="${INSTALL_DIR:-/opt/litebox}"
CONFIG_DIR="${CONFIG_DIR:-/etc/litebox}"
CONFIG_FILE="$CONFIG_DIR/litebox.yaml"
ENV_FILE="$CONFIG_DIR/litebox.env"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

SOURCE="${1:-}"
[ -n "$SOURCE" ] || die "用法:$0 <备份文件>"
[ -f "$SOURCE" ] || die "备份文件不存在:$SOURCE"
[ "$(id -u)" -eq 0 ] || die "请以 root 执行"
[ -f "$CONFIG_FILE" ] || die "找不到 $CONFIG_FILE"

DB_PATH="$(grep -oP '(?<=^  path: ").*(?=")' "$CONFIG_FILE" | head -1)"
DB_PATH="${DB_PATH:-/var/lib/litebox/litebox.db}"

runas() {
    sudo -u "$LITEBOX_USER" env \
        "LITEBOX_MASTER_KEY=$(grep '^LITEBOX_MASTER_KEY=' "$ENV_FILE" | cut -d= -f2-)" "$@"
}

# 先验证备份本身是好的,再动现有数据库 —— 顺序反了就会
# 用一份坏备份换掉一个还能用的数据库。
log "校验备份文件"
TMP_CHECK="$(mktemp -d)"
cp "$SOURCE" "$TMP_CHECK/check.db"
chown "$LITEBOX_USER:$LITEBOX_USER" "$TMP_CHECK/check.db"
chmod 0600 "$TMP_CHECK/check.db"
TMP_CFG="$TMP_CHECK/cfg.yaml"
sed "s#^  path: .*#  path: \"$TMP_CHECK/check.db\"#" "$CONFIG_FILE" > "$TMP_CFG"
chown "$LITEBOX_USER:$LITEBOX_USER" "$TMP_CFG" "$TMP_CHECK"

if ! runas "$INSTALL_DIR/litebox" check --config "$TMP_CFG"; then
    rm -rf "$TMP_CHECK"
    die "备份文件未通过自检,已中止恢复(现有数据库未被改动)"
fi
rm -rf "$TMP_CHECK"

echo
warn "即将用备份覆盖当前数据库:$DB_PATH"
warn "当前的用户、节点与流量记录都会被替换成备份中的内容。"
read -r -p "确认继续?输入 yes 回车:" CONFIRM
[ "$CONFIRM" = "yes" ] || die "已取消"

log "停止服务"
systemctl stop litebox || true

STAMP="$(date -u +%Y%m%d-%H%M%S)"
SAFETY="$(dirname "$DB_PATH")/backup/before-restore-$STAMP.db"
install -d -m 0750 -o "$LITEBOX_USER" -g "$LITEBOX_USER" "$(dirname "$SAFETY")"
if [ -f "$DB_PATH" ]; then
    log "把当前数据库另存为 $SAFETY"
    # 服务已停,此时直接复制是安全的;但 WAL 与 shm 也要一起带走。
    cp -a "$DB_PATH" "$SAFETY"
    [ -f "$DB_PATH-wal" ] && cp -a "$DB_PATH-wal" "$SAFETY-wal" || true
    [ -f "$DB_PATH-shm" ] && cp -a "$DB_PATH-shm" "$SAFETY-shm" || true
fi

log "写入备份内容"
rm -f "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"
cp "$SOURCE" "$DB_PATH"
chown "$LITEBOX_USER:$LITEBOX_USER" "$DB_PATH"
chmod 0600 "$DB_PATH"

log "执行迁移(备份可能来自更早的版本)"
runas "$INSTALL_DIR/litebox" migrate --config "$CONFIG_FILE"

log "启动服务"
systemctl start litebox
sleep 3
systemctl is-active --quiet litebox || die "启动失败:journalctl -u litebox -n 50"

log "恢复后自检"
runas "$INSTALL_DIR/litebox" check --config "$CONFIG_FILE"

echo
log "恢复完成"
echo "恢复前的数据库已另存为:$SAFETY"
echo
warn "若这份备份来自另一台机器或另一次安装,请确认主密钥也是配套的那一把;"
warn "密钥不匹配时服务能启动,但用户 UUID 与节点私钥会解密失败。"
