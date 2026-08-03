-- 管理员对用户做的人工续期与额度调整记录。
--
-- 系统没有支付与订单,这张表只回答"是谁、在什么时候、给谁加了多少流量
-- 或延了多少天"。刻意不叫"充值记录" —— 没有真实支付,那个词会让用户
-- 以为自己付过钱。
--
-- 与 audit_logs 的分工:审计日志记的是"发生了什么操作",面向排查;
-- 这张表记的是"额度与期限怎么变的",面向对账,而且其中一部分要给用户看。
CREATE TABLE user_adjustments (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    proxy_user_id INTEGER NOT NULL REFERENCES proxy_users(id) ON DELETE CASCADE,
    action        TEXT    NOT NULL
        CHECK (action IN ('ADD_QUOTA','SET_QUOTA','RESET_TRAFFIC','EXTEND_EXPIRY',
                          'SET_EXPIRY','CHANGE_TIER','ENABLE_USER','DISABLE_USER')),
    -- 增量字段。设为绝对值的操作(SET_QUOTA/SET_EXPIRY)这里记 0,
    -- 变化前后的具体值在 before_json / after_json 里。
    quota_delta_bytes INTEGER NOT NULL DEFAULT 0,
    expiry_delta_days INTEGER NOT NULL DEFAULT 0,
    before_json       TEXT    NOT NULL DEFAULT '{}',
    after_json        TEXT    NOT NULL DEFAULT '{}',
    -- remark 是唯一对用户公开的文字。管理员的内部说明不要写在这里。
    remark        TEXT    NOT NULL DEFAULT '',
    admin_user_id INTEGER REFERENCES admin_users(id) ON DELETE SET NULL,
    created_at    TEXT    NOT NULL
);
CREATE INDEX idx_user_adjustments_user ON user_adjustments(proxy_user_id, created_at);
CREATE INDEX idx_user_adjustments_time ON user_adjustments(created_at);
