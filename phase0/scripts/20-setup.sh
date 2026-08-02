#!/bin/bash
# Phase 0 步骤 2:在节点上生成密钥、配置与 systemd 单元。
# 全部资源隔离在 /opt/litebox-phase0,systemd 单元名为 litebox-phase0.service,
# 不触碰机器上已有的 /usr/local/bin/sing-box 与其服务。
set -euo pipefail

BASE=/opt/litebox-phase0
SB=$BASE/bin/sing-box-linux-amd64-min
DEST=www.apple.com

cd "$BASE"

# --- 生成密钥材料 ---
KEYPAIR=$($SB generate reality-keypair)
PRIVKEY=$(echo "$KEYPAIR" | grep PrivateKey | awk '{print $2}')
PUBKEY=$(echo "$KEYPAIR" | grep PublicKey | awk '{print $2}')
SHORTID=$($SB generate rand 8 --hex)
UUID1=$($SB generate uuid)
UUID2=$($SB generate uuid)
UUID3=$($SB generate uuid)

cat > "$BASE/conf/params.env" <<EOF
PRIVKEY=$PRIVKEY
PUBKEY=$PUBKEY
SHORTID=$SHORTID
UUID1=$UUID1
UUID2=$UUID2
UUID3=$UUID3
DEST=$DEST
EOF

echo "生成的参数:"
cat "$BASE/conf/params.env"
echo

# --- 服务端配置(2 个用户,revision 1) ---
write_server_config() {
    local outfile=$1
    shift
    local users_json=$1
    cat > "$outfile" <<EOF
{
  "log": { "level": "info", "timestamp": true },
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-in",
      "listen": "127.0.0.1",
      "listen_port": 24443,
      "users": $users_json,
      "tls": {
        "enabled": true,
        "server_name": "$DEST",
        "reality": {
          "enabled": true,
          "handshake": { "server": "$DEST", "server_port": 443 },
          "private_key": "$PRIVKEY",
          "short_id": ["$SHORTID"]
        }
      }
    }
  ],
  "outbounds": [ { "type": "direct", "tag": "direct" } ],
  "experimental": {
    "v2ray_api": {
      "listen": "127.0.0.1:28080",
      "stats": {
        "enabled": true,
        "inbounds": ["vless-in"],
        "users": ["user_000001", "user_000002", "user_000003"]
      }
    }
  }
}
EOF
}

USERS2='[
        { "name": "user_000001", "uuid": "'$UUID1'", "flow": "xtls-rprx-vision" },
        { "name": "user_000002", "uuid": "'$UUID2'", "flow": "xtls-rprx-vision" }
      ]'
USERS3='[
        { "name": "user_000001", "uuid": "'$UUID1'", "flow": "xtls-rprx-vision" },
        { "name": "user_000002", "uuid": "'$UUID2'", "flow": "xtls-rprx-vision" },
        { "name": "user_000003", "uuid": "'$UUID3'", "flow": "xtls-rprx-vision" }
      ]'
USERS1='[
        { "name": "user_000001", "uuid": "'$UUID1'", "flow": "xtls-rprx-vision" }
      ]'

write_server_config "$BASE/conf/server-rev1.json" "$USERS2"   # 2 用户
write_server_config "$BASE/conf/server-rev2.json" "$USERS3"   # 新增 user_000003
write_server_config "$BASE/conf/server-rev3.json" "$USERS1"   # 删除 user_000002/3

# 故意损坏的配置,用于回滚测试。
# 注意:非法 UUID 不能作为损坏样本 —— sing-vmess 在 uuid.FromString 失败时
# 会回退到 uuid.NewV5(uuid.Nil, s) 对字符串做哈希,因此任意字符串都被接受。
sed 's/"'$UUID2'"/"not-a-valid-uuid"/' "$BASE/conf/server-bad-uuid.json.sample" 2>/dev/null || \
sed 's/"'$UUID2'"/"not-a-valid-uuid"/' "$BASE/conf/server-rev1.json" > "$BASE/conf/server-bad-uuid.json"
# 真正会被 check 拒绝的样本:REALITY 私钥长度非法
sed 's/"private_key": "'$PRIVKEY'"/"private_key": "dG9vLXNob3J0"/' "$BASE/conf/server-rev1.json" > "$BASE/conf/server-bad.json"
# 非法 flow 值
sed 's/"flow": "xtls-rprx-vision"/"flow": "xtls-rprx-direct"/' "$BASE/conf/server-rev1.json" > "$BASE/conf/server-bad-flow.json"

cp "$BASE/conf/server-rev1.json" "$BASE/conf/server.json"

# --- 客户端配置 ---
write_client_config() {
    cat > "$BASE/conf/client$1.json" <<EOF
{
  "log": { "level": "warn", "timestamp": true },
  "inbounds": [
    { "type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": $2 }
  ],
  "outbounds": [
    {
      "type": "vless",
      "tag": "proxy",
      "server": "127.0.0.1",
      "server_port": 24443,
      "uuid": "$3",
      "flow": "xtls-rprx-vision",
      "tls": {
        "enabled": true,
        "server_name": "$DEST",
        "utls": { "enabled": true, "fingerprint": "chrome" },
        "reality": { "enabled": true, "public_key": "$PUBKEY", "short_id": "$SHORTID" }
      }
    }
  ]
}
EOF
}
write_client_config 1 21081 "$UUID1"
write_client_config 2 21082 "$UUID2"
write_client_config 3 21083 "$UUID3"

# --- 配置校验 ---
echo "sing-box check 结果:"
for f in server-rev1 server-rev2 server-rev3 client1 client2 client3; do
    if $SB check -c "$BASE/conf/$f.json" 2>/dev/null; then
        echo "  $f.json  通过"
    else
        echo "  $f.json  失败"
        exit 1
    fi
done
if $SB check -c "$BASE/conf/server-bad.json" 2>/dev/null; then
    echo "  server-bad.json  意外通过(测试用例失效)"
    exit 1
else
    echo "  server-bad.json  按预期被拒绝(REALITY 私钥非法)"
fi
# 以下两项是 check 的已知盲区,记录而不作为失败条件:
for f in server-bad-uuid server-bad-flow; do
    if $SB check -c "$BASE/conf/$f.json" 2>/dev/null; then
        echo "  $f.json  通过 —— check 盲区,需由面板自行校验"
    else
        echo "  $f.json  被拒绝(与预期不符,需复核)"
    fi
done
echo

# --- systemd 单元 ---
cat > /etc/systemd/system/litebox-phase0.service <<EOF
[Unit]
Description=LiteBox Phase 0 sing-box test node
After=network.target

[Service]
Type=simple
ExecStart=$SB run -c $BASE/conf/server.json
Restart=on-failure
RestartSec=2
LimitNOFILE=infinity
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$BASE
AmbientCapabilities=
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl restart litebox-phase0
sleep 3
systemctl is-active litebox-phase0 && echo "litebox-phase0 已启动"
ss -tlnp | grep -E '24443|28080' || true
