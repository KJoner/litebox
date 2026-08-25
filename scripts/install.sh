#!/usr/bin/env bash
# LiteBox 主控安装脚本。
#
# 幂等:重复执行不会覆盖已有的主密钥、配置与数据库。
# 用法(在主控机器上以 root 执行):
#   ./install.sh                     从当前目录的 bin/litebox-linux-amd64 安装
#   BINARY=/path/to/litebox ./install.sh
set -euo pipefail

LITEBOX_USER="${LITEBOX_USER:-litebox}"
INSTALL_DIR="${INSTALL_DIR:-/opt/litebox}"
CONFIG_DIR="${CONFIG_DIR:-/etc/litebox}"
DATA_DIR="${DATA_DIR:-/var/lib/litebox}"
BINARY="${BINARY:-}"

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请以 root 执行"
command -v systemctl >/dev/null || die "未检测到 systemd"

# ---- 定位二进制 ----
if [ -z "$BINARY" ]; then
    for candidate in \
        "$(dirname "$0")/../bin/litebox-linux-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
        "$(dirname "$0")/../bin/litebox" \
        "./litebox"; do
        if [ -x "$candidate" ]; then BINARY="$candidate"; break; fi
    done
fi
[ -n "$BINARY" ] && [ -x "$BINARY" ] || die "找不到 litebox 二进制,请用 BINARY=... 指定"

# ---- 用户与目录 ----
if ! id "$LITEBOX_USER" >/dev/null 2>&1; then
    log "创建系统用户 $LITEBOX_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$LITEBOX_USER"
else
    log "系统用户 $LITEBOX_USER 已存在"
fi

install -d -m 0755 "$INSTALL_DIR"
install -d -m 0750 -o "$LITEBOX_USER" -g "$LITEBOX_USER" "$DATA_DIR"
# 配置目录含主密钥,只有属主可进入。
install -d -m 0700 -o "$LITEBOX_USER" -g "$LITEBOX_USER" "$CONFIG_DIR"
install -d -m 0755 "$INSTALL_DIR/assets/singbox"
# Mieru 的 mita/mieru 二进制。与 sing-box 分开放:来源不同(那边是我们按
# 固定构建标签自己构建的,这边是上游 release 原样拉下来的),混在一起会让
# 「这个文件是谁产生的」变成一个要翻脚本才答得出的问题。
# 目录留着即使还没拉二进制 —— 有目录没文件时面板报的是「未找到 mita 二进制,
# 请先执行 scripts/fetch-mieru.sh」,而连目录都没有时管理员会先怀疑装漏了。
install -d -m 0755 "$INSTALL_DIR/assets/mieru"

log "安装二进制到 $INSTALL_DIR/litebox"
install -m 0755 "$BINARY" "$INSTALL_DIR/litebox.new"
mv "$INSTALL_DIR/litebox.new" "$INSTALL_DIR/litebox"

# ---- 主密钥 ----
ENV_FILE="$CONFIG_DIR/litebox.env"
if [ -f "$ENV_FILE" ] && grep -q '^LITEBOX_MASTER_KEY=' "$ENV_FILE"; then
    log "主密钥已存在,保持不变"
else
    log "生成主密钥"
    KEY="$("$INSTALL_DIR/litebox" genkey 2>/dev/null)"
    umask 077
    printf 'LITEBOX_MASTER_KEY=%s\n' "$KEY" > "$ENV_FILE"
    chown "$LITEBOX_USER:$LITEBOX_USER" "$ENV_FILE"
    chmod 0600 "$ENV_FILE"
    echo
    warn "主密钥已写入 $ENV_FILE"
    warn "请立刻把它抄写到另一个安全的地方。"
    warn "主密钥丢失后,数据库中的用户 UUID 与节点私钥将永久无法还原,"
    warn "备份文件也救不回来 —— 只能重建全部用户与节点。"
    echo
fi

# ---- 配置文件 ----
CONFIG_FILE="$CONFIG_DIR/litebox.yaml"
if [ -f "$CONFIG_FILE" ]; then
    log "配置文件已存在,保持不变"
else
    log "写入默认配置 $CONFIG_FILE"
    cat > "$CONFIG_FILE" <<EOF
http:
  listen: "127.0.0.1:8080"
  # 订阅地址的站点根。这只是首次启动的默认值,
  # 装完之后在面板「设置 → 订阅地址」里改就行,不用动这个文件。
  base_url: "http://127.0.0.1:8080"
  # 经 HTTPS 反代时必须改成 true,否则会话 Cookie 不带 Secure。
  secure_cookie: false

database:
  path: "$DATA_DIR/litebox.db"

node:
  binary_dir: "$INSTALL_DIR/assets/singbox"
  # Mieru 用的 mita/mieru 二进制,由 scripts/fetch-mieru.sh 拉取。
  # 写绝对路径而不是靠默认值:默认是相对路径 assets/mieru,只在
  # WorkingDirectory 恰好是 $INSTALL_DIR 时才对 —— 那是一个能用但没人
  # 说得清为什么能用的巧合,改一下 systemd 单元就会变成「找不到二进制」。
  mieru_binary_dir: "$INSTALL_DIR/assets/mieru"
  # 节点资源采集间隔。放得比流量同步宽,避免和部署抢节点连接锁;负数则关闭。
  metrics_interval: 5m
  metrics_retention: 168h

traffic:
  sync_interval: 60s

log:
  level: "info"
  format: "text"
EOF
    chown "$LITEBOX_USER:$LITEBOX_USER" "$CONFIG_FILE"
    chmod 0640 "$CONFIG_FILE"
fi

# ---- systemd ----
UNIT=/etc/systemd/system/litebox.service
log "写入 systemd 单元"
cat > "$UNIT" <<EOF
[Unit]
Description=LiteBox Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$LITEBOX_USER
Group=$LITEBOX_USER
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/litebox serve --config $CONFIG_FILE
EnvironmentFile=$ENV_FILE
Restart=on-failure
RestartSec=5
TimeoutStopSec=20

NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectHostname=true
ProtectClock=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
CapabilityBoundingSet=
AmbientCapabilities=

ReadWritePaths=$DATA_DIR
# 主控要为每个节点维持 SSH 长连接,句柄数不能太低。
LimitNOFILE=8192

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable litebox >/dev/null

# ---- 迁移与自检 ----
log "执行数据库迁移"
sudo -u "$LITEBOX_USER" env "LITEBOX_MASTER_KEY=$(grep '^LITEBOX_MASTER_KEY=' "$ENV_FILE" | cut -d= -f2-)" \
    "$INSTALL_DIR/litebox" migrate --config "$CONFIG_FILE"

log "启动服务"
systemctl restart litebox
sleep 2

if systemctl is-active --quiet litebox; then
    log "LiteBox 已启动"
else
    die "启动失败,请查看:journalctl -u litebox -n 50"
fi

echo
log "安装完成"
cat <<EOF

后续步骤:
  1. 查看初始管理员密码:journalctl -u litebox | grep 初始密码
  2. 登录后在「设置 → 订阅地址」里填实际域名
     经 HTTPS 反代时还要把 $CONFIG_FILE 里的 secure_cookie 改成 true
  3. 构建节点用的 sing-box 并放到 $INSTALL_DIR/assets/singbox/
     (见 scripts/build-singbox.sh)
     要用 Snell 入口的话,再构建一份预览版放同一个目录 ——
     Snell 是 sing-box 1.14 才有的入站,而 1.14 目前只有预览版:
     OUTPUT_DIR=$INSTALL_DIR/assets/singbox SINGBOX_CHANNEL=preview bash scripts/build-singbox.sh
     不构建它也没关系:面板只是不提供「安装预览版」那个选项,
     VLESS 与 Shadowsocks 一切照旧。
     要用 Mieru 入口的话,再拉一次 mita/mieru:
     OUTPUT_DIR=$INSTALL_DIR/assets/mieru bash scripts/fetch-mieru.sh
  4. 配置 Nginx 反代,示例见 deploy/nginx/litebox.conf
  5. 备份主密钥:$ENV_FILE

常用命令:
  systemctl status litebox
  journalctl -u litebox -f
  $INSTALL_DIR/litebox check  --config $CONFIG_FILE
  $INSTALL_DIR/litebox backup --config $CONFIG_FILE
EOF
