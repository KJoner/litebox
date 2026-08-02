-- LiteBox 初始表结构。
--
-- 约定:
--   * 所有时间列存 UTC 的 RFC3339 字符串;
--   * 所有流量列单位为字节;
--   * 需要还原的敏感字段(用户 UUID、节点 REALITY 私钥)存主密钥加密后的密文;
--   * 订阅 Token 只存 SHA-256 哈希。

-- ---------- 管理员 ----------

CREATE TABLE admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,          -- argon2id
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    last_login_at TEXT
);

CREATE TABLE admin_sessions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash    TEXT    NOT NULL UNIQUE,   -- 只存哈希,Cookie 里才是明文
    admin_user_id INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    created_at    TEXT    NOT NULL,
    expires_at    TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL,
    client_ip     TEXT    NOT NULL DEFAULT '',
    user_agent    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_admin_sessions_expires ON admin_sessions(expires_at);
CREATE INDEX idx_admin_sessions_user    ON admin_sessions(admin_user_id);

-- 登录失败记录,用于限流与锁定。
CREATE TABLE login_attempts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    identifier  TEXT NOT NULL,               -- 客户端 IP
    username    TEXT NOT NULL DEFAULT '',
    succeeded   INTEGER NOT NULL DEFAULT 0,
    attempted_at TEXT NOT NULL
);
CREATE INDEX idx_login_attempts_lookup ON login_attempts(identifier, attempted_at);

-- ---------- 代理用户 ----------

CREATE TABLE proxy_users (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    -- user_code 是流量统计的唯一标识,格式 user_000001,一经分配不可变更。
    -- 不使用用户可修改的显示名,否则改名会导致统计断裂。
    user_code      TEXT    NOT NULL UNIQUE,
    display_name   TEXT    NOT NULL,
    remark         TEXT    NOT NULL DEFAULT '',
    uuid_encrypted TEXT    NOT NULL,         -- 主密钥加密后的 VLESS UUID
    status         TEXT    NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE','DISABLED','EXPIRED','QUOTA_EXCEEDED',
                          'DEPLOY_PENDING','DEPLOY_FAILED')),
    quota_bytes    INTEGER NOT NULL DEFAULT 0,   -- 0 表示不限额
    used_uplink    INTEGER NOT NULL DEFAULT 0,
    used_downlink  INTEGER NOT NULL DEFAULT 0,
    expires_at     TEXT,                          -- NULL 表示不过期
    reset_cycle    TEXT    NOT NULL DEFAULT 'NONE'
        CHECK (reset_cycle IN ('NONE','MONTHLY')),
    reset_day      INTEGER NOT NULL DEFAULT 1,    -- MONTHLY 时的重置日 1-28
    last_reset_at  TEXT,
    sub_token_hash TEXT    NOT NULL UNIQUE,       -- 订阅 Token 的 SHA-256
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    deleted_at     TEXT                            -- 软删除
);
CREATE INDEX idx_proxy_users_status  ON proxy_users(status);
CREATE INDEX idx_proxy_users_deleted ON proxy_users(deleted_at);

-- ---------- 节点 ----------

CREATE TABLE nodes (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    name               TEXT    NOT NULL UNIQUE,
    host               TEXT    NOT NULL,
    ssh_port           INTEGER NOT NULL DEFAULT 22,
    ssh_user           TEXT    NOT NULL DEFAULT 'root',
    ssh_key_encrypted  TEXT    NOT NULL DEFAULT '',   -- 主密钥加密后的私钥
    proxy_port         INTEGER NOT NULL,              -- VLESS 监听端口
    api_port           INTEGER NOT NULL DEFAULT 28080,-- 节点上 V2Ray API 的回环端口
    arch               TEXT    NOT NULL DEFAULT '',
    singbox_version    TEXT    NOT NULL DEFAULT '',
    singbox_build_tags TEXT    NOT NULL DEFAULT '',   -- 必须含 with_v2ray_api

    -- REALITY 参数
    reality_dest       TEXT    NOT NULL,              -- 握手目标域名
    reality_dest_port  INTEGER NOT NULL DEFAULT 443,
    reality_privkey_encrypted TEXT NOT NULL,
    reality_pubkey     TEXT    NOT NULL,
    reality_short_id   TEXT    NOT NULL,

    -- Phase 0 发现:REALITY 要求握手目标返回的每个 TLS 记录 <= 8192 字节,
    -- 超限时握手静默失败。必须在节点本机实测并记录结果。
    handshake_max_record_size INTEGER NOT NULL DEFAULT 0,
    handshake_checked_at      TEXT,

    status             TEXT    NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','ONLINE','OFFLINE','DISABLED','DEPLOY_FAILED')),
    last_heartbeat_at  TEXT,
    last_deploy_id     INTEGER,
    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL,
    deleted_at         TEXT
);
CREATE INDEX idx_nodes_status ON nodes(status);

-- 用户与节点的分配关系
CREATE TABLE user_nodes (
    proxy_user_id INTEGER NOT NULL REFERENCES proxy_users(id) ON DELETE CASCADE,
    node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    created_at    TEXT    NOT NULL,
    PRIMARY KEY (proxy_user_id, node_id)
);
CREATE INDEX idx_user_nodes_node ON user_nodes(node_id);

-- ---------- 流量统计 ----------

-- 节点上 sing-box 进程的实例信息。
-- Phase 0 发现:仅靠"计数器变小"判定重启会漏算整个重启前的计数值,
-- 必须用 GetSysStats.Uptime 反推进程启动时刻独立判定换代。
CREATE TABLE node_instances (
    node_id      INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    started_at   INTEGER NOT NULL DEFAULT 0,   -- Unix 秒,由 uptime 反推
    last_sync_at INTEGER NOT NULL DEFAULT 0,   -- Unix 秒
    updated_at   TEXT    NOT NULL
);

-- 各节点各用户各方向的计数器基线。
CREATE TABLE node_counters (
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_code  TEXT    NOT NULL,
    direction  TEXT    NOT NULL CHECK (direction IN ('uplink','downlink')),
    last_value INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (node_id, user_code, direction)
);

-- 追加式流量流水。用户总量由此累加而来,不直接采信节点当前计数器。
CREATE TABLE traffic_ledger (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id      TEXT    NOT NULL,            -- 同一次同步的幂等标识
    node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_code     TEXT    NOT NULL,
    direction     TEXT    NOT NULL CHECK (direction IN ('uplink','downlink')),
    delta_bytes   INTEGER NOT NULL CHECK (delta_bytes >= 0),
    counter_value INTEGER NOT NULL,            -- 入账时的节点计数器绝对值
    created_at    TEXT    NOT NULL
);
-- 同一批次重复写入会被数据库拒绝,提供幂等保证。
CREATE UNIQUE INDEX idx_traffic_ledger_batch
    ON traffic_ledger(batch_id, node_id, user_code, direction);
CREATE INDEX idx_traffic_ledger_user ON traffic_ledger(user_code, created_at);
CREATE INDEX idx_traffic_ledger_node ON traffic_ledger(node_id, created_at);

-- 每日聚合,供趋势图与报表使用。
CREATE TABLE traffic_daily (
    day       TEXT    NOT NULL,               -- UTC 日期 YYYY-MM-DD
    user_code TEXT    NOT NULL,
    node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    uplink    INTEGER NOT NULL DEFAULT 0,
    downlink  INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT   NOT NULL,
    PRIMARY KEY (day, user_code, node_id)
);
CREATE INDEX idx_traffic_daily_day ON traffic_daily(day);

-- ---------- 部署 ----------

CREATE TABLE deployments (
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
    steps_json    TEXT    NOT NULL DEFAULT '[]',  -- 各步骤耗时与输出
    created_at    TEXT    NOT NULL
);
CREATE INDEX idx_deployments_node ON deployments(node_id, started_at);

-- ---------- 审计与设置 ----------

CREATE TABLE audit_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    admin_user_id INTEGER REFERENCES admin_users(id) ON DELETE SET NULL,
    action        TEXT    NOT NULL,
    target_type   TEXT    NOT NULL DEFAULT '',
    target_id     TEXT    NOT NULL DEFAULT '',
    detail        TEXT    NOT NULL DEFAULT '',
    client_ip     TEXT    NOT NULL DEFAULT '',
    succeeded     INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT    NOT NULL
);
CREATE INDEX idx_audit_logs_created ON audit_logs(created_at);
CREATE INDEX idx_audit_logs_action  ON audit_logs(action, created_at);

CREATE TABLE system_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
