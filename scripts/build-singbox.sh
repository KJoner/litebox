#!/usr/bin/env bash
# 构建带 with_v2ray_api 的 sing-box,产物供主控分发到节点。
#
# 不修改上游源码,只调整构建标签。构建标签使用精简集合而非上游默认标签:
# Phase 0 实测精简构建二进制 27.8MB / cgroup 实占 10MB,
# 完整默认标签 58.1MB / 27MB,对 128MB 节点差异明显。
set -euo pipefail

# 固定版本,禁止使用 latest。升级前需重新执行 Phase 0 的验证脚本。
SINGBOX_VERSION="${SINGBOX_VERSION:-v1.13.15}"
OUTPUT_DIR="${OUTPUT_DIR:-assets/singbox}"
WORK_DIR="${WORK_DIR:-.build/sing-box}"

# with_utls    —— REALITY 服务端实现已并入该标签(with_reality_server 在 1.13 已移除)
# with_v2ray_api —— 用户级流量统计,缺少它整套统计无从谈起
# badlinkname,tfogo_checklinkname0 —— 上游默认标签的一部分,与 -checklinkname=0 配套
BUILD_TAGS="with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0"

VERSION_STRING="${SINGBOX_VERSION}-litebox"

echo "构建 sing-box ${SINGBOX_VERSION}"
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
    out="$OUTPUT_DIR/sing-box-linux-$arch"
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
if [ "$(uname -s)" = "Linux" ] && [ -x "$OUTPUT_DIR/sing-box-linux-$HOST_ARCH" ]; then
    echo
    echo "验证构建标签:"
    tags_line="$("$OUTPUT_DIR/sing-box-linux-$HOST_ARCH" version | grep '^Tags:')"
    echo "  $tags_line"
    if ! echo "$tags_line" | grep -q 'with_v2ray_api'; then
        echo "错误:构建标签中缺少 with_v2ray_api" >&2
        exit 1
    fi
fi

cat > "$OUTPUT_DIR/build-metadata.json" <<EOF
{
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
