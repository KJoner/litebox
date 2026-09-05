-- V17:阿里云 CDT 主机 —— 账号、实例绑定、用量样本与开关机记录。
--
-- CDT(云数据传输)的计数器是【账号级 × 业务区域】的,不是实例级的:
-- ListCdtInternetTraffic 返回这个阿里云账号在每个 BusinessRegionId 下本月累计的
-- 公网流量,里面没有实例维度,同一个账号下两台实例共用同一个池子。所以:
--
--   cloud_accounts        一个 AccessKey 一行,额度与阈值挂在这里(按国际 / 内地两个池子)
--   cloud_account_state   每账号最近一次采样(两个池子的用量、上次成功时间、连续失败次数)
--   cloud_traffic_samples 小时点,画月内累计曲线用
--   cloud_nodes           节点 → 实例的绑定,以及这台实例的运行态(状态、IP、被谁停的)
--   cloud_action_marks    动作去重键(INSERT OR IGNORE 拿到才动手)
--   cloud_power_events    开关机动作的记录(给页面与排查看,不承担去重)
--
-- 绑定单独一张表而不是往 nodes 上加十几列:云相关的判断只看「有没有这一行」,
-- nodes 那一侧一个字节不动,存量节点的行为逐字节不变。

CREATE TABLE cloud_accounts (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    name                        TEXT    NOT NULL,
    provider                    TEXT    NOT NULL DEFAULT 'ALIYUN' CHECK (provider IN ('ALIYUN')),
    access_key_id               TEXT    NOT NULL,
    -- 主密钥加密。它是别人家整个云账号的钥匙,与节点 root 口令同级。
    access_key_secret_encrypted TEXT    NOT NULL,
    -- 两个池子的免费额度,按字节。默认值是阿里云国际站的 200 GB / 20 GB
    -- (按 GiB 存:阿里云控制台按 1024 进制显示)。0 表示不限,那时阈值不生效。
    cdt_quota_intl_bytes        INTEGER NOT NULL DEFAULT 214748364800,
    cdt_quota_cn_bytes          INTEGER NOT NULL DEFAULT 21474836480,
    -- 用量达到额度的百分之几算「超阈值」。CDT 的数据有延迟,留余量的意义就在这里。
    threshold_percent           INTEGER NOT NULL DEFAULT 90 CHECK (threshold_percent BETWEEN 1 AND 100),
    enabled                     INTEGER NOT NULL DEFAULT 1,
    created_at                  TEXT    NOT NULL,
    updated_at                  TEXT    NOT NULL
);

CREATE TABLE cloud_account_state (
    account_id           INTEGER PRIMARY KEY REFERENCES cloud_accounts(id) ON DELETE CASCADE,
    intl_bytes           INTEGER NOT NULL DEFAULT 0,
    cn_bytes             INTEGER NOT NULL DEFAULT 0,
    -- sampled_at 是上一次【成功】采样的时间;失败不动它 —— 界面上「数据是几点的」
    -- 与「上次的错」都从这一行来,漏写一边就是"面板说一切正常而数据停在三天前"。
    sampled_at           TEXT    NOT NULL DEFAULT '',
    last_error           TEXT    NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT    NOT NULL
);

-- 只存小时点、不存日点:月内累计值本来就是单调的,日用量由相邻两天的最后一个
-- 小时点相减得出;跨月那一刻计数器归零,曲线断开重画。不删:一个账号一年两万行。
CREATE TABLE cloud_traffic_samples (
    account_id INTEGER NOT NULL REFERENCES cloud_accounts(id) ON DELETE CASCADE,
    class      TEXT    NOT NULL CHECK (class IN ('INTL', 'CN')),
    bucket_ts  INTEGER NOT NULL,
    bytes      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, class, bucket_ts)
);

CREATE TABLE cloud_nodes (
    node_id            INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    -- RESTRICT:还有节点绑着的账号不能删。级联清空会让一台机器悄悄不再受监控。
    account_id         INTEGER NOT NULL REFERENCES cloud_accounts(id) ON DELETE RESTRICT,
    region_id          TEXT    NOT NULL,
    instance_id        TEXT    NOT NULL,
    -- 超阈值时对这台实例做什么。默认只通知:停机会断掉这台机器上全部用户,必须显式选。
    threshold_action   TEXT    NOT NULL DEFAULT 'NOTIFY' CHECK (threshold_action IN ('NOTIFY', 'STOP')),
    -- 关机时的计费模式。节省停机会释放【系统分配】的公网 IP(EIP 不受影响),
    -- 开机后换地址 —— 而订阅里写的正是那个地址。表单上要把这句话写出来。
    stopped_mode       TEXT    NOT NULL DEFAULT 'StopCharging' CHECK (stopped_mode IN ('KeepCharging', 'StopCharging')),
    schedule_enabled   INTEGER NOT NULL DEFAULT 0,
    start_time         TEXT    NOT NULL DEFAULT '',   -- "HH:MM",按 cloud_timezone 解释
    stop_time          TEXT    NOT NULL DEFAULT '',
    keepalive          INTEGER NOT NULL DEFAULT 0,

    -- 以下是运行态,只由引擎写。
    instance_status    TEXT    NOT NULL DEFAULT '',   -- 阿里云的原文:Running / Stopped / ...,空表示还没查过
    status_at          TEXT    NOT NULL DEFAULT '',
    public_ip          TEXT    NOT NULL DEFAULT '',   -- 最近一次看到的对外地址(有 EIP 用 EIP)
    has_eip            INTEGER NOT NULL DEFAULT 0,
    spot               INTEGER NOT NULL DEFAULT 0,    -- 抢占式实例
    charge_type        TEXT    NOT NULL DEFAULT '',
    -- 这台机器停着是面板干的还是别人干的。只由面板的三种停机动作写入,开机成功时清空。
    -- 巡检、流量同步、保活三处都看它:面板按阈值停掉的机器,保活不该把它拉起来;
    -- 管理员在阿里云控制台手停的机器(这一列为空、状态 Stopped),保活才该管。
    stopped_by         TEXT    NOT NULL DEFAULT '' CHECK (stopped_by IN ('', 'THRESHOLD', 'SCHEDULE', 'MANUAL')),
    stopped_at         TEXT    NOT NULL DEFAULT '',
    last_error         TEXT    NOT NULL DEFAULT '',
    -- 保活连续失败次数与下次允许重试的时间,用于退避:节省停机后库存不足(NoStock)
    -- 是常态,每轮捅一次换不来任何东西,只会让日志和推送里全是同一条消息。
    keepalive_failures INTEGER NOT NULL DEFAULT 0,
    keepalive_retry_at TEXT    NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL
);
CREATE INDEX idx_cloud_nodes_account ON cloud_nodes(account_id);
-- 同一台实例只能绑一个节点:两个节点各自按自己的规则开关同一台机器,
-- 结果是它被反复开开关关,而两边各自看起来都对。
CREATE UNIQUE INDEX idx_cloud_nodes_instance ON cloud_nodes(account_id, instance_id);

-- 去重键。阈值 threshold-stop:<node>:<yyyymm>、threshold-notify:<account>:<class>:<yyyymm>,
-- 定时 schedule:<node>:<yyyymmdd>:<start|stop>。面板重启、两轮重叠都不会重复发同一条命令;
-- 用量回落到阈值之下时删掉当月的阈值键,让「额度改大之后再次超过」能再触发一次。
CREATE TABLE cloud_action_marks (
    mark_key   TEXT    PRIMARY KEY,
    node_id    INTEGER NOT NULL DEFAULT 0,
    account_id INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
);

CREATE TABLE cloud_power_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    account_id INTEGER NOT NULL DEFAULT 0,
    kind       TEXT    NOT NULL CHECK (kind IN (
        'THRESHOLD_STOP', 'SCHEDULE_START', 'SCHEDULE_STOP',
        'KEEPALIVE_START', 'MANUAL_START', 'MANUAL_STOP')),
    status     TEXT    NOT NULL CHECK (status IN ('SENT', 'FAILED', 'SKIPPED')),
    detail     TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL
);
CREATE INDEX idx_cloud_power_events_node ON cloud_power_events(node_id, id DESC);
