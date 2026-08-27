#!/usr/bin/env bash
# 拉取 realm 的官方二进制,供面板下发到中转主机。
#
# 与 mieru 一样**不自己构建**:realm 没有任何需要调整的编译选项。
# 取 musl 静态构建 —— 它不依赖节点上的 glibc,Alpine(NAT 小鸡最常见的镜像)
# 与 Debian 都直接能跑。
#
# 版本固定,禁止使用 latest。上游没有随 release 发布校验文件,
# 所以 SHA-256 写死在这里:这个二进制会以 root 身份跑在每一台中转主机上,
# 而下载是唯一一次能发现内容不对的机会。升级版本时先在验证机上跑完
# docs/开发计划/v15 里的 realm 那一节,再改下面两行。
set -euo pipefail

REALM_VERSION="${REALM_VERSION:-v2.9.4}"
OUTPUT_DIR="${OUTPUT_DIR:-assets/realm}"
BASE="https://github.com/zhboner/realm/releases/download/${REALM_VERSION}"

# 与 REALM_VERSION 对应;换版本必须一起换。
declare -A SHA256=(
    [amd64]="a19b86c4ae4642d5864821b41d23633c0c91df279a88496c05834dc584169175"
    [arm64]="0195e77ca99713166e25ff85fefe042049c79fdaddf500e8ffd9ba77494a029c"
)
declare -A TRIPLE=(
    [amd64]="x86_64-unknown-linux-musl"
    [arm64]="aarch64-unknown-linux-musl"
)

mkdir -p "$OUTPUT_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "拉取 realm ${REALM_VERSION}"
for arch in amd64 arm64; do
    out="$OUTPUT_DIR/realm-linux-${arch}"
    # 用 -s(存在且非空)而不是 -x:Windows 的 Git Bash 上可执行位不可靠。
    if [ -s "$out" ]; then
        echo "  realm-${arch} 已存在,跳过"
        continue
    fi
    tarball="realm-${TRIPLE[$arch]}.tar.gz"
    echo "  下载 $tarball"
    curl -fsSL -o "$WORK/$tarball" "$BASE/$tarball"
    echo "${SHA256[$arch]}  $WORK/$tarball" | sha256sum -c - >/dev/null \
        || { echo "  !! $tarball 校验失败" >&2; exit 1; }
    mkdir -p "$WORK/$arch"
    tar -xzf "$WORK/$tarball" -C "$WORK/$arch"
    mv "$WORK/$arch/realm" "$out"
    chmod +x "$out"
done

echo
echo "已就绪:"
ls -la "$OUTPUT_DIR"
