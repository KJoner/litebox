#!/usr/bin/env bash
# LiteBox 一键安装/升级脚本。
#
#   bash <(wget -qO- https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
#   bash <(curl -fsSL https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
#
# 它做这些事:检查并补齐 make / Node.js / Go → 拉源码 → 构建主控与节点用的
# sing-box、拉取 Mieru 用的 mita/mieru → 调用 scripts/install.sh 安装或
# 升级主控 → 打印登录信息。
#
# 全程幂等,重复执行即为升级:已装且版本达标的依赖不会重复安装,
# 已有的数据库、主密钥、配置文件都不会被覆盖。
#
# 环境变量:
#   LITEBOX_REPO       源码仓库地址
#   LITEBOX_BRANCH     分支,默认 master
#   LITEBOX_SRC        源码目录,默认 /usr/local/src/litebox
#   GO_MIN / NODE_MIN  依赖的最低主版本
#   SKIP_SINGBOX=1     跳过节点用 sing-box 的构建(它要拉 sing-box 源码,最慢)
#   SKIP_MIERU=1       跳过 Mieru 用的 mita/mieru 下载(不打算用 Mieru 入口时)
#   WITH_SNELL=1       额外构建预览版 sing-box(1.14)。**只有要用 Snell 入口
#                      才需要** —— 它是上游的 rc,而多编译一次要几分钟。
#                      不构建的话面板只是不提供「安装预览版」那个选项。
set -euo pipefail

REPO="${LITEBOX_REPO:-https://github.com/KJoner/litebox.git}"
BRANCH="${LITEBOX_BRANCH:-master}"
SRC_DIR="${LITEBOX_SRC:-/usr/local/src/litebox}"
GO_MIN="${GO_MIN:-1.26}"
NODE_MIN="${NODE_MIN:-22}"
NODE_VERSION="${NODE_VERSION:-v22.20.0}"

INSTALL_DIR=/opt/litebox
CONFIG_FILE=/etc/litebox/litebox.yaml
ENV_FILE=/etc/litebox/litebox.env

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m  ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "请用 root 执行(sudo -i 之后再运行)"

# ---------------------------------------------------------------- 系统与包管理

case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64; GO_ARCH=amd64; NODE_ARCH=x64 ;;
    aarch64|arm64) ARCH=arm64; GO_ARCH=arm64; NODE_ARCH=arm64 ;;
    *) die "不支持的 CPU 架构 $(uname -m),目前只支持 x86_64 与 aarch64" ;;
esac

if command -v apt-get >/dev/null; then
    PKG_UPDATE="apt-get update -qq"
    PKG_INSTALL="apt-get install -y -qq"
elif command -v dnf >/dev/null; then
    PKG_UPDATE="true"
    PKG_INSTALL="dnf install -y -q"
elif command -v yum >/dev/null; then
    PKG_UPDATE="true"
    PKG_INSTALL="yum install -y -q"
else
    die "找不到 apt/dnf/yum,请手动安装 git curl tar make 后重试"
fi

# 版本比较:$1 >= $2 时返回真。sort -V 认得 1.26 > 1.9 这种语义化顺序,
# 直接用字符串比会判错。
version_ge() {
    [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]
}

ensure_pkgs() {
    local missing=()
    for cmd in "$@"; do
        command -v "$cmd" >/dev/null || missing+=("$cmd")
    done
    [ ${#missing[@]} -eq 0 ] && return 0
    log "安装缺失的系统包:${missing[*]}"
    $PKG_UPDATE >/dev/null 2>&1 || true
    $PKG_INSTALL "${missing[@]}" >/dev/null
}

log "检查基础工具"
ensure_pkgs git curl tar make
ok "git / curl / tar / make 就绪"

# ---------------------------------------------------------------- Go

install_go() {
    local version
    version="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
    [ -n "$version" ] || die "无法获取 Go 最新版本号,请检查网络"

    log "安装 $version"
    local tarball="/tmp/${version}.linux-${GO_ARCH}.tar.gz"
    curl -fL --progress-bar -o "$tarball" "https://go.dev/dl/${version}.linux-${GO_ARCH}.tar.gz"
    # 必须先删干净再解包:tar 是覆盖式的,旧版本残留的文件会留在 /usr/local/go 里。
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tarball"
    rm -f "$tarball"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

log "检查 Go(需要 >= $GO_MIN)"
if command -v go >/dev/null; then
    GO_CURRENT="$(go version | awk '{print $3}' | sed 's/^go//')"
    if version_ge "$GO_CURRENT" "$GO_MIN"; then
        ok "已安装 go$GO_CURRENT"
    else
        warn "已安装 go$GO_CURRENT,低于要求的 $GO_MIN,将升级"
        install_go
        ok "已升级到 $(go version | awk '{print $3}')"
    fi
else
    install_go
    ok "已安装 $(go version | awk '{print $3}')"
fi

# ---------------------------------------------------------------- Node.js

install_node() {
    # 用官方二进制包而不是发行版仓库:Debian 12 的 nodejs 是 18,
    # 而前端构建需要 22+,装了发行版包只会在 npm run build 时才失败。
    log "安装 Node.js $NODE_VERSION"
    local name="node-${NODE_VERSION}-linux-${NODE_ARCH}"
    local tarball="/tmp/${name}.tar.xz"
    curl -fL --progress-bar -o "$tarball" \
        "https://nodejs.org/dist/${NODE_VERSION}/${name}.tar.xz"
    rm -rf "/usr/local/lib/nodejs/${name}"
    mkdir -p /usr/local/lib/nodejs
    tar -C /usr/local/lib/nodejs -xJf "$tarball"
    rm -f "$tarball"
    ln -sf "/usr/local/lib/nodejs/${name}/bin/node" /usr/local/bin/node
    ln -sf "/usr/local/lib/nodejs/${name}/bin/npm"  /usr/local/bin/npm
    ln -sf "/usr/local/lib/nodejs/${name}/bin/npx"  /usr/local/bin/npx
}

log "检查 Node.js(需要 >= $NODE_MIN)"
ensure_pkgs xz-utils >/dev/null 2>&1 || true
if command -v node >/dev/null; then
    NODE_CURRENT="$(node --version | sed 's/^v//')"
    if version_ge "$NODE_CURRENT" "$NODE_MIN"; then
        ok "已安装 node v$NODE_CURRENT"
    else
        warn "已安装 node v$NODE_CURRENT,低于要求的 $NODE_MIN,将升级"
        install_node
        ok "已升级到 $(node --version)"
    fi
else
    install_node
    ok "已安装 $(node --version)"
fi
command -v npm >/dev/null || die "npm 不可用,请检查 Node.js 安装"

# ---------------------------------------------------------------- 源码

if [ -d "$SRC_DIR/.git" ]; then
    log "更新源码 $SRC_DIR"
    git -C "$SRC_DIR" fetch --depth 1 origin "$BRANCH"
    # 硬重置到远端:本地如果有构建产物之类的改动,rebase 会失败卡住升级。
    git -C "$SRC_DIR" reset --hard "origin/$BRANCH"
else
    log "拉取源码到 $SRC_DIR"
    mkdir -p "$(dirname "$SRC_DIR")"
    git clone --depth 1 --branch "$BRANCH" "$REPO" "$SRC_DIR"
fi
cd "$SRC_DIR"
ok "源码版本 $(git rev-parse --short HEAD)"

# ---------------------------------------------------------------- 构建

# build-linux 依赖 web,前端会被一并构建并 embed 进二进制,不必单独跑一遍。
log "构建主控(含前端,首次约 2~5 分钟)"
make build-linux
BIN="bin/litebox-linux-$ARCH"
[ -f "$BIN" ] || die "未生成 $BIN"
ok "$BIN($(du -h "$BIN" | cut -f1))"

if [ "${SKIP_SINGBOX:-0}" = "1" ]; then
    warn "已跳过 sing-box 构建。节点安装前需要自行执行 bash scripts/build-singbox.sh"
elif ls assets/singbox/sing-box-linux-* >/dev/null 2>&1; then
    ok "已有节点用 sing-box,跳过构建(要重建请删掉 assets/singbox/ 后重跑)"
else
    log "构建节点用的 sing-box(带 with_v2ray_api 标签,较慢)"
    bash scripts/build-singbox.sh
    ok "sing-box 已构建"
fi

# 预览版 sing-box(1.14)。**只有要用 Snell 入口才需要。**
#
# 默认不构建:它是上游的 rc,绝大多数机器不会用它,而多编译一次要几分钟。
# 没有它面板只是不提供「安装预览版」那个选项,VLESS / Shadowsocks / Mieru
# 一切照旧。
#
# **已经装过面板的机器,后来想加 Snell,重跑一次这个脚本并带上
# WITH_SNELL=1 就行** —— 它会补构建预览版并拷到 $INSTALL_DIR,
# 主控那一侧不用改任何配置(两支放同一个目录,binary_dir 已经指着它)。
if [ "${WITH_SNELL:-0}" = "1" ]; then
    if ls assets/singbox/sing-box-preview-linux-* >/dev/null 2>&1; then
        ok "已有预览版 sing-box,跳过构建(要重建请删掉 assets/singbox/sing-box-preview-* 后重跑)"
    else
        log "构建预览版 sing-box(1.14,Snell 入口需要,较慢)"
        bash -c 'SINGBOX_CHANNEL=preview bash scripts/build-singbox.sh'
        ok "预览版 sing-box 已构建"
    fi
elif ls assets/singbox/sing-box-preview-linux-* >/dev/null 2>&1; then
    # 上次带 WITH_SNELL=1 装过,这次没带 —— 别把它悄悄扔掉:
    # 那会让一台正在跑 Snell 入口的机器,在下一次「重新安装」时
    # 拿不到预览版二进制,而管理员只是重跑了一遍安装脚本。
    ok "已有预览版 sing-box(上次构建的),保留"
fi

# mita/mieru 不自己构建,拉上游 release 的原样二进制(约 13MB,很快)。
# 拉不到不中止安装:面板没有 Mieru 入口时用不到它们,而这一步依赖
# GitHub release 能不能连上 —— 让它挡住整个安装,换来的是一台
# 装了一半的机器。真要用时面板会明确报「请先执行 scripts/fetch-mieru.sh」。
if [ "${SKIP_MIERU:-0}" = "1" ]; then
    warn "已跳过 Mieru 二进制下载。要用 Mieru 入口时执行 bash scripts/fetch-mieru.sh"
elif ls assets/mieru/mita-linux-* >/dev/null 2>&1; then
    ok "已有 Mieru 二进制,跳过下载(要重拉请删掉 assets/mieru/ 后重跑)"
else
    log "下载 Mieru 用的 mita/mieru(上游 release,带 sha256 校验)"
    if bash scripts/fetch-mieru.sh; then
        ok "mita/mieru 已就绪"
    else
        warn "Mieru 二进制下载失败,已跳过。要用 Mieru 入口时重跑 bash scripts/fetch-mieru.sh"
    fi
fi

# ---------------------------------------------------------------- 安装主控

FIRST_INSTALL=0
[ -f "$ENV_FILE" ] || FIRST_INSTALL=1

log "安装/升级主控"
if [ "$FIRST_INSTALL" = "1" ]; then
    bash scripts/install.sh
else
    # 已装过就走升级路径:它会先备份数据库,失败自动回退二进制。
    bash scripts/upgrade.sh
fi

# 把节点用的二进制放到主控能读到的位置。
# 主控以 litebox 用户运行,而它只需要**读** —— chown 只是让权限一目了然,
# 失败了也不影响(0644 + 目录 0755 本来就人人可读)。
if ls assets/singbox/sing-box-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/singbox"
    cp -f assets/singbox/sing-box-linux-* "$INSTALL_DIR/assets/singbox/"
    ok "节点用 sing-box 已就位"
fi
# 预览版单独拷:通配符不能合成一个 —— sing-box-linux-* 匹配不到
# sing-box-preview-linux-*,而反过来会把两支都算进上面那个判断里,
# 于是"只有预览版"的机器会被当成"正式版已就位"。
if ls assets/singbox/sing-box-preview-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/singbox"
    cp -f assets/singbox/sing-box-preview-linux-* "$INSTALL_DIR/assets/singbox/"
    ok "预览版 sing-box 已就位(节点上可以装它,然后建 Snell 入口)"
fi
if ls assets/mieru/mita-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/mieru"
    # mieru 客户端也要:Mieru 入口的部署健康检查要用它做一次真实拨测,
    # 那是本项目第一条铁律。只拷 mita 的话,部署会卡在拨测那一步。
    cp -f assets/mieru/mita-linux-* assets/mieru/mieru-linux-* "$INSTALL_DIR/assets/mieru/"
    ok "Mieru 用的 mita/mieru 已就位"
fi
chown -R litebox:litebox "$INSTALL_DIR/assets" 2>/dev/null || true

# ---------------------------------------------------------------- 收尾信息

run_litebox() {
    sudo -u litebox env "LITEBOX_MASTER_KEY=$(grep '^LITEBOX_MASTER_KEY=' "$ENV_FILE" | cut -d= -f2-)" \
        "$INSTALL_DIR/litebox" "$@" --config "$CONFIG_FILE"
}

# 面板专用的节点访问密钥:存在数据库里(主密钥加密),这里只是确保它已生成并展示公钥。
PANEL_KEY="$(run_litebox ssh-key 2>/dev/null | tail -1 || true)"

ADMIN_PASSWORD=""
if [ "$FIRST_INSTALL" = "1" ]; then
    # 首装时重置一次密码并打印。首启动生成的那个只出现在 journald 里,
    # 用管道跑脚本的人根本看不到。升级时不动密码。
    # 半角/全角冒号都剥掉,免得将来改了提示文案这里静默取到空串。
    ADMIN_PASSWORD="$(run_litebox reset-password 2>/dev/null |
        grep '新密码' | sed 's/.*[::]//' | tr -d ' \r')"
fi

systemctl is-active --quiet litebox || die "服务未运行,请查看:journalctl -u litebox -n 50"

echo
printf '\033[32m%s\033[0m\n' "LiteBox 安装完成"
echo
echo "  面板地址   http://127.0.0.1:8080  (默认只监听回环,请配置 Nginx 反代)"
if [ -n "$ADMIN_PASSWORD" ]; then
    echo "  管理员     admin"
    printf "  初始密码   \033[33m%s\033[0m\n" "$ADMIN_PASSWORD"
    echo "             登录后请立即在「设置」里修改"
fi
echo
if [ -n "$PANEL_KEY" ]; then
    echo "  面板 SSH 公钥(新增节点时会自动装进节点,只允许密钥登录的机器需手工添加):"
    echo "    $PANEL_KEY"
    echo
fi
cat <<EOF
  接下来:
    1. 配置 Nginx 与证书,示例见 $SRC_DIR/deploy/nginx/litebox.conf
    2. 登录面板 →「设置」→ 填写订阅地址的站点根(比如 https://panel.example.com)
    3. 「节点管理」→「新增节点」→ 填节点 IP 与 root 密码,
       面板会用这个密码把上面的公钥装进节点,密码用完即弃不保存
    4. 备份主密钥:$ENV_FILE  —— 丢失后全部用户与节点凭据不可恢复

  常用命令:
    systemctl status litebox
    journalctl -u litebox -f
    $INSTALL_DIR/litebox check   --config $CONFIG_FILE
    $INSTALL_DIR/litebox backup  --config $CONFIG_FILE
    $INSTALL_DIR/litebox ssh-key --config $CONFIG_FILE

  再次执行本脚本即为升级(会先自动备份数据库)。
EOF

if [ "${WITH_SNELL:-0}" != "1" ] && ! ls assets/singbox/sing-box-preview-linux-* >/dev/null 2>&1; then
    cat <<'EOF'

  想用 Snell 入口的话,重跑一次本脚本并带上 WITH_SNELL=1 —— 它会额外构建
  预览版 sing-box(1.14,Snell 是那一版才有的入站)。装完之后在节点详情的
  「入口」Tab 里,sing-box 那一行的「安装」按钮会多出「安装预览版」这一项。
  一台机器只装一支,那台机器上全部 sing-box 入口都跑在它上面。
EOF
fi
