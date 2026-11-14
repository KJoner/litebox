-- (PostgreSQL 版,与 SQLite 的 0059 同一份逻辑结构;含义见那一份的文件头。)
-- v23.3:链式与转发的目标精确到「入口 + 地址条目」,转发规则可以指向另一条转发规则。
-- PG 不用重建表:换掉 *_target_kind 的 CHECK、在表尾加列即可(列顺序与 SQLite 重建后的一致)。
-- 约束名按内容查(基线里没有显式命名)。user_effective_inbounds / user_effective_nodes 不受影响,
-- 只重建 user_effective_relays(它要沿转发级联走),并新增 relay_forward_paths。

DROP VIEW user_effective_relays;

DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
              WHERE conrelid = 'node_inbounds'::regclass AND contype = 'c'
                AND pg_get_constraintdef(oid) LIKE '%chain_target_kind%' LOOP
        EXECUTE format('ALTER TABLE node_inbounds DROP CONSTRAINT %I', c);
    END LOOP;
    FOR c IN SELECT conname FROM pg_constraint
              WHERE conrelid = 'node_relays'::regclass AND contype = 'c'
                AND pg_get_constraintdef(oid) LIKE '%target_kind%' LOOP
        EXECUTE format('ALTER TABLE node_relays DROP CONSTRAINT %I', c);
    END LOOP;
END $$;

-- ---------- node_inbounds ----------
ALTER TABLE node_inbounds ADD COLUMN chain_target_relay_id BIGINT;
ALTER TABLE node_inbounds ADD COLUMN chain_target_endpoint_id BIGINT;
ALTER TABLE node_inbounds ADD COLUMN chain_target_addr_mode TEXT NOT NULL DEFAULT ''
    CHECK (chain_target_addr_mode IN ('', 'ENDPOINT', 'DEFAULT', 'LEGACY'));
ALTER TABLE node_inbounds ADD COLUMN chain_target_legacy_host TEXT NOT NULL DEFAULT '';
ALTER TABLE node_inbounds ADD COLUMN chain_target_legacy_port BIGINT NOT NULL DEFAULT 0
    CHECK (chain_target_legacy_port BETWEEN 0 AND 65535);
ALTER TABLE node_inbounds ADD CONSTRAINT node_inbounds_chain_target_kind_check
    CHECK (chain_target_kind IN ('', 'INBOUND', 'EXTERNAL', 'RELAY'));
ALTER TABLE node_inbounds ADD CONSTRAINT node_inbounds_chain_target_check CHECK (
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
                                    AND chain_target_external_id IS NULL));
CREATE INDEX idx_node_inbounds_chain_relay ON node_inbounds(chain_target_relay_id)
    WHERE chain_target_kind = 'RELAY';
CREATE INDEX idx_node_inbounds_chain_endpoint ON node_inbounds(chain_target_endpoint_id)
    WHERE chain_target_endpoint_id IS NOT NULL;

-- ---------- node_relays ----------
ALTER TABLE node_relays ADD COLUMN target_relay_id BIGINT;
ALTER TABLE node_relays ADD COLUMN target_endpoint_id BIGINT;
ALTER TABLE node_relays ADD COLUMN target_addr_mode TEXT NOT NULL DEFAULT ''
    CHECK (target_addr_mode IN ('', 'ENDPOINT', 'DEFAULT', 'LEGACY'));
ALTER TABLE node_relays ADD COLUMN target_legacy_host TEXT NOT NULL DEFAULT '';
ALTER TABLE node_relays ADD COLUMN target_legacy_port BIGINT NOT NULL DEFAULT 0
    CHECK (target_legacy_port BETWEEN 0 AND 65535);
ALTER TABLE node_relays ADD CONSTRAINT node_relays_target_kind_check
    CHECK (target_kind IN ('INBOUND', 'EXTERNAL', 'ADDRESS', 'RELAY'));
ALTER TABLE node_relays ADD CONSTRAINT node_relays_target_check CHECK (
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
                              AND target_host = '' AND target_port = 0));
CREATE INDEX idx_node_relays_relay    ON node_relays(target_relay_id)
    WHERE deleted_at IS NULL AND target_kind = 'RELAY';
CREATE INDEX idx_node_relays_endpoint ON node_relays(target_endpoint_id)
    WHERE target_endpoint_id IS NOT NULL;

-- ---------- node_mieru_inbounds ----------
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_relay_id BIGINT;
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_endpoint_id BIGINT;
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_addr_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_legacy_host TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_legacy_port BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_node_mieru_chain_relay ON node_mieru_inbounds(chain_target_relay_id)
    WHERE chain_target_kind = 'RELAY';

-- ---------- 旧认证终点上保留的链路凭据 ----------
CREATE TABLE chain_credential_holds (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    source_kind        TEXT   NOT NULL CHECK (source_kind IN ('SINGBOX', 'MIERU')),
    source_id          BIGINT NOT NULL,
    source_node_id     BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    landing_inbound_id BIGINT NOT NULL,
    chain_code         TEXT   NOT NULL,
    created_at         TEXT   NOT NULL,
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
