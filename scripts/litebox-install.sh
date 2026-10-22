#!/usr/bin/env bash
# LiteBox 一键安装/升级脚本。
#
#   bash <(wget -qO- https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
#   bash <(curl -fsSL https://raw.githubusercontent.com/KJoner/litebox/master/scripts/litebox-install.sh)
#
# 它做这些事:检查并补齐 git / curl / make → 拉源码 → 把 Go 与 Node.js 升到各自那条线上
# 的最新补丁 → 构建主控、节点用的 sing-box 与出站探测辅助程序 litebox-probe → 拉取
# Mieru 用的 mita/mieru 与 realm → 调用 scripts/install.sh 安装或升级主控 → 把节点用的
# 二进制放到 /opt/litebox/assets/ 下 → 打印登录信息。
#
# 全程幂等,重复执行即为升级:已是最新补丁的 Go / Node.js 不会重复安装,
# 已有的数据库、主密钥、配置文件都不会被覆盖。Go 或 Node.js 出了安全补丁,重跑一次即可:
# 面板与 litebox-probe 每次都用新的 Go 重新构建;节点用的 sing-box 在构建它的 Go 换过时
# 也会重建,但已经装在节点上的那一份要到各节点卡片上「重新安装」才换(会断开在线连接)。
#
# litebox-probe 每次都重建 —— 它与面板之间有协议版本号,面板升级之后还分发旧的那一份,节点会报「版本不符」;
# 换上去之后不用重启面板,各节点在下一轮出站探测时自动换成新文件。
# 只用 scripts/upgrade.sh 升级时这一步不会做,要自己执行 bash scripts/build-probe.sh。
#
# 环境变量:
#   LITEBOX_REPO       源码仓库地址
#   LITEBOX_BRANCH     分支,默认 master
#   LITEBOX_SRC        源码目录,默认 /usr/local/src/litebox
#   GO_MIN             Go 的最低版本,默认取 go.mod 的 go 指令(如 1.26.10)
#   NODE_MIN           Node.js 的最低版本,默认 22.23.4
#   SKIP_SINGBOX=1     跳过节点用 sing-box 的构建(它要拉 sing-box 源码,最慢)
#   SKIP_MIERU=1       跳过 Mieru 用的 mita/mieru 下载(不打算用 Mieru 入口时)
#   SKIP_REALM=1       跳过 realm 下载(不打算用 realm 转发时)
#
# sing-box 只分发一支(固定 v1.14.0,本身就带 Snell);V14 的 WITH_SNELL 预览版通道已撤掉,
# 旧面板留在 /opt/litebox/assets/singbox/ 下的预览版文件会被顺手删掉。
set -euo pipefail

REPO="${LITEBOX_REPO:-https://github.com/KJoner/litebox.git}"
BRANCH="${LITEBOX_BRANCH:-master}"
SRC_DIR="${LITEBOX_SRC:-/usr/local/src/litebox}"

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

# ---------------------------------------------------------------- 工具链(Go / Node.js)
#
# 两者都不只看主版本,还要跟到【同一条线上的最新补丁】,补丁一出来重跑这个脚本就能换上。
# 原来只判「>= 1.26 / >= 22」,一台装过一次的机器重跑多少次都还是最初那一版 ——
# Go 与 Node.js 的安全修复全在补丁版本里,一个都进不来。
# Go 这一侧更要紧:标准库是静态编进二进制的,面板、litebox-probe、节点用的 sing-box
# 都带着【构建时】那一版标准库的漏洞,光装新 Go 不够,要用它重新构建才算修好(见下面「构建」)。
# 查不到最新补丁(go.dev / nodejs.org 连不上)时只按下限检查并警告,不中止。

# 两个版本号里较高的那个,空串当作没有。
max_version() {
    printf '%s\n%s\n' "$1" "$2" | sed '/^$/d' | sort -V | tail -1
}

# 下载并核对 sha256。校验值与安装包出自同一个站点,挡不住站点本身出事;
# 挡得住的是下到一半、被截断或被缓存坏的文件 —— 解包到一半失败会把 /usr/local/go 留成空目录。
fetch_checked() {  # $1 URL,$2 期望的 sha256,$3 存到哪里
    [ -n "$2" ] || die "取不到 $1 的 sha256,不安装没有校验过的文件"
    curl -fL --progress-bar -o "$3" "$1" || die "下载失败:$1"
    if ! echo "$2  $3" | sha256sum -c --status -; then
        rm -f "$3"
        die "sha256 不符,已删除下载的文件:$1"
    fi
}

# Go 的下限取自 go.mod 的 go 指令(带补丁号)—— CI 的 setup-go 读的也是这一行,不在这里再写一遍。
# 要更新下限就改 go.mod,改完之后 GOTOOLCHAIN=auto 的开发机也会自动换到那一版。
GO_FLOOR="${GO_MIN:-$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod)}"
[ -n "$GO_FLOOR" ] || die "读不到 go.mod 里的 Go 版本"

# 只问本机装的那一份。在源码目录里直接跑 go version,go.mod 要求更高时 go 命令会
# 自动下载新工具链、报的是那一个的版本号 —— 检查会被它骗过去,而 sing-box 的构建
# (GOTOOLCHAIN=local)用的仍是本机那个旧的。
local_go_version() {
    command -v go >/dev/null || return 0
    (cd / && GOTOOLCHAIN=local go env GOVERSION 2>/dev/null) | sed -n 's/^go\([0-9][0-9.]*\).*/\1/p' || true
}

# 同一条线(如 1.26)上最新的补丁号,查不到时输出空。
latest_go_patch() {
    local line_re
    line_re="$(printf '%s' "$1" | sed 's/\./\\./g')"
    curl -fsSL --max-time 30 'https://go.dev/dl/?mode=json' 2>/dev/null |
        grep -o "\"version\": *\"go${line_re}\.[0-9][0-9]*\"" |
        sed 's/.*"go\([0-9.]*\)"$/\1/' | sort -V | tail -1 || true
}

install_go() {  # $1 版本号,如 1.26.10
    local file="go$1.linux-${GO_ARCH}.tar.gz" sum
    log "安装 go$1"
    sum="$(curl -fsSL --max-time 30 "https://dl.google.com/go/${file}.sha256" 2>/dev/null |
        grep -oE '^[0-9a-f]{64}' | head -1 || true)"
    fetch_checked "https://go.dev/dl/${file}" "$sum" "/tmp/${file}"
    # 必须先删干净再解包:tar 是覆盖式的,旧版本残留的文件会留在 /usr/local/go 里。
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${file}"
    rm -f "/tmp/${file}"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
    hash -r
}

GO_CURRENT="$(local_go_version)"
# 已经装了更新的一条线(比如 1.27)就跟那一条的补丁,不降回去。
GO_LINE="$(max_version "$GO_CURRENT" "$GO_FLOOR" | cut -d. -f1,2)"
GO_LATEST="$(latest_go_patch "$GO_LINE")"
GO_WANT="$(max_version "$GO_FLOOR" "$GO_LATEST")"

log "检查 Go(不低于 $GO_FLOOR,并跟到 $GO_LINE 线的最新补丁)"
[ -n "$GO_LATEST" ] || warn "查不到 Go $GO_LINE 线的最新补丁(go.dev 连不上?),只按下限 $GO_FLOOR 检查"
if [ -n "$GO_CURRENT" ] && version_ge "$GO_CURRENT" "$GO_WANT"; then
    ok "已安装 go$GO_CURRENT"
else
    [ -z "$GO_CURRENT" ] || warn "已安装 go$GO_CURRENT,换成 go$GO_WANT(安全补丁要重新构建才会进二进制,下面会重新构建)"
    install_go "$GO_WANT"
    GO_CURRENT="$(local_go_version)"
    [ "$GO_CURRENT" = "$GO_WANT" ] ||
        die "PATH 里的 go 是 ${GO_CURRENT:-(找不到)},不是刚装到 /usr/local/bin/go 的 go$GO_WANT,请调整 PATH 后重跑"
    ok "已安装 go$GO_CURRENT"
fi
# 后面的构建一律用这一份,不让 go 命令按 go.mod 自己去下载另一版工具链 ——
# 那样编进二进制的与 build-metadata.json 里记的、与上面检查过的,三者都可能对不上。
export GOTOOLCHAIN=local
GO_BUILD_VERSION="go$GO_CURRENT"

# Node.js 只用来构建前端(产物 embed 进面板二进制),面板运行时不跑它;
# 但它装在这台机器上、每次升级都要跑 npm ci,所以同样跟到最新补丁。
# 下限是已知修过安全问题的版本(2026-10-21 的安全发布),要更新就改这里。
NODE_FLOOR="${NODE_MIN:-22.23.4}"
NODE_FLOOR_MAJOR="${NODE_FLOOR%%.*}"

local_node_version() {
    command -v node >/dev/null || return 0
    node --version 2>/dev/null | sed -n 's/^v\([0-9][0-9.]*\)$/\1/p' || true
}

# 同一条线(主版本,如 22)上最新的补丁号,查不到时输出空。
latest_node_patch() {
    curl -fsSL --max-time 30 "https://nodejs.org/dist/latest-v$1.x/SHASUMS256.txt" 2>/dev/null |
        sed -n "s/^[0-9a-f]\{64\}  node-v\($1\.[0-9][0-9]*\.[0-9][0-9]*\)-linux-${NODE_ARCH}\.tar\.xz\$/\1/p" |
        head -1 || true
}

install_node() {  # $1 版本号,如 22.23.4
    # 用官方二进制包而不是发行版仓库:Debian 12 的 nodejs 是 18,
    # 而前端构建需要 22+,装了发行版包只会在 npm run build 时才失败。
    local name="node-v$1-linux-${NODE_ARCH}" sum
    log "安装 Node.js v$1"
    sum="$(curl -fsSL --max-time 30 "https://nodejs.org/dist/v$1/SHASUMS256.txt" 2>/dev/null |
        sed -n "s/^\([0-9a-f]\{64\}\)  ${name}\.tar\.xz\$/\1/p" | head -1 || true)"
    fetch_checked "https://nodejs.org/dist/v$1/${name}.tar.xz" "$sum" "/tmp/${name}.tar.xz"
    mkdir -p /usr/local/lib/nodejs
    rm -rf "/usr/local/lib/nodejs/${name}"
    tar -C /usr/local/lib/nodejs -xJf "/tmp/${name}.tar.xz"
    rm -f "/tmp/${name}.tar.xz"
    ln -sf "/usr/local/lib/nodejs/${name}/bin/node" /usr/local/bin/node
    ln -sf "/usr/local/lib/nodejs/${name}/bin/npm"  /usr/local/bin/npm
    ln -sf "/usr/local/lib/nodejs/${name}/bin/npx"  /usr/local/bin/npx
    # 这个目录只放本脚本装的 Node.js。旧版本留着不会被执行,
    # 但漏洞扫描会一直报它们,看的人会以为那是正在用的那一份。
    find /usr/local/lib/nodejs -mindepth 1 -maxdepth 1 -type d -name 'node-v*-linux-*' \
        ! -name "$name" -exec rm -rf {} +
    hash -r
}

ensure_pkgs xz-utils >/dev/null 2>&1 || true
NODE_CURRENT="$(local_node_version)"
NODE_LINE="$NODE_FLOOR_MAJOR"
# 已经装了更新的 LTS 线(偶数主版本)就跟那一条;奇数线只维护半年,不跟,换回下限那一条。
if [ -n "$NODE_CURRENT" ] && [ "${NODE_CURRENT%%.*}" -gt "$NODE_FLOOR_MAJOR" ] &&
    [ $(( ${NODE_CURRENT%%.*} % 2 )) -eq 0 ]; then
    NODE_LINE="${NODE_CURRENT%%.*}"
fi
NODE_LATEST="$(latest_node_patch "$NODE_LINE")"
if [ "$NODE_LINE" = "$NODE_FLOOR_MAJOR" ]; then
    NODE_WANT="$(max_version "$NODE_FLOOR" "$NODE_LATEST")"
else
    NODE_WANT="$NODE_LATEST"
fi

log "检查 Node.js(不低于 $NODE_FLOOR,并跟到 v$NODE_LINE 线的最新补丁)"
[ -n "$NODE_LATEST" ] || warn "查不到 Node.js v$NODE_LINE 线的最新补丁(nodejs.org 连不上?),只按下限检查"
if [ -n "$NODE_CURRENT" ] && [ "${NODE_CURRENT%%.*}" = "$NODE_LINE" ] &&
    { [ -z "$NODE_WANT" ] || version_ge "$NODE_CURRENT" "$NODE_WANT"; }; then
    ok "已安装 node v$NODE_CURRENT"
else
    [ -z "$NODE_CURRENT" ] || warn "已安装 node v$NODE_CURRENT,换成 v$NODE_WANT"
    install_node "$NODE_WANT"
    NODE_CURRENT="$(local_node_version)"
    [ "$NODE_CURRENT" = "$NODE_WANT" ] ||
        die "PATH 里的 node 是 ${NODE_CURRENT:-(找不到)},不是刚装到 /usr/local/bin/node 的 v$NODE_WANT,请调整 PATH 后重跑"
    ok "已安装 node v$NODE_CURRENT"
fi
command -v npm >/dev/null || die "npm 不可用,请检查 Node.js 安装"

# ---------------------------------------------------------------- 构建

# build-linux 依赖 web,前端会被一并构建并 embed 进二进制,不必单独跑一遍。
log "构建主控(含前端,首次约 2~5 分钟)"
make build-linux
BIN="bin/litebox-linux-$ARCH"
[ -f "$BIN" ] || die "未生成 $BIN"
ok "$BIN($(du -h "$BIN" | cut -f1),$GO_BUILD_VERSION 构建)"

# 已有的构建只在【sing-box 版本与构建它的 Go 都对得上】时才跳过。只看文件在不在的话,
# 升级面板的人会一直分发上一次构建的那一版 —— 节点上装的仍是旧版,而 Snell 这类要新版本的
# 入站在那些机器上建不了。只看 sing-box 版本也不够:上面已经换了打过补丁的 Go,
# 而节点用的 sing-box 仍带着旧标准库的漏洞 —— 它是节点上直接对着公网的那个进程。
# 版本号从 build-singbox.sh 里那一行读,不在这里再写一遍;构建它的 Go 记在 build-metadata.json。
WANT_SINGBOX="$(sed -n 's/^SINGBOX_VERSION="\${SINGBOX_VERSION:-\([^}]*\)}"$/\1/p' scripts/build-singbox.sh)"
HAVE_SINGBOX="$(sed -n 's/.*"singbox_version": *"\([^"]*\)".*/\1/p' assets/singbox/build-metadata.json 2>/dev/null || true)"
HAVE_SINGBOX_GO="$(sed -n 's/.*"go_version": *"\([^"]*\)".*/\1/p' assets/singbox/build-metadata.json 2>/dev/null || true)"
SINGBOX_UPGRADED=0
SINGBOX_REBUILD_REASON=""
if [ "${SKIP_SINGBOX:-0}" = "1" ]; then
    warn "已跳过 sing-box 构建。节点安装前需要自行执行 bash scripts/build-singbox.sh"
elif ls assets/singbox/sing-box-linux-* >/dev/null 2>&1 &&
    [ -n "$WANT_SINGBOX" ] && [ "$HAVE_SINGBOX" = "$WANT_SINGBOX" ] &&
    [ "$HAVE_SINGBOX_GO" = "$GO_BUILD_VERSION" ]; then
    ok "已有节点用 sing-box $HAVE_SINGBOX($HAVE_SINGBOX_GO 构建),跳过构建(要重建请删掉 assets/singbox/ 后重跑)"
else
    if ls assets/singbox/sing-box-linux-* >/dev/null 2>&1; then
        if [ "$HAVE_SINGBOX" != "$WANT_SINGBOX" ]; then
            SINGBOX_REBUILD_REASON="版本 ${HAVE_SINGBOX:-未知} → ${WANT_SINGBOX}"
        else
            SINGBOX_REBUILD_REASON="构建它的 Go ${HAVE_SINGBOX_GO:-未知} → ${GO_BUILD_VERSION}"
        fi
        log "重新构建节点用的 sing-box(${SINGBOX_REBUILD_REASON},较慢)"
        SINGBOX_UPGRADED=1
    else
        log "构建节点用的 sing-box ${WANT_SINGBOX}(带 with_v2ray_api 标签,较慢)"
    fi
    bash scripts/build-singbox.sh
    ok "sing-box 已构建"
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

# realm(V15 的第二种转发引擎)同理:上游 release 的 musl 静态二进制(约 6MB),
# sha256 写死在脚本里。拉不到不中止安装 —— 不用 realm 转发的面板用不到它。
if [ "${SKIP_REALM:-0}" = "1" ]; then
    warn "已跳过 realm 二进制下载。要用 realm 转发时执行 bash scripts/fetch-realm.sh"
elif ls assets/realm/realm-linux-* >/dev/null 2>&1; then
    ok "已有 realm 二进制,跳过下载(要重拉请删掉 assets/realm/ 后重跑)"
else
    log "下载 realm(上游 release,带 sha256 校验)"
    if bash scripts/fetch-realm.sh; then
        ok "realm 已就绪"
    else
        warn "realm 二进制下载失败,已跳过。要用 realm 转发时重跑 bash scripts/fetch-realm.sh"
    fi
fi

# 出站网络探测的辅助程序从本仓库源码构建(几秒钟)。每次都重建:它与面板之间有协议版本号,
# 面板升级之后还分发旧的那一份,节点会一直报「版本不符」。构建失败不中止安装 ——
# 没有它只是出站探测报「工具不可用」,其余功能不受影响。
log "构建出站网络探测辅助程序"
if bash scripts/build-probe.sh; then
    ok "litebox-probe 已就绪"
else
    warn "litebox-probe 构建失败,已跳过。要用出站探测时重跑 bash scripts/build-probe.sh"
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
# V14 的预览版通道已撤掉(1.14 正式版本身就带 Snell),面板不再读
# sing-box-preview-linux-*。旧版面板留下的那几个文件各三十几 MB,顺手删掉 ——
# 留着只会让人以为它们还有用。
rm -f "$INSTALL_DIR"/assets/singbox/sing-box-preview-linux-* 2>/dev/null || true
if ls assets/mieru/mita-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/mieru"
    # mieru 客户端也要:Mieru 入口的部署健康检查要用它做一次真实拨测,
    # 那是本项目第一条铁律。只拷 mita 的话,部署会卡在拨测那一步。
    cp -f assets/mieru/mita-linux-* assets/mieru/mieru-linux-* "$INSTALL_DIR/assets/mieru/"
    ok "Mieru 用的 mita/mieru 已就位"
fi
if ls assets/realm/realm-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/realm"
    cp -f assets/realm/realm-linux-* "$INSTALL_DIR/assets/realm/"
    ok "realm 已就位"
fi
if ls assets/probe/litebox-probe-linux-* >/dev/null 2>&1; then
    install -d -m 0755 "$INSTALL_DIR/assets/probe"
    cp -f assets/probe/litebox-probe-linux-* "$INSTALL_DIR/assets/probe/"
    ok "出站探测辅助程序已就位"
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

if [ "$SINGBOX_UPGRADED" = "1" ]; then
    cat <<EOF

  节点用的 sing-box 重新构建了(${SINGBOX_REBUILD_REASON}),但已经装在节点上的仍是之前那一份。
  在每台机器的「入口」Tab 里,sing-box 卡片「重启」旁的下拉 →「重新安装」,
  再下发一次配置(会断开那台机器上全部入口的在线连接)。
  卡片上写着「可升级」的就是还没换的。
EOF
fi
