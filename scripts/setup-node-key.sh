#!/usr/bin/env bash
# 为 LiteBox 生成一把专用的节点 SSH 密钥,并把公钥装到节点上。
#
# 为什么要专用密钥:面板需要节点的 root 权限(装 systemd 单元、重启服务),
# 这个权限收不掉。能收的是"爆炸半径" —— 用一把只给面板用的密钥,
# 泄露或轮换时不必动你自己的日常密钥,节点侧也能单独吊销。
#
# 用法:
#   ./setup-node-key.sh 192.0.2.10 22 root        # 生成并安装
#   ./setup-node-key.sh --print-only              # 只生成,自己去装
set -euo pipefail

KEY_DIR="${KEY_DIR:-/etc/litebox/keys}"
KEY_PATH="$KEY_DIR/node_ed25519"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

command -v ssh-keygen >/dev/null || die "需要 ssh-keygen"

if [ ! -f "$KEY_PATH" ]; then
    log "生成专用密钥 $KEY_PATH"
    install -d -m 0700 "$KEY_DIR"
    # 不设 passphrase:面板要无人值守地连接节点,带口令的密钥用不了。
    # 安全性靠文件权限与专用用途,而不是口令。
    ssh-keygen -t ed25519 -N '' -C 'litebox-panel' -f "$KEY_PATH" >/dev/null
    chmod 0600 "$KEY_PATH"
    chmod 0644 "$KEY_PATH.pub"
else
    log "密钥已存在,复用 $KEY_PATH"
fi

if [ "${1:-}" = "--print-only" ]; then
    echo
    echo "把下面这行加到节点的 /root/.ssh/authorized_keys:"
    echo
    cat "$KEY_PATH.pub"
    echo
    echo "私钥内容(粘贴到面板的「新增节点 → SSH 私钥」):"
    echo
    cat "$KEY_PATH"
    exit 0
fi

HOST="${1:-}"
PORT="${2:-22}"
USER="${3:-root}"
[ -n "$HOST" ] || die "用法:$0 <主机> [端口] [用户]  或  $0 --print-only"

log "把公钥安装到 $USER@$HOST:$PORT"
if command -v ssh-copy-id >/dev/null; then
    ssh-copy-id -i "$KEY_PATH.pub" -p "$PORT" "$USER@$HOST"
else
    # 没有 ssh-copy-id 时手工追加,注意不要覆盖已有条目。
    ssh -p "$PORT" "$USER@$HOST" \
        "mkdir -p ~/.ssh && chmod 700 ~/.ssh && cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys" \
        < "$KEY_PATH.pub"
fi

log "验证连接"
ssh -i "$KEY_PATH" -p "$PORT" -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
    "$USER@$HOST" 'echo 连接正常; uname -a'

echo
log "完成"
cat <<EOF

在面板里新增该节点时:
  主机地址  $HOST
  SSH 端口  $PORT
  SSH 用户  $USER
  SSH 私钥  粘贴 $KEY_PATH 的内容

私钥会用主密钥加密后存进数据库,面板不会再次显示它。

节点侧建议的加固(可选,收窄这把密钥的用途):
  1. 只允许密钥登录:sshd_config 中 PasswordAuthentication no
  2. 限制来源 IP:在 authorized_keys 该条目前加
       from="主控公网IP",no-agent-forwarding,no-X11-forwarding,no-user-rc
     注意不要加 command= —— 面板需要执行多种命令,限定单条命令会让它失效。
  3. 换 SSH 端口并配合 fail2ban

轮换这把密钥时:重新执行本脚本生成新密钥、装到各节点、
在面板里逐个更新节点的 SSH 私钥,最后从节点的 authorized_keys 删掉旧公钥。
EOF

warn "私钥 $KEY_PATH 未设口令,请确认它的权限是 0600 且目录不可被他人进入。"
