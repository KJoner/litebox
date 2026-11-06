-- VNext PERF06:后台任务。
--
-- 安装、下发、重启、导入这类长操作原来是同步 HTTP:浏览器要等十几秒到几分钟,
-- 中间任何一层超时(反代 60 秒、浏览器、移动网络切换)都会让一次成功的操作显示成失败。
-- 现在带 ?async=1 的请求立即返回 task_id,操作在后台跑完,结果(处理器本来要返回的 HTTP 状态
-- 与 JSON)记在这一行里,页面轮询它。
--
-- 运行中的任务在主控重启后标成 INTERRUPTED(待核对),**绝不自动重放** —— 那可能是一次
-- 已经换过配置、重启过服务的部署,重放会再重启一次;结果要去节点上核对。
CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT    NOT NULL,              -- 路由,例如 "POST /api/nodes/{id}/deploy"
    path        TEXT    NOT NULL,              -- 实际路径(不含查询串)
    target      TEXT    NOT NULL DEFAULT '',   -- 例如 "node:3",供按对象筛选
    status      TEXT    NOT NULL DEFAULT 'RUNNING'
        CHECK (status IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'INTERRUPTED')),
    progress    TEXT    NOT NULL DEFAULT '',   -- 最近一个阶段的说明
    http_status INTEGER NOT NULL DEFAULT 0,
    result_json TEXT    NOT NULL DEFAULT '',   -- 处理器本来要返回的响应体
    admin_id    INTEGER NOT NULL DEFAULT 0,
    idem_key    TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    finished_at TEXT
);
CREATE INDEX idx_tasks_created ON tasks(created_at);
CREATE INDEX idx_tasks_target ON tasks(target, created_at);
-- 同一个管理员用同一个幂等键提交两次,只跑一次(重复点击、网络重试)。
CREATE UNIQUE INDEX idx_tasks_idem ON tasks(admin_id, idem_key) WHERE idem_key != '';
