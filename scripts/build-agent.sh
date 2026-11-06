#!/usr/bin/env bash
# 构建可选的节点 Agent litebox-agent(VNext AGENT01),供面板在管理员显式启用时下发到节点。
#
# 它是本仓库 cmd/litebox-agent 的静态构建(CGO_ENABLED=0,只依赖标准库与 internal/agentproto),
# 不依赖节点上的 libc,Alpine 与 Debian 都直接能跑。每次都重新构建:面板按 sha256 认出文件换过了。
# 默认不会装到任何节点上 —— 只有在节点详情里点「启用 Agent」,且那台机器内存不少于 256 MB 才装。
set -euo pipefail

OUTPUT_DIR="${OUTPUT_DIR:-assets/agent}"
VERSION="${VERSION:-dev}"
mkdir -p "$OUTPUT_DIR"

echo "构建 litebox-agent"
for arch in amd64 arm64; do
    out="$OUTPUT_DIR/litebox-agent-linux-${arch}"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o "${out}.tmp" ./cmd/litebox-agent
    mv -f "${out}.tmp" "$out"
    chmod 0755 "$out"
    echo "  ${out}($(wc -c < "$out") 字节)"
done
