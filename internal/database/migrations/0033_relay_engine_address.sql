-- V15:转发规则的第二种引擎(realm)与第三种落地(指定地址)。
--
-- ---------- engine ----------
--
-- realm 与 nginx stream 回答的是同一个问题(客户端连 A 的某个端口,字节被
-- 原样搬到落地),所以**同一张表加一列**,而不是另建一张 node_realm_rules:
-- 分两张表的话,端口冲突检测、订阅里的条目转换、门户的第三组、跨节点的脏标记
-- 传播四处都要各写两遍,而漏掉其中一处的表现是两种引擎抢同一个端口、
-- 或者一条 realm 线路在订阅里凭空消失。
--
-- 引擎与落地种类一样**一经创建不可改**:换引擎等于换掉服务用户的那个进程,
-- 而 realm 没有 reload —— 那次切换对在途连接的影响与新建一条线路一样。
--
-- ---------- ADDRESS ----------
--
-- 落地是一个由管理员直接填的 host:port,面板不知道它背后跑的是什么。
-- 所以这种规则**不进 user_effective_relays**:视图里没有它那一段 UNION,
-- 订阅与门户就造不出它的条目 —— 造不出是对的,面板手里没有凭据。
-- 它的用途是把这台机器当纯端口转发器,用户拿到的地址由管理员另行分发。
-- 拨测同样测不了(不知道协议),部署记录里记 SKIPPED 并写明原因。
--
-- SQLite 改不了 CHECK,只能重建。历史规则**全量搬过来**,engine 一律 NGINX ——
-- 在此之前只有这一种。

DROP VIEW user_effective_relays;

CREATE TABLE node_relays_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine  TEXT NOT NULL DEFAULT 'NGINX' CHECK (engine IN ('NGINX', 'REALM')),
    display_name TEXT NOT NULL,
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),

    target_kind TEXT NOT NULL CHECK (target_kind IN ('INBOUND', 'EXTERNAL', 'ADDRESS')),
    target_inbound_id  INTEGER,
    target_external_id INTEGER,
    -- 只有 ADDRESS 用:host 是 IPv4 / IPv6 / 域名,port 是落地的公网端口。
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

    CHECK (
        (target_kind = 'INBOUND'  AND target_inbound_id  IS NOT NULL
                                  AND target_external_id IS NULL
                                  AND target_host = '' AND target_port = 0) OR
        (target_kind = 'EXTERNAL' AND target_external_id IS NOT NULL
                                  AND target_inbound_id  IS NULL
                                  AND target_host = '' AND target_port = 0) OR
        (target_kind = 'ADDRESS'  AND target_inbound_id  IS NULL
                                  AND target_external_id IS NULL
                                  AND target_host != '' AND target_port != 0)
    )
);

INSERT INTO node_relays_new (
    id, node_id, engine, display_name, listen_port, public_port,
    target_kind, target_inbound_id, target_external_id, target_host, target_port,
    access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
    deleted_at, created_at, updated_at)
SELECT
    id, node_id, 'NGINX', display_name, listen_port, public_port,
    target_kind, target_inbound_id, target_external_id, '', 0,
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

-- 视图原样重建(与 0030 一字不差):ADDRESS 刻意没有第三段 UNION,见文件头。
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

-- ---------- deployments.kind 加 REALM ----------
--
-- 与 0027 同一个教训:加一种下发就要改这条 CHECK,否则每一次 realm 下发
-- 都落不了库,部署记录页上一条都没有。TestEveryDeployKindCanBeSaved 盯着。

CREATE TABLE deployments_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    revision      INTEGER NOT NULL,
    config_sha256 TEXT    NOT NULL,
    status        TEXT    NOT NULL
        CHECK (status IN ('RUNNING','SUCCESS','FAILED','ROLLED_BACK')),
    started_at    TEXT    NOT NULL,
    finished_at   TEXT,
    error_message TEXT    NOT NULL DEFAULT '',
    rollback_result TEXT  NOT NULL DEFAULT '',
    steps_json    TEXT    NOT NULL DEFAULT '[]',
    created_at    TEXT    NOT NULL,
    kind          TEXT    NOT NULL DEFAULT 'SINGBOX'
        CHECK (kind IN ('SINGBOX', 'RELAY', 'MIERU', 'REALM'))
);

INSERT INTO deployments_new
    (id, node_id, revision, config_sha256, status, started_at, finished_at,
     error_message, rollback_result, steps_json, created_at, kind)
SELECT id, node_id, revision, config_sha256, status, started_at, finished_at,
       error_message, rollback_result, steps_json, created_at, kind
  FROM deployments;

DROP TABLE deployments;
ALTER TABLE deployments_new RENAME TO deployments;

CREATE INDEX idx_deployments_node ON deployments(node_id, started_at);
