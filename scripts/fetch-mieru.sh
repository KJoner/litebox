#!/usr/bin/env bash
# 拉取 mieru / mita 的官方二进制,供面板下发到节点。
#
# 与 sing-box 不同:**我们不自己构建**。sing-box 要调构建标签
# (with_v2ray_api 是整套流量统计的前提,精简标签是 128MB 机器的前提),
# 而 mita 没有任何需要调整的编译选项 —— 自己构建只会多一份要维护的
# 构建环境,换不来任何东西。
#
# 版本固定,禁止使用 latest。升级前要重跑 docs/开发计划/v13 里的验证清单:
# 这一版的实现依赖三条实测到的语义(apply 是整体替换、reload 不释放旧端口、
# metrics.pb 路径硬编码),而它们都可能随版本变。
set -euo pipefail

MIERU_VERSION="${MIERU_VERSION:-v3.36.0}"
OUTPUT_DIR="${OUTPUT_DIR:-assets/mieru}"

VER="${MIERU_VERSION#v}"
BASE="https://github.com/enfein/mieru/releases/download/${MIERU_VERSION}"

mkdir -p "$OUTPUT_DIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "拉取 mieru/mita ${MIERU_VERSION}"
for arch in amd64 arm64; do
    for prog in mita mieru; do
        out="$OUTPUT_DIR/${prog}-linux-${arch}"
        # 用 -s(存在且非空)而不是 -x:在 Windows 的 Git Bash 上
        # 可执行位不可靠,而这个脚本也会在开发机上跑。
        if [ -s "$out" ]; then
            echo "  ${prog}-${arch} 已存在,跳过"
            continue
        fi
        tarball="${prog}_${VER}_linux_${arch}.tar.gz"
        echo "  下载 $tarball"
        curl -fsSL -o "$WORK/$tarball" "$BASE/$tarball"
        # 官方同时发布 .sha256.txt,校验它 —— 这两个二进制会以 root 身份
        # 跑在每一台节点上,而下载是唯一一次能发现内容不对的机会。
        curl -fsSL -o "$WORK/$tarball.sha256" "$BASE/$tarball.sha256.txt"
        ( cd "$WORK" && sha256sum -c "$tarball.sha256" >/dev/null ) \
            || { echo "  !! $tarball 校验失败" >&2; exit 1; }
        tar -xzf "$WORK/$tarball" -C "$WORK"
        mv "$WORK/$prog" "$out"
        chmod +x "$out"
    done
done

echo
echo "已就绪:"
ls -la "$OUTPUT_DIR"
