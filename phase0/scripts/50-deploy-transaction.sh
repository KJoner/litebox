#!/bin/bash
# Phase 0 步骤 5:部署事务原型 —— 新增用户、删除用户、无效配置自动回滚。
#
# 实现开发计划第 6 节的事务流程,并根据 Phase 0 发现补强健康检查:
#   sing-box check 不校验 flow 与 UUID 语义(见 20-setup 结果),
#   因此"systemd active + 端口监听"会把一个所有用户都连不上的节点判为健康。
#   健康检查必须包含一次真实的 VLESS 拨测。
set -uo pipefail
BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
PROBE=$BASE/bin/statsprobe-linux-amd64
DB=$BASE/phase0.db
API=127.0.0.1:28080
LIVE=$BASE/conf/server.json
SVC=litebox-phase0

start_client() {
    nohup "$SB" run -c "$BASE/conf/client$1.json" > "$BASE/logs/client$1.log" 2>&1 &
    echo $! > "$BASE/logs/client$1.pid"
}
stop_client() {
    [ -f "$BASE/logs/client$1.pid" ] && kill "$(cat "$BASE/logs/client$1.pid")" 2>/dev/null
    rm -f "$BASE/logs/client$1.pid"
}
stop_all() { for i in 1 2 3; do stop_client $i; done; }
trap stop_all EXIT

# 真实拨测:通过指定客户端配置发起一次 VLESS 请求,返回 0 表示可用。
probe_user() {  # $1=客户端编号 $2=端口
    stop_client "$1"
    start_client "$1"
    sleep 2
    local code
    code=$(curl -s -o /dev/null --max-time 12 --socks5-hostname "127.0.0.1:$2" \
        -w '%{http_code}' "https://www.cloudflare.com/cdn-cgi/trace" 2>/dev/null)
    stop_client "$1"
    [ "$code" = "200" ]
}

report_user() {  # $1=客户端编号 $2=端口 $3=期望 up|down
    if probe_user "$1" "$2"; then actual=up; else actual=down; fi
    if [ "$actual" = "$3" ]; then
        echo "    user_00000$1: $actual (符合预期)"
    else
        echo "    user_00000$1: $actual (预期 $3) —— 不符"
    fi
}

# ---- 部署事务 ----
deploy() {  # $1=新配置文件 $2=描述
    local newconf=$1 desc=$2
    echo "--- 部署: $desc ---"
    local rev backup
    rev=$(date +%s)
    backup=$BASE/backup/server-$rev.json

    echo "  [1/8] 重启前强制同步流量"
    if ! $PROBE sync --api "$API" --db "$DB" --node node-la > /dev/null 2>&1; then
        echo "        同步失败 —— 中止部署(避免丢失流量)"
        return 1
    fi

    echo "  [2/8] 上传临时配置"
    cp "$newconf" "$LIVE.tmp"

    echo "  [3/8] sing-box check"
    if ! $SB check -c "$LIVE.tmp" 2>"$BASE/logs/check.err"; then
        echo "        校验失败,不重启。错误:"
        sed 's/^/          /' "$BASE/logs/check.err" | head -3
        rm -f "$LIVE.tmp"
        return 2
    fi

    echo "  [4/8] 备份当前配置 -> $(basename "$backup")"
    cp "$LIVE" "$backup"

    echo "  [5/8] 原子替换"
    mv "$LIVE.tmp" "$LIVE"

    echo "  [6/8] 重启服务"
    systemctl restart "$SVC"
    sleep 4

    echo "  [7/8] 健康检查:systemd 状态 + 端口"
    if ! systemctl is-active --quiet "$SVC" || ! ss -tln | grep -q '127.0.0.1:24443'; then
        echo "        基础健康检查失败"
        rollback "$backup"
        return 3
    fi

    echo "  [8/8] 健康检查:真实 VLESS 拨测"
    if ! probe_user 1 21081; then
        echo "        拨测失败 —— 配置可加载但用户无法连接"
        rollback "$backup"
        return 4
    fi
    echo "  部署成功 (revision=$rev)"
    return 0
}

rollback() {
    echo "  >>> 回滚到 $(basename "$1")"
    cp "$1" "$LIVE"
    systemctl restart "$SVC"
    sleep 4
    if systemctl is-active --quiet "$SVC" && probe_user 1 21081; then
        echo "  >>> 回滚成功,节点已恢复服务"
    else
        echo "  >>> 回滚后仍不可用 —— 标记 DEPLOY_FAILED"
    fi
}

echo "================ 基线:rev1(user1 + user2)================"
cp "$BASE/conf/server-rev1.json" "$LIVE"
systemctl restart "$SVC"; sleep 4
report_user 1 21081 up
report_user 2 21082 up
report_user 3 21083 down
echo

echo "================ 场景1:新增 user_000003(rev2)================"
deploy "$BASE/conf/server-rev2.json" "新增 user_000003"
echo "  变更后连通性:"
report_user 1 21081 up
report_user 2 21082 up
report_user 3 21083 up
echo

echo "================ 场景2:删除 user_000002/3(rev3)================"
deploy "$BASE/conf/server-rev3.json" "仅保留 user_000001"
echo "  变更后连通性:"
report_user 1 21081 up
report_user 2 21082 down
report_user 3 21083 down
echo

echo "================ 场景3:无效配置(REALITY 私钥非法)================"
deploy "$BASE/conf/server-bad.json" "故意损坏的配置"
rc=$?
echo "  部署返回码=$rc (2=check 拦截,未重启)"
echo "  服务状态: $(systemctl is-active $SVC)"
echo "  当前配置是否仍为 rev3: $(cmp -s "$LIVE" "$BASE/conf/server-rev3.json" && echo 是 || echo 否)"
report_user 1 21081 up
echo

echo "================ 场景4:通过 check 但用户不可用(flow 非法)================"
deploy "$BASE/conf/server-bad-flow.json" "flow=xtls-rprx-direct"
rc=$?
echo "  部署返回码=$rc (4=拨测失败并回滚)"
echo "  服务状态: $(systemctl is-active $SVC)"
report_user 1 21081 up
echo

echo "================ 备份保留情况 ================"
ls -1t "$BASE/backup" | head -6 | sed 's/^/    /'
echo "    共 $(ls -1 "$BASE/backup" | wc -l) 个备份"
