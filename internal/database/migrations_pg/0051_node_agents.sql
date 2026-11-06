-- (PostgreSQL 版,与 SQLite 的 0051 同一份逻辑结构)
-- VNext AGENT01–06:可选的节点 Agent(这一版只做状态采集)。
--
-- 每台机器至多一行。state:
--   PENDING  已装上、还没收到第一次上报(握手没完成)—— 资源采集仍走 SSH;
--   ACTIVE   收到过上报 —— 资源采集改由 Agent 写,SSH 那一路对这台机器让路(写入者切换);
--   REMOVED  已卸载,令牌作废 —— 回到 SSH。
-- 令牌只存 SHA-256(与订阅 Token 同一条规矩),明文只在安装那一刻写进节点上 0600 的配置文件。
-- (last_boot_id, last_seq) 是去重的依据:重传、主控重启、网络重试都不会让同一个样本入两次库 ——
-- 判据在接收那一个事务里,带条件更新,不靠先查后写。

CREATE TABLE node_agents (
    node_id       BIGINT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    state         TEXT    NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'ACTIVE', 'REMOVED')),
    token_hash    TEXT    NOT NULL DEFAULT '',
    version       TEXT    NOT NULL DEFAULT '',
    last_boot_id  TEXT    NOT NULL DEFAULT '',
    last_seq      BIGINT NOT NULL DEFAULT 0,
    last_seen_at  TEXT    NOT NULL DEFAULT '',
    rss_kb        BIGINT NOT NULL DEFAULT 0,
    dropped_total BIGINT NOT NULL DEFAULT 0,
    last_error    TEXT    NOT NULL DEFAULT '',
    installed_at  TEXT    NOT NULL DEFAULT '',
    updated_at    TEXT    NOT NULL
);
CREATE INDEX idx_node_agents_token ON node_agents(token_hash);
