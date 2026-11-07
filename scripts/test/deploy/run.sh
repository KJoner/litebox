#!/usr/bin/env bash
# 部署脚本的故障注入回归(R03 / C03)。会起停容器、建卷,**只在开发机或专用测试机上跑**。
#
#   bash scripts/test/deploy/run.sh [sqlite|postgres|external-postgres|native|all]
#
# 做什么:
#   1. 升级源 = 已发布的 kjoner/litebox:v23.0.0-rc.1(本机没有就拉取);
#   2. 升级目标 = 在 rc.1 镜像上换成当前工作区交叉编译的主控与部署文件(v23.1.0-test),
#      外加三个坏版本:broken-migrate(迁移失败)、broken-up(Compose 文件坏了,启动命令失败)、
#      broken-health(进程起来但永远不健康);
#   3. faults.sh 在一个 docker:cli 容器里跑 deploy/docker/litebox.sh,数据目录放在一个卷的**宿主真实路径**上
#      (/var/lib/docker/volumes/<卷>/_data 同时挂进客户端容器)—— 这样 Docker Desktop 上也拿到真实的
#      Linux 文件权限语义,快捷 PG 读不到口令文件这种问题测得出来;
#   4. native-upgrade.sh 在 alpine 容器里用 systemctl / sudo 替身跑 scripts/upgrade.sh。
#
# 每一种模式:装 rc.1 → 建用户 → 三种故障注入各升级一次(每次都检查版本回到 rc.1、健康、能登录、用户还在)
# → 正常升级 → 备份并真实验证 → 建第二个用户后恢复(第二个用户消失、自动下发暂停、解除)→ 损坏的备份在停主控之前被拒绝。
set -euo pipefail
cd "$(dirname "$0")/../../.."
WHAT="${1:-all}"
OLD=v23.0.0-rc.1
VOL=lbdeploytest
ROOT="/var/lib/docker/volumes/$VOL/_data"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

docker image inspect "kjoner/litebox:$OLD" >/dev/null 2>&1 || docker pull "kjoner/litebox:$OLD"

echo "==> 交叉编译当前工作区的主控(需要先构建过 web/dist)"
mkdir -p "$work/new/deploy" "$work/bh" "$work/bu" "$work/bm"
GOOS=linux GOARCH="$(docker version -f '{{.Server.Arch}}')" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.Version=v23.1.0-test" -o "$work/new/litebox" ./cmd/litebox
cp -r deploy/docker/. "$work/new/deploy/"
printf 'FROM kjoner/litebox:%s\nCOPY litebox /usr/local/bin/litebox\nCOPY deploy/ /opt/litebox/deploy/\n' "$OLD" > "$work/new/Dockerfile"
docker build -q -t kjoner/litebox:v23.1.0-test "$work/new" >/dev/null
printf 'FROM kjoner/litebox:v23.1.0-test\nENV LITEBOX_HTTP_LISTEN=256.0.0.1:8080\n' > "$work/bh/Dockerfile"
docker build -q -t kjoner/litebox:v23.1.0-broken-health "$work/bh" >/dev/null
printf 'services:\n  litebox: [this is not valid\n' > "$work/bu/compose.yaml"
printf 'FROM kjoner/litebox:v23.1.0-test\nCOPY compose.yaml /opt/litebox/deploy/compose.yaml\n' > "$work/bu/Dockerfile"
docker build -q -t kjoner/litebox:v23.1.0-broken-up "$work/bu" >/dev/null
cp scripts/test/deploy/broken-migrate-entry.sh "$work/bm/entry"
printf 'FROM kjoner/litebox:v23.1.0-test\nCOPY --chmod=0755 entry /usr/local/bin/entry\nENTRYPOINT ["/usr/local/bin/entry"]\nCMD ["serve", "--config", "/data/litebox.yaml"]\n' > "$work/bm/Dockerfile"
docker build -q -t kjoner/litebox:v23.1.0-broken-migrate "$work/bm" >/dev/null

docker volume create "$VOL" >/dev/null
# 把脚本放进卷:客户端容器里的路径与宿主上的路径一致。
docker run --rm -v "$PWD/deploy/docker:/src:ro" -v "$PWD/scripts/test/deploy:/t:ro" -v "$VOL:$ROOT" alpine:3.22 \
    sh -c "cp /src/litebox.sh $ROOT/litebox.sh && sed 's#lbr03#$VOL#' /t/faults.sh > $ROOT/r03test.sh"

rc=0
for m in sqlite postgres external-postgres; do
    [ "$WHAT" = all ] || [ "$WHAT" = "$m" ] || continue
    docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$VOL:$ROOT" docker:27-cli \
        sh -c "apk add -q --no-cache bash coreutils curl tar >/dev/null && bash $ROOT/r03test.sh $m" || rc=1
done
if [ "$WHAT" = all ] || [ "$WHAT" = native ]; then
    mkdir -p "$work/native"
    cid="$(docker create "kjoner/litebox:$OLD")"; docker cp "$cid:/usr/local/bin/litebox" "$work/native/old-litebox" >/dev/null; docker rm "$cid" >/dev/null
    cp "$work/new/litebox" "$work/native/new-litebox"
    cp scripts/upgrade.sh scripts/test/deploy/native-upgrade.sh "$work/native/"
    printf '#!/bin/sh\nfor a in "$@"; do if [ "$a" = migrate ]; then /t/new-litebox "$@"; echo "注入:迁移失败" >&2; exit 1; fi; done\nexec /t/new-litebox "$@"\n' > "$work/native/broken-litebox"
    docker run --rm -v "$work/native:/t0:ro" alpine:3.22 \
        sh -c 'apk add -q --no-cache bash >/dev/null && mkdir /t && cp /t0/* /t/ && chmod +x /t/* && bash /t/native-upgrade.sh' || rc=1
fi
exit $rc
