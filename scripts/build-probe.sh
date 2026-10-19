#!/usr/bin/env bash
# 构建出站网络探测的非驻留辅助程序 litebox-probe,供面板下发到自建节点。
#
# 它是本仓库 cmd/litebox-probe 的静态构建(CGO_ENABLED=0,约 6MB:v3 起带 HTTP(S) 检测,
# 编进了 net/http 与 crypto/tls),不依赖节点上的 libc,
# Alpine 与 Debian 都直接能跑。每次都重新构建而不是「有了就跳过」:辅助程序与面板之间有
# 协议版本号(netprobe.Version),面板升级之后还分发旧的那一份,节点会一直报「版本不符」。
# 面板按 sha256 认出文件换过了,下一轮探测时自动重传到各节点。
set -euo pipefail

OUTPUT_DIR="${OUTPUT_DIR:-assets/probe}"
mkdir -p "$OUTPUT_DIR"

echo "构建 litebox-probe"
for arch in amd64 arm64; do
    out="$OUTPUT_DIR/litebox-probe-linux-${arch}"
    # 先写临时文件再改名:面板可能正在读这个文件给节点下发。
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w" -o "${out}.tmp" ./cmd/litebox-probe
    mv -f "${out}.tmp" "$out"
    chmod 0755 "$out"
    echo "  ${out}($(wc -c < "$out") 字节)"
done
