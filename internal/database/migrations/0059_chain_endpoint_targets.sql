-- v23.3:链式与转发的目标精确到「入口 + 地址条目」,转发规则可以指向另一条转发规则。
--
-- 在此之前链式出站与转发规则只记「哪个入口」,连接地址在渲染时现算:订阅 IPv4 优先(地址池首条 V4 的镜像)、
-- 回落管理地址;端口取入口的公网端口、回落监听端口。于是**地址池一重排,链路的目的地址就跟着变了**,
-- 而管理员在入口上配的多条地址里到底连的是哪一条,谁都说不清。这一版把「连哪一条」变成存下来的事实:
--
--   *_target_kind        多收一个 RELAY:下一跳是一条 nginx / realm 转发规则(透明转发,不认证)。
--   *_target_relay_id    RELAY 时指向 node_relays.id。
--   *_target_endpoint_id 目标入口上被选中的那条地址条目(inbound_endpoints.id)。
--   *_target_addr_mode   目标是 INBOUND / RELAY 时的地址来源:
--                          ENDPOINT  连 endpoint_id 那一条(新建关系一律是它);
--                          DEFAULT   目标入口一条地址条目都没有,连「管理地址 + 跟随端口」——
--                                    没有条目的入口只有这一个默认地址,不依赖任何列表顺序;
--                          LEGACY    迁移时找不到唯一等价的条目,原样保留迁移前实际连的地址与端口
--                                    (legacy_host / legacy_port),界面上标「待确认地址选择」。
--                        目标是外部代理或直连时为空串。
--   *_target_legacy_*    只在 LEGACY 下有值。
--
-- 回填的唯一目标是**升级前后每条关系实际连的地址与端口一个字节不变**:先按迁移前的解析规则算出旧的有效目标
-- (域名保持域名),有唯一等价的条目才绑定它;没有或有歧义的保留兼容快照,不绑「第一条」,
-- 也不为了凑匹配新增会进订阅的地址条目。链路凭据、授权、订阅名与排序一个都不动,迁移不触发任何下发。
--
-- node_inbounds 与 node_relays 的 *_kind 有 CHECK,只能按 0052 的做法重建整张表(先拆依赖它们的三个视图);
-- node_mieru_inbounds 的那几列当年是 ADD COLUMN 加的、没有 CHECK,直接加列。新列一律放在表尾,
-- 与 PG 那一份 ADD COLUMN 的列顺序一致。
--
-- user_effective_relays 改成沿转发级联走到第一个代理认证终点:用户资格 = 沿途每一条转发的访问等级
-- (取最高的那个) ∩ 他在认证终点上的有效权限。不级联的规则(存量全部)结果逐行不变 ——
-- database/chain_endpoint_upgrade_test.go 逐行比对升级前后的三张表与三个有效权限视图,并核对每条关系的有效目标。

DROP VIEW user_effective_relays;
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;

-- ---------- node_inbounds ----------
CREATE TABLE node_inbounds_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 以下各列的含义与迁移 0019 / 0030 / 0031 / 0032 / 0052 完全一致,注释不再重复。
    tag          TEXT NOT NULL,
    display_name TEXT NOT NULL,

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

    -- 这次重建放宽的约束之一:多收一个 RELAY。
    chain_target_kind TEXT NOT NULL DEFAULT ''
        CHECK (chain_target_kind IN ('', 'INBOUND', 'EXTERNAL', 'RELAY')),
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

    snell_shared_psk          INTEGER NOT NULL DEFAULT 0,
    deployed_snell_shared_psk INTEGER NOT NULL DEFAULT 0,
    unmetered                 INTEGER NOT NULL DEFAULT 0,
    deployed_unmetered        INTEGER NOT NULL DEFAULT 0,

    restricted INTEGER NOT NULL DEFAULT 0 CHECK (restricted IN (0, 1)),

    -- v23.3 新列(放在表尾,与 PG 的 ADD COLUMN 顺序一致)。
    chain_target_relay_id    INTEGER,
    chain_target_endpoint_id INTEGER,
    chain_target_addr_mode   TEXT    NOT NULL DEFAULT ''
        CHECK (chain_target_addr_mode IN ('', 'ENDPOINT', 'DEFAULT', 'LEGACY')),
    chain_target_legacy_host TEXT    NOT NULL DEFAULT '',
    chain_target_legacy_port INTEGER NOT NULL DEFAULT 0
        CHECK (chain_target_legacy_port BETWEEN 0 AND 65535),

    CHECK (
        (chain_target_kind = ''         AND chain_target_inbound_id  IS NULL
                                        AND chain_target_external_id IS NULL
                                        AND chain_target_relay_id    IS NULL) OR
        (chain_target_kind = 'INBOUND'  AND chain_target_inbound_id  IS NOT NULL
                                        AND chain_target_external_id IS NULL
                                        AND chain_target_relay_id    IS NULL) OR
        (chain_target_kind = 'EXTERNAL' AND chain_target_external_id IS NOT NULL
                                        AND chain_target_inbound_id  IS NULL
                                        AND chain_target_relay_id    IS NULL) OR
        (chain_target_kind = 'RELAY'    AND chain_target_relay_id    IS NOT NULL
                                        AND chain_target_inbound_id  IS NULL
                                        AND chain_target_external_id IS NULL)
    )
);

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

CREATE UNIQUE INDEX idx_node_inbounds_listen ON node_inbounds(node_id, listen_port)
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_node_inbounds_tag ON node_inbounds(node_id, tag)
    WHERE tag != '';
CREATE INDEX idx_node_inbounds_node ON node_inbounds(node_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_inbounds_chain_inbound  ON node_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_inbounds_chain_external ON node_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';
CREATE INDEX idx_node_inbounds_chain_relay ON node_inbounds(chain_target_relay_id)
    WHERE chain_target_kind = 'RELAY';
CREATE INDEX idx_node_inbounds_chain_endpoint ON node_inbounds(chain_target_endpoint_id)
    WHERE chain_target_endpoint_id IS NOT NULL;

-- ---------- node_relays ----------
CREATE TABLE node_relays_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine  TEXT NOT NULL DEFAULT 'NGINX' CHECK (engine IN ('NGINX', 'REALM')),
    display_name TEXT NOT NULL,
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),

    -- 这次重建放宽的约束之二:多收一个 RELAY(透明转发级联)。
    target_kind TEXT NOT NULL CHECK (target_kind IN ('INBOUND', 'EXTERNAL', 'ADDRESS', 'RELAY')),
    target_inbound_id  INTEGER,
    target_external_id INTEGER,
    target_host TEXT    NOT NULL DEFAULT '',
    target_port INTEGER NOT NULL DEFAULT 0
        CHECK (target_port = 0 OR target_port BETWEEN 1 AND 65535),

    access_tier_id INTEGER NOT NULL DEFAULT 1,
    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- v23.3 新列。
    target_relay_id    INTEGER,
    target_endpoint_id INTEGER,
    target_addr_mode   TEXT    NOT NULL DEFAULT ''
        CHECK (target_addr_mode IN ('', 'ENDPOINT', 'DEFAULT', 'LEGACY')),
    target_legacy_host TEXT    NOT NULL DEFAULT '',
    target_legacy_port INTEGER NOT NULL DEFAULT 0
        CHECK (target_legacy_port BETWEEN 0 AND 65535),

    CHECK (
        (target_kind = 'INBOUND'  AND target_inbound_id  IS NOT NULL
                                  AND target_external_id IS NULL
                                  AND target_relay_id    IS NULL
                                  AND target_host = '' AND target_port = 0) OR
        (target_kind = 'EXTERNAL' AND target_external_id IS NOT NULL
                                  AND target_inbound_id  IS NULL
                                  AND target_relay_id    IS NULL
                                  AND target_host = '' AND target_port = 0) OR
        (target_kind = 'ADDRESS'  AND target_inbound_id  IS NULL
                                  AND target_external_id IS NULL
                                  AND target_relay_id    IS NULL
                                  AND target_host != '' AND target_port != 0) OR
        (target_kind = 'RELAY'    AND target_relay_id    IS NOT NULL
                                  AND target_inbound_id  IS NULL
                                  AND target_external_id IS NULL
                                  AND target_host = '' AND target_port = 0)
    )
);

INSERT INTO node_relays_new (
    id, node_id, engine, display_name, listen_port, public_port,
    target_kind, target_inbound_id, target_external_id, target_host, target_port,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deleted_at, created_at, updated_at)
SELECT
    id, node_id, engine, display_name, listen_port, public_port,
    target_kind, target_inbound_id, target_external_id, target_host, target_port,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deleted_at, created_at, updated_at
  FROM node_relays;

DROP TABLE node_relays;
ALTER TABLE node_relays_new RENAME TO node_relays;

CREATE UNIQUE INDEX idx_node_relays_listen
    ON node_relays(node_id, listen_port) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_node     ON node_relays(node_id)     WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_target   ON node_relays(target_inbound_id)
    WHERE deleted_at IS NULL AND target_kind = 'INBOUND';
CREATE INDEX idx_node_relays_external ON node_relays(target_external_id)
    WHERE deleted_at IS NULL AND target_kind = 'EXTERNAL';
CREATE INDEX idx_node_relays_relay    ON node_relays(target_relay_id)
    WHERE deleted_at IS NULL AND target_kind = 'RELAY';
CREATE INDEX idx_node_relays_endpoint ON node_relays(target_endpoint_id)
    WHERE target_endpoint_id IS NOT NULL;

-- ---------- node_mieru_inbounds(没有 CHECK,直接加列) ----------
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_relay_id INTEGER;
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_endpoint_id INTEGER;
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_addr_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_legacy_host TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_legacy_port INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_node_mieru_chain_relay ON node_mieru_inbounds(chain_target_relay_id)
    WHERE chain_target_kind = 'RELAY';

-- ---------- 换了认证终点之后、上游还没切换过去之前,旧终点上保留的链路凭据 ----------
--
-- 一个上游入口(sing-box 或 Mieru)的认证终点从 L 换走时插一行:L 的用户列表里继续保留这个上游的链路凭据,
-- 直到上游那台机器的 sing-box 成功下发过一次新配置(那之后它不再拿旧参数去连 L),这一行才删掉、并把 L 标脏
-- 去撤掉凭据。不留这一步的话,L 只要被任何别的原因先下发一次,就把一份远端还在用的凭据撤掉了 ——
-- 数据库的旧边已经被覆盖,不代表远端那份旧配置已经不用它了。凭据本身取自上游那一行(链路凭据一经分配不变),
-- 这里只记「还要在哪个终点上保留」。
CREATE TABLE chain_credential_holds (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    source_kind        TEXT    NOT NULL CHECK (source_kind IN ('SINGBOX', 'MIERU')),
    source_id          INTEGER NOT NULL,
    source_node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    landing_inbound_id INTEGER NOT NULL,
    chain_code         TEXT    NOT NULL,
    created_at         TEXT    NOT NULL,
    UNIQUE (source_kind, source_id, landing_inbound_id)
);
CREATE INDEX idx_chain_credential_holds_landing ON chain_credential_holds(landing_inbound_id);
CREATE INDEX idx_chain_credential_holds_node    ON chain_credential_holds(source_node_id);

-- ---------- 回填:存量关系的地址来源 ----------
--
-- 旧的有效目标 = 迁移前 chainInboundTarget 的解析:地址取目标机器的订阅 IPv4(地址池首条 V4 的镜像)、
-- 为空回落管理地址;端口取目标入站的公网端口、为 0 回落监听端口。先全部记成 LEGACY + 快照,再逐级收窄。

-- 1. 先把旧的有效目标原样记下来。
UPDATE node_inbounds SET
    chain_target_addr_mode = 'LEGACY',
    chain_target_legacy_host = COALESCE((
        SELECT CASE WHEN tn.sub_ipv4_address != '' THEN tn.sub_ipv4_address ELSE tn.host END
          FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_inbounds.chain_target_inbound_id), ''),
    chain_target_legacy_port = COALESCE((
        SELECT CASE WHEN t.public_port != 0 THEN t.public_port ELSE t.listen_port END
          FROM node_inbounds t WHERE t.id = node_inbounds.chain_target_inbound_id), 0)
 WHERE chain_target_kind = 'INBOUND';

UPDATE node_mieru_inbounds SET
    chain_target_addr_mode = 'LEGACY',
    chain_target_legacy_host = COALESCE((
        SELECT CASE WHEN tn.sub_ipv4_address != '' THEN tn.sub_ipv4_address ELSE tn.host END
          FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_mieru_inbounds.chain_target_inbound_id), ''),
    chain_target_legacy_port = COALESCE((
        SELECT CASE WHEN t.public_port != 0 THEN t.public_port ELSE t.listen_port END
          FROM node_inbounds t WHERE t.id = node_mieru_inbounds.chain_target_inbound_id), 0)
 WHERE chain_target_kind = 'INBOUND';

UPDATE node_relays SET
    target_addr_mode = 'LEGACY',
    target_legacy_host = COALESCE((
        SELECT CASE WHEN tn.sub_ipv4_address != '' THEN tn.sub_ipv4_address ELSE tn.host END
          FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_relays.target_inbound_id), ''),
    target_legacy_port = COALESCE((
        SELECT CASE WHEN t.public_port != 0 THEN t.public_port ELSE t.listen_port END
          FROM node_inbounds t WHERE t.id = node_relays.target_inbound_id), 0)
 WHERE target_kind = 'INBOUND';

-- 2. 目标入站上恰好有一条地址条目解析出同一个地址与端口:绑定它。
--    条目的解析与订阅一致:地址 = 地址池那一条(address_id 为空是管理地址),端口 0 跟随监听端口。
--    SET 里的表达式一律按更新前的那一行求值,所以同一句里清快照不影响上面的比较。
UPDATE node_inbounds SET
    chain_target_addr_mode = 'ENDPOINT',
    chain_target_endpoint_id = (
        SELECT e.id FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_inbounds.chain_target_inbound_id
           AND COALESCE(a.address, tn.host) = node_inbounds.chain_target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_inbounds.chain_target_legacy_port),
    chain_target_legacy_host = '',
    chain_target_legacy_port = 0
 WHERE chain_target_kind = 'INBOUND' AND chain_target_addr_mode = 'LEGACY'
   AND (SELECT COUNT(*) FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_inbounds.chain_target_inbound_id
           AND COALESCE(a.address, tn.host) = node_inbounds.chain_target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_inbounds.chain_target_legacy_port) = 1;

UPDATE node_mieru_inbounds SET
    chain_target_addr_mode = 'ENDPOINT',
    chain_target_endpoint_id = (
        SELECT e.id FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_mieru_inbounds.chain_target_inbound_id
           AND COALESCE(a.address, tn.host) = node_mieru_inbounds.chain_target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_mieru_inbounds.chain_target_legacy_port),
    chain_target_legacy_host = '',
    chain_target_legacy_port = 0
 WHERE chain_target_kind = 'INBOUND' AND chain_target_addr_mode = 'LEGACY'
   AND (SELECT COUNT(*) FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_mieru_inbounds.chain_target_inbound_id
           AND COALESCE(a.address, tn.host) = node_mieru_inbounds.chain_target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_mieru_inbounds.chain_target_legacy_port) = 1;

UPDATE node_relays SET
    target_addr_mode = 'ENDPOINT',
    target_endpoint_id = (
        SELECT e.id FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_relays.target_inbound_id
           AND COALESCE(a.address, tn.host) = node_relays.target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_relays.target_legacy_port),
    target_legacy_host = '',
    target_legacy_port = 0
 WHERE target_kind = 'INBOUND' AND target_addr_mode = 'LEGACY'
   AND (SELECT COUNT(*) FROM inbound_endpoints e
          JOIN node_inbounds t ON t.id = e.entry_id
          JOIN nodes tn ON tn.id = t.node_id
          LEFT JOIN node_addresses a ON a.id = e.address_id
         WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_relays.target_inbound_id
           AND COALESCE(a.address, tn.host) = node_relays.target_legacy_host
           AND (CASE WHEN e.public_port != 0 THEN e.public_port ELSE t.listen_port END)
               = node_relays.target_legacy_port) = 1;

-- 3. 目标入站一条地址条目都没有、而旧目标就是它的管理地址(订阅 IPv4 为空):记成 DEFAULT。
--    DEFAULT 的端口规则(公网端口、回落监听端口)与旧目标一字不差,地址跟着管理地址走 —— 与升级前同一个行为。
UPDATE node_inbounds SET
    chain_target_addr_mode = 'DEFAULT', chain_target_legacy_host = '', chain_target_legacy_port = 0
 WHERE chain_target_kind = 'INBOUND' AND chain_target_addr_mode = 'LEGACY'
   AND NOT EXISTS (SELECT 1 FROM inbound_endpoints e
                    WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_inbounds.chain_target_inbound_id)
   AND chain_target_legacy_host = COALESCE((
        SELECT tn.host FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_inbounds.chain_target_inbound_id), '');

UPDATE node_mieru_inbounds SET
    chain_target_addr_mode = 'DEFAULT', chain_target_legacy_host = '', chain_target_legacy_port = 0
 WHERE chain_target_kind = 'INBOUND' AND chain_target_addr_mode = 'LEGACY'
   AND NOT EXISTS (SELECT 1 FROM inbound_endpoints e
                    WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_mieru_inbounds.chain_target_inbound_id)
   AND chain_target_legacy_host = COALESCE((
        SELECT tn.host FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_mieru_inbounds.chain_target_inbound_id), '');

UPDATE node_relays SET
    target_addr_mode = 'DEFAULT', target_legacy_host = '', target_legacy_port = 0
 WHERE target_kind = 'INBOUND' AND target_addr_mode = 'LEGACY'
   AND NOT EXISTS (SELECT 1 FROM inbound_endpoints e
                    WHERE e.entry_kind = 'SINGBOX' AND e.entry_id = node_relays.target_inbound_id)
   AND target_legacy_host = COALESCE((
        SELECT tn.host FROM node_inbounds t JOIN nodes tn ON tn.id = t.node_id
         WHERE t.id = node_relays.target_inbound_id), '');

-- 剩下的仍是 LEGACY:快照就是升级前实际连的那一对,渲染照它连,界面标「待确认地址选择」。

-- ---------- 视图 ----------
-- 前两个与 0052 逐字相同。
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

-- 一条转发规则沿 RELAY 级联走到的那条「终点转发」(它的去向是入站、外部代理或指定地址),
-- 以及沿途(含起点)最高的访问等级。中间的规则必须启用、所在机器没删没禁用 —— 停用的中间入口让整条路径断掉,
-- 不绕过它。深度封顶 8:存量里的环走不到终点,自然不出现在结果里,也拖不垮查询。
-- 不级联的规则只有起点那一行,终点就是它自己、等级就是它自己的等级。
CREATE VIEW relay_forward_paths AS
    WITH RECURSIVE walk(relay_id, cur_id, depth, max_level) AS (
        SELECT r.id, r.id, 0, rt.level
          FROM node_relays r
          JOIN access_tiers rt ON rt.id = r.access_tier_id
         WHERE r.deleted_at IS NULL
        UNION
        SELECT w.relay_id, nx.id, w.depth + 1,
               CASE WHEN nt.level > w.max_level THEN nt.level ELSE w.max_level END
          FROM walk w
          JOIN node_relays  cur ON cur.id = w.cur_id AND cur.target_kind = 'RELAY'
          JOIN node_relays  nx  ON nx.id = cur.target_relay_id AND nx.deleted_at IS NULL AND nx.enabled = 1
          JOIN nodes        nn  ON nn.id = nx.node_id AND nn.deleted_at IS NULL AND nn.status != 'DISABLED'
          JOIN access_tiers nt  ON nt.id = nx.access_tier_id
         WHERE w.depth < 8
    )
    SELECT w.relay_id AS relay_id, w.cur_id AS terminal_relay_id, w.max_level AS max_level
      FROM walk w
      JOIN node_relays t ON t.id = w.cur_id
     WHERE t.target_kind != 'RELAY';

-- 用户资格 = 沿途转发的访问等级(取最高)∩ 他在第一个代理认证终点上的有效权限。
-- 中间转发的订阅开关不参与:那只控制它自己公不公开,不等于转发服务关了。
CREATE VIEW user_effective_relays AS
    -- 认证终点是自建节点的某个入站
    SELECT u.id AS proxy_user_id, fp.relay_id AS relay_id
      FROM relay_forward_paths fp
      JOIN node_relays   r  ON r.id = fp.relay_id
      JOIN nodes         a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN node_relays   t  ON t.id = fp.terminal_relay_id
      JOIN proxy_users   u  ON u.deleted_at IS NULL
      JOIN access_tiers  ut ON ut.id = u.access_tier_id
      JOIN node_inbounds b  ON b.id = t.target_inbound_id AND b.deleted_at IS NULL
      JOIN nodes         bn ON bn.id = b.node_id AND bn.deleted_at IS NULL
      JOIN user_effective_inbounds eb
             ON eb.inbound_id = b.id AND eb.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND t.target_kind = 'INBOUND'
       AND fp.max_level <= ut.level
       AND bn.status != 'DISABLED'
       AND b.deployed_protocol != ''
    UNION
    -- 认证终点是外部代理
    SELECT u.id AS proxy_user_id, fp.relay_id AS relay_id
      FROM relay_forward_paths fp
      JOIN node_relays      r  ON r.id = fp.relay_id
      JOIN nodes            a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN node_relays      t  ON t.id = fp.terminal_relay_id
      JOIN proxy_users      u  ON u.deleted_at IS NULL
      JOIN access_tiers     ut ON ut.id = u.access_tier_id
      JOIN external_proxies p  ON p.id = t.target_external_id AND p.deleted_at IS NULL
      JOIN user_effective_external_proxies en_p
             ON en_p.external_proxy_id = p.id AND en_p.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND t.target_kind = 'EXTERNAL'
       AND fp.max_level <= ut.level;
