-- (PostgreSQL 版,与 SQLite 的 0049 同一份逻辑结构)
-- VNext AUTH01–03:用户 × 入口的直接授权。
--
-- 原来能用哪个入口只由两条规则决定:入口等级继承(G)与整机额外授权(N,user_nodes)。
-- 这一版加第三条:直接入口授权(D,user_inbound_grants),并给用户一个模式、给入口一个开关:
--
--   proxy_users.access_mode = 'INHERIT'(默认)  归属 = G ∪ N ∪ D
--                           = 'DIRECT_ONLY'        归属 = D
--   入口的 restricted = 1(「仅指定用户」)         这个入口只经 D 出现
--
-- 直接授权只能越过**等级门槛**,越不过硬条件:账号停用 / 过期 / 超额(由 User.Serviceable 在 Go 侧照旧拦)、
-- 入口停用 / 删除、机器删除。默认值让存量数据的归属集合逐用户一个不变(升级测试逐行比对)。
--
-- 入口的身份是「种类 + id」:sing-box 入口与 Mieru 入口在两张表里,id 会撞。
-- 规则只写在视图里(V2 那条规矩),配置生成、订阅、门户、脏标记全都查视图,不各写一遍。

ALTER TABLE proxy_users ADD COLUMN access_mode TEXT NOT NULL DEFAULT 'INHERIT'
    CHECK (access_mode IN ('INHERIT', 'DIRECT_ONLY'));
ALTER TABLE node_inbounds ADD COLUMN restricted BIGINT NOT NULL DEFAULT 0 CHECK (restricted IN (0, 1));
ALTER TABLE node_mieru_inbounds ADD COLUMN restricted BIGINT NOT NULL DEFAULT 0 CHECK (restricted IN (0, 1));

CREATE TABLE user_inbound_grants (
    proxy_user_id BIGINT NOT NULL REFERENCES proxy_users(id) ON DELETE CASCADE,
    entry_kind    TEXT    NOT NULL CHECK (entry_kind IN ('SINGBOX', 'MIERU')),
    entry_id      BIGINT NOT NULL,
    created_at    TEXT    NOT NULL,
    PRIMARY KEY (proxy_user_id, entry_kind, entry_id)
);
CREATE INDEX idx_user_inbound_grants_entry ON user_inbound_grants(entry_kind, entry_id);

-- 依赖顺序:中转视图与有效节点视图都建在有效入口视图之上,先拆它们。
DROP VIEW user_effective_relays;
DROP VIEW user_effective_nodes;
DROP VIEW user_effective_inbounds;
DROP VIEW user_effective_mieru_inbounds;

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

CREATE VIEW user_effective_mieru_inbounds AS
    -- 1. 等级继承
    SELECT u.id AS proxy_user_id, i.id AS mieru_inbound_id, i.node_id AS node_id
      FROM node_mieru_inbounds i
      JOIN nodes        n  ON n.id = i.node_id AND n.deleted_at IS NULL
      JOIN proxy_users  u  ON u.deleted_at IS NULL AND u.access_mode = 'INHERIT'
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND i.restricted = 0
       AND it.level <= ut.level
    UNION
    -- 2. user_nodes 里管理员对整台机器的额外授权
    SELECT un.proxy_user_id, i.id, i.node_id
      FROM user_nodes          un
      JOIN proxy_users         u ON u.id = un.proxy_user_id AND u.deleted_at IS NULL AND u.access_mode = 'INHERIT'
      JOIN nodes               n ON n.id = un.node_id       AND n.deleted_at IS NULL
      JOIN node_mieru_inbounds i ON i.node_id = n.id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND i.restricted = 0
    UNION
    -- 3. 直接入口授权
    SELECT g.proxy_user_id, i.id, i.node_id
      FROM user_inbound_grants g
      JOIN proxy_users         u ON u.id = g.proxy_user_id AND u.deleted_at IS NULL
      JOIN node_mieru_inbounds i ON i.id = g.entry_id
      JOIN nodes               n ON n.id = i.node_id AND n.deleted_at IS NULL
     WHERE g.entry_kind = 'MIERU'
       AND i.deleted_at IS NULL
       AND i.enabled = 1;
