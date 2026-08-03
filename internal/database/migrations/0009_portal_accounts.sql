-- 普通用户的登录账号、会话与登录失败记录。
--
-- 与管理员完全分开的一套:表不同、Cookie 不同、中间件不同。
-- 不把普通用户塞进 admin_users —— 那张表的每一行都能进后台,
-- 一次判断写漏就等于把管理权限发给了代理用户。

CREATE TABLE portal_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 一个代理用户最多一个登录账号。
    proxy_user_id INTEGER NOT NULL UNIQUE REFERENCES proxy_users(id) ON DELETE CASCADE,
    -- 登录账号与用户显示名称分离:显示名称允许重复、允许随时改,
    -- 而登录账号是凭据的一半,必须全局唯一且稳定。
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,          -- argon2id,只存哈希
    -- login_enabled 是唯一能挡住门户登录的开关。
    -- 代理服务过期或超额的用户仍要能登录 —— 否则他连"为什么断了"都看不到,
    -- 只能来问管理员。
    login_enabled        INTEGER NOT NULL DEFAULT 1,
    must_change_password INTEGER NOT NULL DEFAULT 0,
    last_login_at        TEXT,
    last_login_ip        TEXT    NOT NULL DEFAULT '',
    created_at           TEXT    NOT NULL,
    updated_at           TEXT    NOT NULL
);

CREATE TABLE portal_sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT    NOT NULL UNIQUE,      -- 只存哈希,Cookie 里才是明文
    account_id INTEGER NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    client_ip    TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_portal_sessions_account ON portal_sessions(account_id);
CREATE INDEX idx_portal_sessions_expires ON portal_sessions(expires_at);

-- 门户登录的失败记录单独一张表,不与管理员的 login_attempts 混用:
-- 混在一起时,门户被撞库会连带把管理员登录锁死。
CREATE TABLE portal_login_attempts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    identifier   TEXT    NOT NULL,           -- 客户端 IP
    username     TEXT    NOT NULL DEFAULT '',
    succeeded    INTEGER NOT NULL DEFAULT 0,
    attempted_at TEXT    NOT NULL
);
CREATE INDEX idx_portal_login_attempts_lookup
    ON portal_login_attempts(identifier, attempted_at);
