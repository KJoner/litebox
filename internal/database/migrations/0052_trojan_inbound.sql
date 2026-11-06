-- VNext PROTO02:自建 Trojan + REALITY 入站。
--
-- node_inbounds.protocol 的 CHECK 只收 VLESS_REALITY / SHADOWSOCKS / SNELL,要加 TROJAN
-- 只能重建整张表 —— 做法逐字照搬 0046(那一次放宽的是 ss_method):先拆依赖它的三个视图,
-- 建新表、逐列搬数据(连 0049 加的 restricted 一起,一列都不能少)、改名、重建索引与视图。
-- 视图取自 0049(三条规则:等级继承、整机授权、直接授权),**一个字都没改**。
--
-- Trojan 不新增任何列:TLS 那一层复用 REALITY 的那几列,用户凭据复用 UUID。
-- 存量行的取值一个不变,升级之后渲染结果逐字节相同,不会有任何一台机器因此被判成「待部署」。
-- database/trojan_upgrade_test.go 逐行比对升级前后的 node_inbounds 与四个有效权限视图。

DROP VIEW user_effective_relays;
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;

CREATE TABLE node_inbounds_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 以下各列的含义与迁移 0019 / 0030 / 0031 / 0032 完全一致,注释不再重复。
    tag          TEXT NOT NULL,
    display_name TEXT NOT NULL,

    -- 这次重建唯一的变化:protocol 多收一个 TROJAN。取值必须与 singbox.ParseProtocol 收的一字不差。
    protocol TEXT NOT NULL DEFAULT 'VLESS_REALITY'
        CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS', 'SNELL', 'TROJAN')),

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

    -- 迁移 0049 加在表尾的「仅指定用户」。
    restricted INTEGER NOT NULL DEFAULT 0 CHECK (restricted IN (0, 1)),

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
    snell_shared_psk, deployed_snell_shared_psk, unmetered, deployed_unmetered,
    restricted)
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
    snell_shared_psk, deployed_snell_shared_psk, unmetered, deployed_unmetered,
    restricted
  FROM node_inbounds;

DROP TABLE node_inbounds;
ALTER TABLE node_inbounds_new RENAME TO node_inbounds;

-- 索引与迁移 0030 / 0046 逐字相同。
CREATE UNIQUE INDEX idx_node_inbounds_listen ON node_inbounds(node_id, listen_port)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_node_inbounds_tag ON node_inbounds(node_id, tag)
    WHERE tag != '';
CREATE INDEX idx_node_inbounds_node ON node_inbounds(node_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_inbounds_chain_inbound  ON node_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_inbounds_chain_external ON node_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';


CREATE VIEW user_effective_inbounds AS
    -- 1. 等级继承(只对 INHERIT 模式;「仅指定用户」的入口不走这条)
    SELECT u.id AS proxy_user_id, i.id AS inbound_id, i.node_id AS node_id
      FROM node_inbounds i
      JOIN nodes        n  ON n.id = i.node_id AND n.deleted_at IS NULL
      JOIN proxy_users  u  ON u.deleted_at IS NULL AND u.access_mode = 'INHERIT'
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND i.restricted = 0
       AND it.level <= ut.level
    UNION
    -- 2. user_nodes 里管理员对整台机器的额外授权(同上两个条件)
    SELECT un.proxy_user_id, i.id, i.node_id
      FROM user_nodes    un
      JOIN proxy_users   u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL AND u.access_mode = 'INHERIT'
      JOIN nodes         n ON n.id = un.node_id       AND n.deleted_at IS NULL
      JOIN node_inbounds i ON i.node_id = n.id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND i.restricted = 0
    UNION
    -- 3. 直接入口授权(两种模式都认;越过等级门槛,越不过入口停用 / 删除)
    SELECT g.proxy_user_id, i.id, i.node_id
      FROM user_inbound_grants g
      JOIN proxy_users   u ON u.id = g.proxy_user_id AND u.deleted_at IS NULL
      JOIN node_inbounds i ON i.id = g.entry_id
      JOIN nodes         n ON n.id = i.node_id AND n.deleted_at IS NULL
     WHERE g.entry_kind = 'SINGBOX'
       AND i.deleted_at IS NULL
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
