-- 公共探测目标库:来源、筛选维度、TCP 握手近似探测、单台节点的覆盖。
--
-- 0041 把出站探测做出来了,但公共目标只是一张手填的小表(上限 32 个)。这一版让它变成一个库:
--
--   probe_sources         目标从哪来:自定义 / Zstatic 节点目录 / TcpQuality 节点列表。
--                         第三方来源带授权确认、暂停开关、最小间隔下限与单目标全站限流。
--   probe_public_targets  加来源、上游标识、地区 / 省 / 市 / 运营商、TCP 方式、验证状态、
--                         上游快照与「管理员手工改过哪些字段」(同步时不覆盖)。
--   probe_targets         加 tcp_mode(序列的身份之一,与 protocol 一样创建后不可改)。
--   probe_node_settings   单台节点:暂停本机出站探测、覆盖探测间隔。
--
-- **只加列、加表,不重建任何一张已有的表。** 0041 重建过三张探测表,那是因为要改 CHECK;
-- 这一版要的东西都能用 ADD COLUMN 表达,探测明细与小时汇总里已有的行一行都不改。顺带的好处是回滚:
-- 旧版本的程序读写这几张表时不认识新列,而新列都有默认值,它照常能跑。
--
-- **迁移不创建任何公共目标,也不改任何已有目标的生效范围。** 三行来源只是来源的说明与限额,
-- 第三方来源默认「未确认授权」,在管理员确认之前它们的目标对任何节点都不会生效。
-- 已有的公共目标全部归到「自定义」、TCP 方式是 CONNECT —— 与升级前的含义一字不差。

CREATE TABLE probe_sources (
    kind TEXT PRIMARY KEY CHECK (kind IN ('MANUAL', 'ZSTATIC', 'TCPQUALITY')),
    name TEXT NOT NULL,
    -- 1 = 第三方来源(不是管理员自己的机器,也不是他另外取得授权的目标):受授权确认与限额约束。
    third_party INTEGER NOT NULL DEFAULT 1,
    -- 1 = 暂停:这个来源的全部目标对所有节点停测,历史保留。
    paused INTEGER NOT NULL DEFAULT 0,
    -- 管理员确认「已阅读来源说明并具备使用授权」的时间与人。空串 = 没确认过:
    -- 这个来源的目标对任何节点都不生效。这条记录只是管理员的声明,不代表来源方的授权。
    authorized_at   TEXT NOT NULL DEFAULT '',
    authorized_by   TEXT NOT NULL DEFAULT '',
    authorized_note TEXT NOT NULL DEFAULT '',
    -- 1 = 管理员声明已获得来源方的更高频率许可:最小间隔才允许调到 300 秒以下(最低 60)。
    high_freq_allowed INTEGER NOT NULL DEFAULT 0,
    -- 这个来源的目标的探测间隔下限(秒)。节点覆盖与目标自己的间隔都不能比它短。
    min_interval_sec INTEGER NOT NULL DEFAULT 300 CHECK (min_interval_sec BETWEEN 30 AND 3600),
    -- 单目标全站限流:同一个目标,全部节点合计每分钟最多多少次实际探测尝试(ICMP 按包、
    -- TCP 握手按次)。超出的延后执行,不记任何结果。
    per_target_per_min INTEGER NOT NULL DEFAULT 12 CHECK (per_target_per_min BETWEEN 1 AND 600),
    -- 最近一次从上游拉取的结果(只对能同步的来源有意义)。
    sync_state      TEXT NOT NULL DEFAULT 'NEVER' CHECK (sync_state IN ('NEVER', 'OK', 'ERROR')),
    last_sync_at    TEXT NOT NULL DEFAULT '',
    last_success_at TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    item_count      INTEGER NOT NULL DEFAULT 0,
    updated_at      TEXT NOT NULL DEFAULT '',
    -- 第三方来源:没有更高频率许可时下限至少 300 秒,有许可也至少 60 秒。
    CHECK (third_party = 0 OR min_interval_sec >= 60),
    CHECK (third_party = 0 OR high_freq_allowed = 1 OR min_interval_sec >= 300)
);

INSERT INTO probe_sources (kind, name, third_party, min_interval_sec, per_target_per_min) VALUES
    ('MANUAL',     '自定义目标',             0,  30, 600),
    ('ZSTATIC',    'Zstatic CDN 节点目录',   1, 300,  12),
    ('TCPQUALITY', 'TcpQuality 节点列表',    1, 300,  12);

-- 来源。不挂外键:SQLite 在 foreign_keys 打开时不允许 ADD COLUMN 带一个非 NULL 默认值的 REFERENCES。
ALTER TABLE probe_public_targets ADD COLUMN source TEXT NOT NULL DEFAULT 'MANUAL'
    CHECK (source IN ('MANUAL', 'ZSTATIC', 'TCPQUALITY'));
-- 上游给它的身份(Zstatic:prov:bj:ct:4 / city:sn-xian-cm:4;TcpQuality:cdn:4:北京:电信)。
-- 同步按它对上本地的那一行;自定义目标为空串。
ALTER TABLE probe_public_targets ADD COLUMN source_key TEXT NOT NULL DEFAULT '';
-- TCP 的两种方式:CONNECT = 一次完整建连(连接失败率);SYN = 短超时握手近似探测
-- (握手超时无应答率)。ICMP 行恒为 CONNECT、不用。与协议一样创建后不可改。
ALTER TABLE probe_public_targets ADD COLUMN tcp_mode TEXT NOT NULL DEFAULT 'CONNECT'
    CHECK (tcp_mode IN ('CONNECT', 'SYN'));
-- 筛选维度。地区是大区或国家(华北、海外……),运营商只收几个固定代码。
ALTER TABLE probe_public_targets ADD COLUMN region   TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_public_targets ADD COLUMN province TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_public_targets ADD COLUMN city     TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_public_targets ADD COLUMN isp      TEXT NOT NULL DEFAULT ''
    CHECK (isp IN ('', 'CT', 'CU', 'CM', 'CERNET', 'OTHER'));
-- 上游列表里的备用节点单独成行,默认不选。
ALTER TABLE probe_public_targets ADD COLUMN role TEXT NOT NULL DEFAULT 'PRIMARY'
    CHECK (role IN ('PRIMARY', 'BACKUP'));
-- 最近一次验证(主控解析 / 节点试测)的结论。
ALTER TABLE probe_public_targets ADD COLUMN verify_state TEXT NOT NULL DEFAULT 'UNVERIFIED'
    CHECK (verify_state IN ('UNVERIFIED', 'OK', 'WARN', 'FAILED'));
ALTER TABLE probe_public_targets ADD COLUMN verified_at   TEXT NOT NULL DEFAULT '';
ALTER TABLE probe_public_targets ADD COLUMN verify_detail TEXT NOT NULL DEFAULT '';
-- 上游最近一次给的原始值(JSON),同步时拿它比出「上游改了什么」。
ALTER TABLE probe_public_targets ADD COLUMN upstream TEXT NOT NULL DEFAULT '';
-- 导入之后管理员手工改过的字段(逗号分隔):同步不覆盖它们。
ALTER TABLE probe_public_targets ADD COLUMN manual_fields TEXT NOT NULL DEFAULT '';
-- 上游列表里不见了的时间。只标记,不自动删除、不自动停测。
ALTER TABLE probe_public_targets ADD COLUMN upstream_missing_since TEXT NOT NULL DEFAULT '';

-- 同一主机端口上,TCP 建连与 TCP 握手近似是两条不同的测量,允许并存。
DROP INDEX idx_probe_public_targets_key;
CREATE UNIQUE INDEX idx_probe_public_targets_key ON probe_public_targets(host, protocol, tcp_mode, port, family);
CREATE UNIQUE INDEX idx_probe_public_targets_source_key ON probe_public_targets(source, source_key)
    WHERE source_key != '';
CREATE INDEX idx_probe_public_targets_source ON probe_public_targets(source);

-- 序列跟着公共目标的 tcp_mode 走;入口序列恒为 CONNECT。
ALTER TABLE probe_targets ADD COLUMN tcp_mode TEXT NOT NULL DEFAULT 'CONNECT'
    CHECK (tcp_mode IN ('CONNECT', 'SYN'));

-- 参与 RTT 平均的样本数。0 = 等于 received(升级前的全部明细都是这样)。
-- TCP 握手近似里经过内核重发的那几次不取 RTT,样本数可能少于 received。
ALTER TABLE probe_results ADD COLUMN rtt_n INTEGER NOT NULL DEFAULT 0;

-- 单台节点的出站探测覆盖。没有行 = 不暂停、跟随各目标的间隔。
-- 只能暂停、不能强开:全局关闭与来源暂停对每一台都生效,这是收到投诉时一下子停掉全部探测的那个开关。
CREATE TABLE probe_node_settings (
    node_id      INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    paused       INTEGER NOT NULL DEFAULT 0,
    -- 0 = 跟随各目标自己的间隔;否则这台机器上全部出站目标都按它(仍受来源下限约束)。
    interval_sec INTEGER NOT NULL DEFAULT 0 CHECK (interval_sec = 0 OR interval_sec BETWEEN 30 AND 3600),
    updated_at   TEXT NOT NULL
);
