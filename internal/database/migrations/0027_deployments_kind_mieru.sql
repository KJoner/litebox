-- 部署记录的种类加上 MIERU(V13 漏掉的)。
--
-- 迁移 0018 给 deployments 加 kind 时,一台机器上只有两种下发:
-- sing-box 配置(重启服务)与 nginx 转发(reload)。V13 加了第三种 ——
-- 一个 Mieru 入口的下发,重启的是那一个 mita 实例 —— 但那条 CHECK
-- 没跟着改。
--
-- 后果不是"少一种分类",而是**每一次 Mieru 下发都没有记录**:
--
--   constraint failed: CHECK constraint failed: kind IN ('SINGBOX', 'RELAY')
--
-- 部署本身照常跑完(成功或失败都跑完了),只有落库那一步失败。
-- 于是部署记录页上一条 Mieru 的记录都没有,管理员只能去翻 journalctl ——
-- 而部署记录才是那份带访问控制、带完整步骤与节点日志原文的东西。
-- 生产上撞到了,报错就在 sing-box 部署失败的日志旁边,看起来像是两件事。
--
-- **这一条与「一次部署的结局必须落进系统日志」是配套的**:那一条保证了
-- 至少有一行日志,这一条保证了记录本身存在。少了后者,前者就成了唯一的
-- 线索,而它按设计只写失败步骤名与错误的第一行。
--
-- ---------- 为什么必须重建表 ----------
--
-- SQLite 改不了 CHECK 约束,只能建新表再搬。历史部署记录要**全量搬过来**:
-- 它是排查"这台机器上次什么时候被动过、动了什么"唯一的依据,
-- 丢了就再也补不回来。

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
    -- 默认仍是 SINGBOX:存量记录在 0018 之前全是 sing-box 部署。
    kind          TEXT    NOT NULL DEFAULT 'SINGBOX'
        CHECK (kind IN ('SINGBOX', 'RELAY', 'MIERU'))
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
