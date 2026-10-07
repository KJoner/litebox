-- V20:供应商到期与续费管理 —— 自建节点、外部代理、代理源三类对象共用。
--
-- 三张表各管一件事:
--
--   expiry_profiles  每个对象一行:商家账单到期时间、提醒开关、商家是否自动续费、
--                    商家名称 / 续费页面、内部备注、按对象覆盖的提前天数
--   expiry_renewals  续费历史(谁、什么时候、从哪个到期时间改到哪个、怎么改的),
--                    带客户端生成的幂等键 —— 重复提交同一请求不能延长两次
--   expiry_notices   到期提醒的去重与发送状态,键是「对象 + 到期周期 + 阶段 + 渠道」
--
-- 刻意做成独立表而不是往三张对象表上各加六列:续费、提醒、去重的逻辑只写一遍,
-- 而三处各一份迟早分叉 —— 分叉的表现是节点提醒了而代理源没提醒,两边都不报错。
--
-- 这里存的是【供应商账单到期时间】,与已有的两种时间是三件事:
--   proxy_sources.expires_at / external_proxies.expires_at  面板控制的可用截止时间(到期即退出订阅)
--   proxy_sources.upstream_expires_at                       上游订阅报告的到期时间
-- 这一版只提醒,不碰那两列,也不停服务、不删节点、不关订阅。

CREATE TABLE expiry_profiles (
    kind             TEXT    NOT NULL CHECK (kind IN ('NODE', 'EXTERNAL_PROXY', 'PROXY_SOURCE')),
    object_id        INTEGER NOT NULL,
    -- RFC3339 UTC;空串表示「未设置」。界面上显示「未设置」,不能显示成「永不过期」——
    -- 商家那边没有"永不过期"这回事,空只说明管理员还没登记。
    expires_at       TEXT    NOT NULL DEFAULT '',
    reminder_enabled INTEGER NOT NULL DEFAULT 1,
    -- 「已在商家开启自动续费」:管理员登记的商家状态,不是面板代为扣款。
    -- 开着仍然提醒,只换一句文案(请确认余额及扣费结果)。
    auto_renew       INTEGER NOT NULL DEFAULT 0,
    vendor_name      TEXT    NOT NULL DEFAULT '',
    vendor_url       TEXT    NOT NULL DEFAULT '',
    note             TEXT    NOT NULL DEFAULT '',
    -- 覆盖全局的提前天数(逗号分隔,如 "14,3"),空串表示继承系统规则。
    lead_days        TEXT    NOT NULL DEFAULT '',
    updated_at       TEXT    NOT NULL,
    PRIMARY KEY (kind, object_id)
);

CREATE TABLE expiry_renewals (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind           TEXT    NOT NULL CHECK (kind IN ('NODE', 'EXTERNAL_PROXY', 'PROXY_SOURCE')),
    object_id      INTEGER NOT NULL,
    -- 客户端生成的幂等键。同一个键第二次提交原样返回第一次的记录,不再延长。
    request_id     TEXT    NOT NULL DEFAULT '',
    old_expires_at TEXT    NOT NULL DEFAULT '',
    new_expires_at TEXT    NOT NULL DEFAULT '',
    -- MONTHS:<n> / YEARS:<n> / ABSOLUTE / CLEAR
    method         TEXT    NOT NULL,
    -- 延长的基准:CURRENT(原到期时间)/ NOW(当前时间);ABSOLUTE 与 CLEAR 时为空。
    base           TEXT    NOT NULL DEFAULT '',
    admin_user_id  INTEGER REFERENCES admin_users(id) ON DELETE SET NULL,
    note           TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL
);
CREATE UNIQUE INDEX idx_expiry_renewals_request ON expiry_renewals(request_id) WHERE request_id <> '';
CREATE INDEX idx_expiry_renewals_object ON expiry_renewals(kind, object_id, created_at);

CREATE TABLE expiry_notices (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    kind              TEXT    NOT NULL,
    object_id         INTEGER NOT NULL,
    -- 这一轮到期周期的到期时间。续费之后到期时间变了,键也变了,提醒从头来。
    period_expires_at TEXT    NOT NULL,
    -- D<天数>(提前 N 天)或 OVERDUE(已到期未确认)。
    stage             TEXT    NOT NULL,
    channel           TEXT    NOT NULL,
    -- PENDING 已加入发送队列;SENT 实际发送成功;FAILED 重试用尽;CANCELLED 续费后作废。
    -- 「已加入队列」与「发送成功」必须分开:先标成功会让一条发不出去的提醒永久漏发。
    status            TEXT    NOT NULL CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'CANCELLED')),
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   TEXT    NOT NULL DEFAULT '',
    last_error        TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL,
    sent_at           TEXT    NOT NULL DEFAULT '',
    UNIQUE (kind, object_id, period_expires_at, stage, channel)
);
CREATE INDEX idx_expiry_notices_pending ON expiry_notices(status, next_attempt_at);
