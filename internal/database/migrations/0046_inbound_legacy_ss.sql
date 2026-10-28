-- 自建 Shadowsocks 入站收传统 AEAD(V22.8)。
--
-- ss_method 从只收 2022 系列三种,放宽到再加 aes-128-gcm / aes-256-gcm /
-- chacha20-ietf-poly1305。理由只有一条:SS2022 的 AEAD 头带时间戳,节点时钟
-- 偏差超过约 30 秒时全部用户连不上,而不少小鸡(尤其 LXC / OpenVZ 容器)里根本
-- 校不了时;传统 AEAD 与时钟无关。代价(没有 replay 防护、多用户逐个试解密)
-- 写在 singbox.SSMethod 的注释与入口表单上。
--
-- 用户凭据不新增:传统 AEAD 的客户端 password 就是用户已有的那把 SS 密钥
-- (32 字节标准 base64)原样,见 singbox.SSClientPassword。
--
-- ---------- 为什么又要重建整张表 ----------
--
-- 与迁移 0030 同一件事:那一列上的 CHECK 只能靠重建来改。做法逐字照搬 0030 ——
-- 先拆三个视图(RENAME 会解析库里全部视图,指向不存在的表的那个会让整条迁移失败),
-- 建新表、逐列搬数据、改名、重建索引与视图。
--
-- 行要**全量搬过来**:丢一行就是那个入口从所有人的订阅里消失,而节点上它还在跑。
-- 列也一个都不能少 —— 0030 之后 0031 / 0032 又加了四列(snell_shared_psk、
-- unmetered 与各自的 deployed_ 镜像),漏搬其中一列,那个入口的凭据模式或计量方式
-- 会在升级那一刻悄悄回到默认值。database/ss_legacy_upgrade_test.go 逐列比对升级前后。
--
-- 存量行的取值一个不变,所以升级之后渲染结果逐字节相同,不会有任何一台机器
-- 因此被判成「待部署」。

DROP VIEW user_effective_relays;
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;

CREATE TABLE node_inbounds_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 以下各列的含义与迁移 0019 / 0030 / 0031 / 0032 完全一致,注释不再重复。
    tag          TEXT NOT NULL,
    display_name TEXT NOT NULL,

    protocol TEXT NOT NULL DEFAULT 'VLESS_REALITY'
        CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS', 'SNELL')),

    listen_port      INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port      INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),
    ipv6_public_port INTEGER NOT NULL DEFAULT 0
        CHECK (ipv6_public_port = 0 OR ipv6_public_port BETWEEN 1 AND 65535),

    tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    reality_dest              TEXT    NOT NULL DEFAULT '',
    reality_dest_port         INTEGER NOT NULL DEFAULT 443,
    reality_privkey_encrypted TEXT    NOT NULL DEFAULT '',
    reality_pubkey            TEXT    NOT NULL DEFAULT '',
    reality_short_id          TEXT    NOT NULL DEFAULT '',
    handshake_max_record_size INTEGER NOT NULL DEFAULT 0,
    handshake_checked_at      TEXT,

    -- 这次重建唯一的变化。取值必须与 singbox.ParseSSMethod 收的那六种一字不差:
    -- 少一种是"保存入口时报一句约束失败",多一种是绕过 Go 侧校验写进库的方法
    -- 渲染不出来,整台机器的配置状态变成「未知」。
    ss_method TEXT NOT NULL DEFAULT ''
        CHECK (ss_method IN ('', '2022-blake3-aes-128-gcm',
                             '2022-blake3-aes-256-gcm',
                             '2022-blake3-chacha20-poly1305',
                             'aes-128-gcm', 'aes-256-gcm',
                             'chacha20-ietf-poly1305')),
    ss_password_encrypted TEXT NOT NULL DEFAULT '',

    snell_version INTEGER NOT NULL DEFAULT 0
        CHECK (snell_version IN (0, 5, 6)),
    snell_psk_encrypted TEXT NOT NULL DEFAULT '',
    snell_obfs_mode TEXT NOT NULL DEFAULT ''
        CHECK (snell_obfs_mode IN ('', 'none', 'http', 'tls')),
    snell_obfs_host TEXT NOT NULL DEFAULT '',
    snell_v6_mode TEXT NOT NULL DEFAULT ''
        CHECK (snell_v6_mode IN ('', 'default', 'unshaped', 'unsafe-raw')),

    chain_target_kind TEXT NOT NULL DEFAULT ''
        CHECK (chain_target_kind IN ('', 'INBOUND', 'EXTERNAL')),
    chain_target_inbound_id  INTEGER,
    chain_target_external_id INTEGER,

    chain_code                  TEXT NOT NULL DEFAULT '',
    chain_uuid_encrypted        TEXT NOT NULL DEFAULT '',
    chain_ss_password_encrypted TEXT NOT NULL DEFAULT '',

    access_tier_id INTEGER NOT NULL DEFAULT 1,

    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,

    deployed_protocol      TEXT    NOT NULL DEFAULT '',
    deployed_ss_method     TEXT    NOT NULL DEFAULT '',
    deployed_tcp_fast_open INTEGER NOT NULL DEFAULT 0,

    deployed_snell_version   INTEGER NOT NULL DEFAULT 0,
    deployed_snell_obfs_mode TEXT    NOT NULL DEFAULT '',
    deployed_snell_v6_mode   TEXT    NOT NULL DEFAULT '',

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    ipv6_enabled      INTEGER NOT NULL DEFAULT 1,
    ipv6_display_name TEXT    NOT NULL DEFAULT '',

    -- 迁移 0031 / 0032 用 ADD COLUMN 加在表尾的四列,位置照旧放在最后。
    snell_shared_psk          INTEGER NOT NULL DEFAULT 0,
    deployed_snell_shared_psk INTEGER NOT NULL DEFAULT 0,
    unmetered                 INTEGER NOT NULL DEFAULT 0,
    deployed_unmetered        INTEGER NOT NULL DEFAULT 0,

    CHECK (
        (chain_target_kind = ''         AND chain_target_inbound_id  IS NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'INBOUND'  AND chain_target_inbound_id  IS NOT NULL
                                        AND chain_target_external_id IS NULL) OR
        (chain_target_kind = 'EXTERNAL' AND chain_target_external_id IS NOT NULL
                                        AND chain_target_inbound_id  IS NULL)
    )
);

-- 逐列写出来而不是 SELECT *:与 0030 同一个理由 —— 靠位置对齐的话,
-- 哪天两张表的列顺序不一致,值就会被塞进别的列里。
INSERT INTO node_inbounds_new (
    id, node_id, tag, display_name, protocol,
    listen_port, public_port, ipv6_public_port, tcp_fast_open,
    reality_dest, reality_dest_port, reality_privkey_encrypted,
    reality_pubkey, reality_short_id,
    handshake_max_record_size, handshake_checked_at,
    ss_method, ss_password_encrypted,
    snell_version, snell_psk_encrypted, snell_obfs_mode, snell_obfs_host, snell_v6_mode,
    chain_target_kind, chain_target_inbound_id, chain_target_external_id,
    chain_code, chain_uuid_encrypted, chain_ss_password_encrypted,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
    deployed_snell_version, deployed_snell_obfs_mode, deployed_snell_v6_mode,
    deleted_at, created_at, updated_at,
    ipv6_enabled, ipv6_display_name,
    snell_shared_psk, deployed_snell_shared_psk, unmetered, deployed_unmetered)
SELECT
    id, node_id, tag, display_name, protocol,
    listen_port, public_port, ipv6_public_port, tcp_fast_open,
    reality_dest, reality_dest_port, reality_privkey_encrypted,
    reality_pubkey, reality_short_id,
    handshake_max_record_size, handshake_checked_at,
    ss_method, ss_password_encrypted,
    snell_version, snell_psk_encrypted, snell_obfs_mode, snell_obfs_host, snell_v6_mode,
    chain_target_kind, chain_target_inbound_id, chain_target_external_id,
    chain_code, chain_uuid_encrypted, chain_ss_password_encrypted,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
    deployed_snell_version, deployed_snell_obfs_mode, deployed_snell_v6_mode,
    deleted_at, created_at, updated_at,
    ipv6_enabled, ipv6_display_name,
    snell_shared_psk, deployed_snell_shared_psk, unmetered, deployed_unmetered
  FROM node_inbounds;

DROP TABLE node_inbounds;
ALTER TABLE node_inbounds_new RENAME TO node_inbounds;

-- 索引与迁移 0030 逐字相同,少建一个的后果见那里的注释。
CREATE UNIQUE INDEX idx_node_inbounds_listen ON node_inbounds(node_id, listen_port)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_node_inbounds_tag ON node_inbounds(node_id, tag)
    WHERE tag != '';
CREATE INDEX idx_node_inbounds_node ON node_inbounds(node_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_inbounds_chain_inbound  ON node_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_inbounds_chain_external ON node_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';

-- 三个视图逐字重建,**规则一个字都没改**:前两个取自 0030,user_effective_relays
-- 取自 0033(那一版把它重建过,ADDRESS 刻意没有第三段 UNION)。
-- 顺手改一条规则会让"这次升级到底改了什么"变成一个要逐行比对才能回答的问题,
-- 而访问范围正是最不该靠比对来确认的东西。规则若要变更,必须再新增一条迁移。
CREATE VIEW user_effective_inbounds AS
    -- 1. 等级继承
    SELECT u.id AS proxy_user_id, i.id AS inbound_id, i.node_id AS node_id
      FROM node_inbounds i
      JOIN nodes        n  ON n.id = i.node_id AND n.deleted_at IS NULL
      JOIN proxy_users  u  ON u.deleted_at IS NULL
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND it.level <= ut.level
    UNION
    -- 2. user_nodes 里管理员对整台机器的额外授权
    SELECT un.proxy_user_id, i.id, i.node_id
      FROM user_nodes    un
      JOIN proxy_users   u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL
      JOIN nodes         n ON n.id = un.node_id       AND n.deleted_at IS NULL
      JOIN node_inbounds i ON i.node_id = n.id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1;

CREATE VIEW user_effective_nodes AS
    SELECT DISTINCT proxy_user_id, node_id FROM user_effective_inbounds;

CREATE VIEW user_effective_relays AS
    -- 落地是自建节点的某个入站
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays   r
      JOIN nodes         a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users   u  ON u.deleted_at IS NULL
      JOIN access_tiers  ut ON ut.id = u.access_tier_id
      JOIN access_tiers  rt ON rt.id = r.access_tier_id
      JOIN node_inbounds b  ON b.id = r.target_inbound_id AND b.deleted_at IS NULL
      JOIN nodes         bn ON bn.id = b.node_id AND bn.deleted_at IS NULL
      JOIN user_effective_inbounds eb
             ON eb.inbound_id = b.id AND eb.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'INBOUND'
       AND rt.level <= ut.level
       AND bn.status != 'DISABLED'
       AND b.deployed_protocol != ''
    UNION
    -- 落地是外部代理
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays      r
      JOIN nodes            a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users      u  ON u.deleted_at IS NULL
      JOIN access_tiers     ut ON ut.id = u.access_tier_id
      JOIN access_tiers     rt ON rt.id = r.access_tier_id
      JOIN external_proxies p  ON p.id = r.target_external_id AND p.deleted_at IS NULL
      JOIN user_effective_external_proxies en_p
             ON en_p.external_proxy_id = p.id AND en_p.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'EXTERNAL'
       AND rt.level <= ut.level;
