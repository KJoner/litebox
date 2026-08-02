#!/bin/bash
# Phase 0 步骤 1:从节点出口检测 REALITY 握手目标是否满足要求。
cd /opt/litebox-phase0 || exit 1
for h in www.apple.com www.cloudflare.com dl.google.com addons.mozilla.org gateway.icloud.com www.microsoft.com www.bing.com; do
    printf '%-24s ' "$h"
    ./bin/statsprobe-linux-amd64 destcheck --host "$h" 2>&1 | grep -E '最大记录长度|结论' | tr '\n' ' '
    echo
done
