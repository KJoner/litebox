-- litebox:foreign-keys-off
--
-- VNext TRAFFIC08(复核批次三):用户额度周期多一种「固定 N 天」,并带时区与锚点。
--
-- proxy_users.reset_cycle 的 CHECK 只收 NONE / MONTHLY,要多收 DAYS 只能重建整张表。
-- 这张表被五张表引用(portal_accounts、user_adjustments、user_external_proxies、user_inbound_grants、
-- user_nodes,全部 ON DELETE CASCADE),所以按 SQLite 官方的通用改表流程做:迁移框架看到第一行的标记,
-- 在一条专用连接上关掉外键、打开 legacy_alter_table 再执行,提交之前跑 foreign_key_check
-- (见 database.applyMigrationFKOff)。**外键开着的话,DROP TABLE 会把这五张表里的行级联删光。**
--
-- 列的顺序与重建前逐列一致(包括后来 ADD COLUMN 加的那些),新列放在最后;取值一个都不变,
-- 用户的身份(id、user_code)、凭据密文、门户账号、授权、等级与账本一样都不动。
-- 依赖这张表的视图按表名引用它、而新表是旧表的超集,不需要重建(database/user_reset_days_upgrade_test.go
-- 逐行比对升级前后的 proxy_users、引用它的五张表与全部有效权限视图)。
--
-- 新列:
--   reset_period_days  DAYS 时的周期天数(1–3650),其余为 0;
--   reset_anchor_at    DAYS 时第一个周期的起点(RFC3339);空串表示从创建那天的零点起算;
--   reset_timezone     周期边界按哪个时区的零点切(IANA 名);存量一律 UTC —— 与升级前的行为逐字节相同。
-- reset_day 不再限于 1–28:1–31,超出当月天数时落到当月最后一天(与节点周期同一条规矩),由 Go 侧校验。
CREATE TABLE proxy_users_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    user_code      TEXT    NOT NULL UNIQUE,
    display_name   TEXT    NOT NULL,
    remark         TEXT    NOT NULL DEFAULT '',
    uuid_encrypted TEXT    NOT NULL,
    status         TEXT    NOT NULL DEFAULT 'ACTIVE'
        CHECK (status IN ('ACTIVE','DISABLED','EXPIRED','QUOTA_EXCEEDED',
                          'DEPLOY_PENDING','DEPLOY_FAILED')),
    quota_bytes    INTEGER NOT NULL DEFAULT 0,
    used_uplink    INTEGER NOT NULL DEFAULT 0,
    used_downlink  INTEGER NOT NULL DEFAULT 0,
    expires_at     TEXT,
    -- 这次重建唯一放宽的约束:多收一个 DAYS。
    reset_cycle    TEXT    NOT NULL DEFAULT 'NONE'
        CHECK (reset_cycle IN ('NONE','MONTHLY','DAYS')),
    reset_day      INTEGER NOT NULL DEFAULT 1,
    last_reset_at  TEXT,
    sub_token_hash TEXT    NOT NULL UNIQUE,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    deleted_at     TEXT,
    sub_token_encrypted TEXT NOT NULL DEFAULT '',
    sub_last_access_at TEXT,
    sub_last_access_ip TEXT NOT NULL DEFAULT '',
    sub_last_user_agent TEXT NOT NULL DEFAULT '',
    sub_access_count INTEGER NOT NULL DEFAULT 0,
    access_tier_id INTEGER NOT NULL DEFAULT 1,
    ss_password_encrypted TEXT NOT NULL DEFAULT '',
    mieru_password_encrypted TEXT NOT NULL DEFAULT '',
    snell_password_encrypted TEXT NOT NULL DEFAULT '',
    access_mode TEXT NOT NULL DEFAULT 'INHERIT'
        CHECK (access_mode IN ('INHERIT', 'DIRECT_ONLY')),
    billing_mode TEXT NOT NULL DEFAULT 'SUM'
        CHECK (billing_mode IN ('SUM', 'UP', 'DOWN', 'MAX')),
    billing_multiplier_pct INTEGER NOT NULL DEFAULT 100
        CHECK (billing_multiplier_pct BETWEEN 1 AND 10000),
    billing_rule_version INTEGER NOT NULL DEFAULT 1,
    billing_next_mode TEXT NOT NULL DEFAULT ''
        CHECK (billing_next_mode IN ('', 'SUM', 'UP', 'DOWN', 'MAX')),
    billing_next_multiplier_pct INTEGER NOT NULL DEFAULT 0,
    billing_sealed_bytes INTEGER NOT NULL DEFAULT 0,
    billing_base_uplink INTEGER NOT NULL DEFAULT 0,
    billing_base_downlink INTEGER NOT NULL DEFAULT 0,
    billing_changed_at TEXT NOT NULL DEFAULT '',
    reset_period_days INTEGER NOT NULL DEFAULT 0
        CHECK (reset_period_days BETWEEN 0 AND 3650),
    reset_anchor_at TEXT NOT NULL DEFAULT '',
    reset_timezone TEXT NOT NULL DEFAULT 'UTC'
);

INSERT INTO proxy_users_new (
    id, user_code, display_name, remark, uuid_encrypted, status, quota_bytes, used_uplink, used_downlink,
    expires_at, reset_cycle, reset_day, last_reset_at, sub_token_hash, created_at, updated_at, deleted_at,
    sub_token_encrypted, sub_last_access_at, sub_last_access_ip, sub_last_user_agent, sub_access_count,
    access_tier_id, ss_password_encrypted, mieru_password_encrypted, snell_password_encrypted, access_mode,
    billing_mode, billing_multiplier_pct, billing_rule_version, billing_next_mode, billing_next_multiplier_pct,
    billing_sealed_bytes, billing_base_uplink, billing_base_downlink, billing_changed_at)
SELECT
    id, user_code, display_name, remark, uuid_encrypted, status, quota_bytes, used_uplink, used_downlink,
    expires_at, reset_cycle, reset_day, last_reset_at, sub_token_hash, created_at, updated_at, deleted_at,
    sub_token_encrypted, sub_last_access_at, sub_last_access_ip, sub_last_user_agent, sub_access_count,
    access_tier_id, ss_password_encrypted, mieru_password_encrypted, snell_password_encrypted, access_mode,
    billing_mode, billing_multiplier_pct, billing_rule_version, billing_next_mode, billing_next_multiplier_pct,
    billing_sealed_bytes, billing_base_uplink, billing_base_downlink, billing_changed_at
FROM proxy_users;

DROP TABLE proxy_users;
ALTER TABLE proxy_users_new RENAME TO proxy_users;

CREATE INDEX idx_proxy_users_deleted ON proxy_users(deleted_at);
CREATE INDEX idx_proxy_users_status  ON proxy_users(status);
CREATE INDEX idx_proxy_users_tier ON proxy_users(access_tier_id);
