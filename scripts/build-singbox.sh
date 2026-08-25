#!/usr/bin/env bash
# 构建带 with_v2ray_api 的 sing-box,产物供主控分发到节点。
#
# 不修改上游源码,只调整构建标签。构建标签使用精简集合而非上游默认标签:
# Phase 0 实测精简构建二进制 27.8MB / cgroup 实占 10MB,
# 完整默认标签 58.1MB / 27MB,对 128MB 节点差异明显。
set -euo pipefail

# 两支构建:正式版与预览版(V14)。
#
#   SINGBOX_CHANNEL=stable   （默认）产出 sing-box-linux-<arch>
#   SINGBOX_CHANNEL=preview           产出 sing-box-preview-linux-<arch>
#
# 预览版存在的唯一理由是 **Snell**:那个入站要 sing-box 1.14,而 1.14
# 目前只有预览版。面板按节点选装哪一支(nodes.singbox_channel),
# 一台机器一支 —— 一个 sing-box 进程、一份 config.json、一个 API 端口,
# 装第二个只会覆盖第一个。
#
# 两支的**构建标签完全相同**,这一点是刻意的:预览版不是"另一个东西",
# 它是同一份配置的另一个版本。真机实测(V14 技术验证 §2):由正式版
# 渲染出来的配置在预览版上 check 通过、真跑起来、零 deprecation 警告。
# 所以切通道不需要改配置,也就不会触发一次全站重新部署。
#
# 代价要说出来:同一份配置下常驻内存实测 30.4MB(正式版 22.4MB),
# 对 128MB 的机器是 +8MB。
#
# 固定版本,禁止使用 latest。升级前需重新执行 Phase 0 的验证脚本。
SINGBOX_CHANNEL="${SINGBOX_CHANNEL:-stable}"
case "$SINGBOX_CHANNEL" in
    stable)
        SINGBOX_VERSION="${SINGBOX_VERSION:-v1.13.15}"
        OUTPUT_NAME="sing-box"
        ;;
    preview)
        SINGBOX_VERSION="${SINGBOX_VERSION:-v1.14.0-rc.1}"
        OUTPUT_NAME="sing-box-preview"
        ;;
    *)
        echo "SINGBOX_CHANNEL 只能是 stable 或 preview,实际是 $SINGBOX_CHANNEL" >&2
        exit 1
        ;;
esac
OUTPUT_DIR="${OUTPUT_DIR:-assets/singbox}"
# 两支各自的工作目录:共用一个的话,来回切通道会反复 checkout 两个 tag,
# 每次都要重新下载依赖。
WORK_DIR="${WORK_DIR:-.build/sing-box-$SINGBOX_CHANNEL}"

# with_utls    —— REALITY 服务端实现已并入该标签(with_reality_server 在 1.13 已移除)
# with_v2ray_api —— 用户级流量统计,缺少它整套统计无从谈起
# badlinkname,tfogo_checklinkname0 —— 上游默认标签的一部分,与 -checklinkname=0 配套
BUILD_TAGS="with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0"

VERSION_STRING="${SINGBOX_VERSION}-litebox"

echo "构建 sing-box ${SINGBOX_VERSION}(${SINGBOX_CHANNEL})"
echo "构建标签: ${BUILD_TAGS}"
echo

if [ ! -d "$WORK_DIR/.git" ]; then
    mkdir -p "$(dirname "$WORK_DIR")"
    git clone --depth 1 --branch "$SINGBOX_VERSION" \
        https://github.com/SagerNet/sing-box "$WORK_DIR"
else
    git -C "$WORK_DIR" fetch --depth 1 origin tag "$SINGBOX_VERSION"
    git -C "$WORK_DIR" checkout -q "$SINGBOX_VERSION"
fi

# 上游 LDFLAGS 必须原样带上,-checklinkname=0 与 badlinkname 标签配套,缺失会构建失败。
UPSTREAM_LDFLAGS="$(cat "$WORK_DIR/release/LDFLAGS")"
LDFLAGS="-X 'github.com/sagernet/sing-box/constant.Version=${VERSION_STRING}' ${UPSTREAM_LDFLAGS} -s -w -buildid="

mkdir -p "$OUTPUT_DIR"

for arch in amd64 arm64; do
    out="$OUTPUT_DIR/$OUTPUT_NAME-linux-$arch"
    echo "构建 linux/$arch ..."
    (
        cd "$WORK_DIR"
        CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOTOOLCHAIN=local \
            go build -trimpath -tags "$BUILD_TAGS" -ldflags "$LDFLAGS" \
            -o "$OLDPWD/$out" ./cmd/sing-box
    )
    sha256sum "$out" | tee "$out.sha256"
done

# 用本机可执行的那份验证构建标签确实生效。
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
    x86_64) HOST_ARCH=amd64 ;;
    aarch64) HOST_ARCH=arm64 ;;
esac
if [ "$(uname -s)" = "Linux" ] && [ -x "$OUTPUT_DIR/$OUTPUT_NAME-linux-$HOST_ARCH" ]; then
    echo
    echo "验证构建标签:"
    tags_line="$("$OUTPUT_DIR/$OUTPUT_NAME-linux-$HOST_ARCH" version | grep '^Tags:')"
    echo "  $tags_line"
    if ! echo "$tags_line" | grep -q 'with_v2ray_api'; then
        echo "错误:构建标签中缺少 with_v2ray_api" >&2
        exit 1
    fi
fi

# 两支各写各的元数据文件:共用一个的话,后构建的那一支会把前一支的
# 版本号盖掉,而那个文件正是排查"节点上跑的到底是哪一份"的起点。
cat > "$OUTPUT_DIR/build-metadata-$SINGBOX_CHANNEL.json" <<EOF
{
  "channel": "${SINGBOX_CHANNEL}",
  "singbox_version": "${SINGBOX_VERSION}",
  "version_string": "${VERSION_STRING}",
  "build_tags": "${BUILD_TAGS}",
  "go_version": "$(go version | awk '{print $3}')",
  "revision": "$(git -C "$WORK_DIR" rev-parse HEAD)",
  "built_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

echo
echo "完成,产物在 $OUTPUT_DIR:"
ls -la "$OUTPUT_DIR"
