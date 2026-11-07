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

# supports_hold BIN:这个二进制有没有 deploy-hold 子命令(不碰数据库:不带参数时它只打印用法)。
# 先把输出收下来再看:管道 + pipefail 时 grep 的结果会被二进制自己的非零退出码盖掉。
supports_hold() {
    local out
    out="$("$1" deploy-hold 2>&1 || true)"
    case "$out" in *未知命令*) return 1 ;; esac
    return 0
}

# hold_or_stay_stopped BIN WHY:暂停自动下发写进去了返回 0;写不进去或版本不支持时返回非 0,
# 调用方据此**不启动**主控(G03):旧快照一启动就会把快照之后撤掉的凭据推回节点。
hold_or_stay_stopped() {
    if ! supports_hold "$1"; then
        warn "$2:这个版本没有「暂停自动下发」的开关,主控保持停止。核对用户与撤权之后手动 systemctl start litebox"
        return 2
    fi
    # LITEBOX_FAULT=hold-write 只给故障注入回归用(scripts/test/deploy/)。
    if [ "${LITEBOX_FAULT:-}" = hold-write ] || ! runas "$1" deploy-hold on --reason "$2" --config "$CONFIG_FILE"; then
        warn "$2:暂停自动下发没写进去,主控保持停止。排查之后先 $1 deploy-hold on --config $CONFIG_FILE,再 systemctl start litebox"
        return 1
    fi
    return 0
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
warn "恢复之后先暂停自动下发再启动(旧快照之后撤掉的凭据不会被推回节点),核对之后在面板顶部恢复。"
if ! supports_hold "$INSTALL_DIR/litebox"; then
    warn "当前装的版本没有「暂停自动下发」的开关:恢复之后主控保持停止,核对节点之后手动 systemctl start litebox。"
fi
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

# put_back WHY:恢复中途失败时把恢复前的库放回去再按原样启动 —— 那是恢复之前的现状,不是旧快照,启动是安全的。
put_back() {
    warn "$1:把恢复前的库放回去"
    rm -f "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"
    if [ -f "$SAFETY" ]; then
        cp -a "$SAFETY" "$DB_PATH" || die "$1;恢复前的库也放不回去,主控保持停止。它在 $SAFETY"
        [ -f "$SAFETY-wal" ] && cp -a "$SAFETY-wal" "$DB_PATH-wal" || true
        [ -f "$SAFETY-shm" ] && cp -a "$SAFETY-shm" "$DB_PATH-shm" || true
        systemctl start litebox || true
        die "$1;已放回恢复前的库并按原样启动($SAFETY)"
    fi
    die "$1;原来没有库,主控保持停止"
}

log "执行迁移(备份可能来自更早的版本)"
runas "$INSTALL_DIR/litebox" migrate --config "$CONFIG_FILE" || put_back "迁移失败"

log "暂停自动下发"
hold_rc=0
hold_or_stay_stopped "$INSTALL_DIR/litebox" "从备份恢复了主控库($(basename "$SOURCE"))" || hold_rc=$?
if [ "$hold_rc" != 0 ]; then
    echo "恢复前的数据库已另存为:$SAFETY"
    die "数据已恢复;主控保持停止(原因见上面)"
fi

log "启动服务"
systemctl start litebox || true
sleep 3
systemctl is-active --quiet litebox || die "启动失败(数据已恢复、自动下发处于暂停):journalctl -u litebox -n 50"

log "恢复后自检"
runas "$INSTALL_DIR/litebox" check --config "$CONFIG_FILE" || warn "恢复后自检没过:核对之后再在面板上恢复自动下发"

echo
log "恢复完成。自动下发已暂停:核对用户、撤权与节点配置之后在面板顶部恢复"
echo "恢复前的数据库已另存为:$SAFETY"
echo
warn "若这份备份来自另一台机器或另一次安装,请确认主密钥也是配套的那一把;"
warn "密钥不匹配时服务能启动,但用户 UUID 与节点私钥会解密失败。"
