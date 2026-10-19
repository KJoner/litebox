-- Boce 外部入站拨测与卡片展示自定义(方案见 docs/asset-probe/06-boce-plan.md)。
--
-- Boce 是付费的第三方拨测服务:从它在各省各运营商(以及海外)的拨测点对节点的入口做 TCPing。
-- 它回答的是「从外面的某个地区连这台机器的这个端口怎么样」,与主控的入口连通性同属入站方向、
-- 但来源不同(source = BOCE),与节点本机的出站探测更是两回事。
--
-- **复用探测的序列模型**:每个「Boce 任务 × 拨测点」就是 probe_targets 里的一条入站序列,
-- 结果进 probe_results / probe_hourly —— 点阵、历史、换代、告警、保留期全部沿用,不另起一套历史。
-- 供应商自己的字段(任务 ID、拨测点 ID、原始错误、解析到的 IP)另存 boce_results,
-- 它的主键 (run_id, vantage_id) 就是「Boce 任务 ID + 拨测点 ID」的去重键。
--
-- SQLite 改不了 CHECK,三张探测表只能重建,顺序照 0041:先建新表并全量搬数据,
-- 再【先删两张子表、后删父表】,最后改名。反过来先删父表的话,foreign_keys 开着,
-- DROP TABLE 的隐式 DELETE 会顺着 ON DELETE CASCADE 把全部历史结果与小时汇总一起删掉,
-- 而迁移照样成功、一个错都不报。
--
-- **迁移不创建任何 Boce 任务、不打开任何开关**:升级之后一次 Boce 请求都不会发。
-- 已有的入口目标、出站序列与它们的历史一行不动。

-- ---------- Boce 拨测点缓存 ----------
-- 节点列表缓存在库里,打开页面不去请求 Boce。上游列表里消失的只标 missing_since,不删:
-- 任务里选着它的话,要能看出「这个拨测点已经不在列表里了」,而不是悄悄换成另一个付费拨测点。
CREATE TABLE boce_vantages (
    -- Boce 的节点 ID(上游给的,不是自增)
    id            INTEGER PRIMARY KEY,
    area          TEXT    NOT NULL CHECK (area IN ('CN', 'OVERSEA')),
    -- 地区与运营商只存接口实际返回的值,不臆造
    node_name     TEXT    NOT NULL,
    isp_name      TEXT    NOT NULL DEFAULT '',
    isp_code      INTEGER NOT NULL DEFAULT 0,
    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL,
    missing_since TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_boce_vantages_area ON boce_vantages(area);

-- ---------- 任务定义 ----------
-- 一个任务 = 一台节点上的一个入口 + 若干拨测点 + 一张时间表。改任务、暂停任务都不删历史;
-- 归档的任务不再执行,序列与历史保留;只有显式删除才连历史一起删。
CREATE TABLE boce_tasks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    check_type TEXT    NOT NULL DEFAULT 'TCPING' CHECK (check_type IN ('TCPING')),
    -- 被测入口的引用(EP:<订阅地址条目> / ENTRY:<种类>:<入口>),与入口连通性同一份白名单;
    -- 每轮按引用解析出当前的地址与端口,变了就换代。创建后不可改 —— 换目标就是另一条测量。
    ref        TEXT    NOT NULL,
    -- Boce 的 TCPing 文档没有说明支持 IPv6:代码只放 V4,列上留着 V6 是给以后核实了再放开
    family     TEXT    NOT NULL DEFAULT 'V4' CHECK (family IN ('V4', 'V6')),
    -- 选中的拨测点 ID(JSON 数组,有序)
    vantage_ids TEXT   NOT NULL DEFAULT '[]',
    -- INTERVAL = 固定间隔(从运行窗口起点对齐);TIMES = 固定时间点(窗口不参与)
    schedule_mode TEXT NOT NULL DEFAULT 'INTERVAL' CHECK (schedule_mode IN ('INTERVAL', 'TIMES')),
    interval_min  INTEGER NOT NULL DEFAULT 60 CHECK (interval_min BETWEEN 10 AND 1440),
    -- TIMES 模式的时间点,"08:00,12:00"
    times         TEXT    NOT NULL DEFAULT '',
    -- 运行窗口,end 可以是 24:00;end <= start 表示跨午夜
    window_start  TEXT    NOT NULL DEFAULT '08:00',
    window_end    TEXT    NOT NULL DEFAULT '24:00',
    -- 星期掩码:bit0 = 周一 … bit6 = 周日
    weekdays      INTEGER NOT NULL DEFAULT 127 CHECK (weekdays BETWEEN 1 AND 127),
    timezone      TEXT    NOT NULL DEFAULT 'Asia/Shanghai',
    -- 单任务每月波点上限,0 = 只受全局预算约束
    month_points_limit INTEGER NOT NULL DEFAULT 0 CHECK (month_points_limit >= 0),
    -- 自动检测开关,默认关:管理员确认费用之后再打开
    enabled    INTEGER NOT NULL DEFAULT 0,
    -- 连续几次被 Boce 明确拒绝(参数错误等)之后自动停下的原因;非空 = 自动检测已停,要管理员重新打开
    auto_paused_reason   TEXT    NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    next_run_at TEXT NOT NULL DEFAULT '',
    last_run_at TEXT NOT NULL DEFAULT '',
    last_state  TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    archived_at TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_boce_tasks_node ON boce_tasks(node_id);

-- ---------- 每一轮执行(= 一次 Boce 任务) ----------
-- idem_key 是这一轮的幂等标识:定时 S:<任务>:<时间槽>,手动 M:<任务>:<请求 ID>。
-- 预算预留与插入这一行在同一个事务里;发请求之前它已经是 CREATING —— 面板在那一刻重启,
-- 恢复时记 UNKNOWN,不再重发:不知道发没发出去的请求,重发就可能扣两次钱。
--
-- task_id 在任务删除时置空而不是连删:points 是已经花掉(或可能花掉)的估算,删了它,
-- 当月的预算就凭空多出一截。
CREATE TABLE boce_runs (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id  INTEGER REFERENCES boce_tasks(id) ON DELETE SET NULL,
    node_id  INTEGER NOT NULL,
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('SCHEDULE', 'MANUAL')),
    -- 定时轮次的名义时间槽(UTC);手动为空
    slot     TEXT    NOT NULL DEFAULT '',
    idem_key TEXT    NOT NULL,
    --   CREATING 已预留、正在创建;UNKNOWN 创建请求发出去了但结果不确定(不重试,预留按已花算)
    --   POLLING 已拿到任务 ID、正在取结果;DONE 上游说完成了;TIMEOUT 2 分钟没完成(已拿到的照存)
    --   EXPIRED 任务 ID 失效 / 结果已过保留期;FAILED 创建被明确拒绝(没扣费);SKIPPED 没执行(预算、关机)
    state    TEXT    NOT NULL CHECK (state IN ('CREATING', 'UNKNOWN', 'POLLING', 'DONE', 'TIMEOUT',
                                               'EXPIRED', 'FAILED', 'SKIPPED')),
    vendor_task_id TEXT NOT NULL DEFAULT '',
    -- 提交的 host:port 与当时目标的口径(generation)
    host       TEXT    NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    vantage_ids TEXT   NOT NULL DEFAULT '[]',
    -- points 是这一轮此刻的估算消耗(预留时 = 拨测点数 × 每点波点,结算时扣掉不计费的拨测点,失败归零);
    -- points_reserved 是预留时的数字,留着对账
    points          INTEGER NOT NULL DEFAULT 0,
    points_reserved INTEGER NOT NULL DEFAULT 0,
    -- 预算按设置的时区切日 / 切月,这两列在预留时就定下来
    day_key   TEXT NOT NULL DEFAULT '',
    month_key TEXT NOT NULL DEFAULT '',
    -- 创建请求发了几次(只有确定没发出去才会重试)与取结果问了几次
    attempts     INTEGER NOT NULL DEFAULT 0,
    polls        INTEGER NOT NULL DEFAULT 0,
    next_poll_at TEXT    NOT NULL DEFAULT '',
    results      INTEGER NOT NULL DEFAULT 0,
    error_code   INTEGER NOT NULL DEFAULT 0,
    error        TEXT    NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    sent_at      TEXT NOT NULL DEFAULT '',
    finished_at  TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_boce_runs_idem ON boce_runs(idem_key);
CREATE INDEX idx_boce_runs_task ON boce_runs(task_id, created_at);
CREATE INDEX idx_boce_runs_state ON boce_runs(state);
CREATE INDEX idx_boce_runs_day ON boce_runs(day_key);
CREATE INDEX idx_boce_runs_month ON boce_runs(month_key);
CREATE INDEX idx_boce_runs_created ON boce_runs(created_at);

-- ---------- 拨测点明细(供应商原始字段) ----------
-- 统一口径的结果在 probe_results 里(7 天);这里留 Boce 自己的字段给执行记录看(90 天)。
CREATE TABLE boce_results (
    run_id     INTEGER NOT NULL REFERENCES boce_runs(id) ON DELETE CASCADE,
    vantage_id INTEGER NOT NULL,
    -- 写进的那条序列;序列删了这份明细也留着
    target_id  INTEGER NOT NULL DEFAULT 0,
    at         TEXT    NOT NULL,
    status     TEXT    NOT NULL,
    node_name  TEXT    NOT NULL DEFAULT '',
    origin_ip  TEXT    NOT NULL DEFAULT '',
    ip         TEXT    NOT NULL DEFAULT '',
    ip_region  TEXT    NOT NULL DEFAULT '',
    ip_isp     TEXT    NOT NULL DEFAULT '',
    -- 上游原样(单位文档没写),不参与统计:丢包率由 sent / received 自己算
    packet_loss REAL   NOT NULL DEFAULT 0,
    sent       INTEGER NOT NULL DEFAULT 0,
    received   INTEGER NOT NULL DEFAULT 0,
    rtt_avg_ms REAL    NOT NULL DEFAULT 0,
    rtt_min_ms REAL    NOT NULL DEFAULT 0,
    rtt_max_ms REAL    NOT NULL DEFAULT 0,
    error_code INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    -- 0 = 不计费(上游说节点下线 not found node,不扣波点)
    charged    INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (run_id, vantage_id)
);

-- ---------- 运行状态(单行) ----------
CREATE TABLE boce_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    verify_state  TEXT NOT NULL DEFAULT 'NEVER' CHECK (verify_state IN ('NEVER', 'OK', 'ERROR')),
    verified_at   TEXT NOT NULL DEFAULT '',
    verify_error  TEXT NOT NULL DEFAULT '',
    -- 余额是 Boce 账户的真实数字,可能受面板以外的调用影响;与面板自己的估算消耗分开显示
    balance_yuan   TEXT    NOT NULL DEFAULT '',
    balance_points INTEGER,
    balance_at     TEXT    NOT NULL DEFAULT '',
    balance_error  TEXT    NOT NULL DEFAULT '',
    vantages_synced_at TEXT NOT NULL DEFAULT '',
    vantages_error     TEXT NOT NULL DEFAULT '',
    -- 鉴权失败、波点不足、连续接口异常时自动暂停,要管理员恢复
    auto_paused       INTEGER NOT NULL DEFAULT 0,
    auto_pause_reason TEXT    NOT NULL DEFAULT '',
    auto_paused_at    TEXT    NOT NULL DEFAULT '',
    consecutive_api_errors INTEGER NOT NULL DEFAULT 0,
    last_api_error    TEXT NOT NULL DEFAULT '',
    last_api_error_at TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO boce_state (id) VALUES (1);

-- 提醒去重(预算 80% / 用完按日、月键;低余额;接口异常),INSERT OR IGNORE 拿到键才推。
CREATE TABLE boce_marks (
    key TEXT PRIMARY KEY,
    at  TEXT NOT NULL
);

-- ---------- 卡片展示偏好 ----------
-- 每台节点的卡片入站 / 出站各最多展示 3 项。这是【展示】数量,不是检测数量;
-- 没有行 = 两个方向都自动推荐(升级之后卡片不会空)。改它与 Boce 引擎没有任何调用关系。
CREATE TABLE probe_card_prefs (
    node_id       INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    ingress_mode  TEXT NOT NULL DEFAULT 'AUTO' CHECK (ingress_mode IN ('AUTO', 'MANUAL')),
    -- 有序的序列 ID(JSON 数组)
    ingress_ids   TEXT NOT NULL DEFAULT '[]',
    outbound_mode TEXT NOT NULL DEFAULT 'AUTO' CHECK (outbound_mode IN ('AUTO', 'MANUAL')),
    outbound_ids  TEXT NOT NULL DEFAULT '[]',
    updated_at    TEXT NOT NULL
);

-- 单台节点暂停 Boce 自动检测(手动检测不受影响,那是管理员当场的决定)。
ALTER TABLE probe_node_settings ADD COLUMN boce_paused INTEGER NOT NULL DEFAULT 0;

-- ---------- 三张探测表重建 ----------

CREATE TABLE probe_targets_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    direction TEXT NOT NULL DEFAULT 'INGRESS' CHECK (direction IN ('INGRESS', 'OUTBOUND')),
    protocol  TEXT NOT NULL DEFAULT 'TCP' CHECK (protocol IN ('TCP', 'ICMP')),
    kind    TEXT    NOT NULL CHECK (kind IN ('MGMT', 'SERVICE', 'PUBLIC')),
    ref     TEXT    NOT NULL,
    public_id INTEGER REFERENCES probe_public_targets(id) ON DELETE CASCADE,
    family  TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    label   TEXT    NOT NULL DEFAULT '',
    -- MASTER = 主控本机;NODE = 被测 VPS 本机;BOCE = Boce 的拨测点
    source  TEXT    NOT NULL DEFAULT 'MASTER',
    enabled INTEGER NOT NULL DEFAULT 1,
    resolved_host    TEXT    NOT NULL DEFAULT '',
    resolved_port    INTEGER NOT NULL DEFAULT 0,
    generation       INTEGER NOT NULL DEFAULT 1,
    generation_since TEXT    NOT NULL,
    ref_state TEXT NOT NULL DEFAULT 'OK' CHECK (ref_state IN ('OK', 'INVALID')),
    ref_error TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    alert_state          TEXT    NOT NULL DEFAULT 'OK' CHECK (alert_state IN ('OK', 'DOWN')),
    alert_since          TEXT    NOT NULL DEFAULT '',
    alert_notified       INTEGER NOT NULL DEFAULT 0,
    recovery_pending     INTEGER NOT NULL DEFAULT 0,
    last_status     TEXT NOT NULL DEFAULT '',
    last_at         TEXT NOT NULL DEFAULT '',
    last_success_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    tcp_mode TEXT NOT NULL DEFAULT 'CONNECT' CHECK (tcp_mode IN ('CONNECT', 'SYN')),
    -- Boce 序列所属的任务与拨测点。删任务连序列与历史一起删(界面上写明;只想停用就归档)。
    boce_task_id INTEGER REFERENCES boce_tasks(id) ON DELETE CASCADE,
    vantage_id   INTEGER NOT NULL DEFAULT 0,
    -- 三类序列的字段组合写死:混写的话,一条「Boce 测的」或「主控测的 ICMP」会安静地进到另一类的统计里。
    CHECK (
        (direction = 'INGRESS' AND source = 'MASTER' AND kind IN ('MGMT', 'SERVICE') AND public_id IS NULL
                               AND protocol = 'TCP' AND boce_task_id IS NULL AND vantage_id = 0) OR
        (direction = 'INGRESS' AND source = 'BOCE' AND kind = 'SERVICE' AND public_id IS NULL
                               AND protocol = 'TCP' AND boce_task_id IS NOT NULL AND vantage_id > 0) OR
        (direction = 'OUTBOUND' AND kind = 'PUBLIC' AND public_id IS NOT NULL AND source = 'NODE'
                                AND boce_task_id IS NULL AND vantage_id = 0)
    )
);

INSERT INTO probe_targets_new
  (id, node_id, direction, protocol, kind, ref, public_id, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at, tcp_mode, boce_task_id, vantage_id)
SELECT id, node_id, direction, protocol, kind, ref, public_id, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at, tcp_mode, NULL, 0
  FROM probe_targets;

-- status 新增三种,都**没有发起**、不进比例的分母、不改变连续失败计数:
--   VANTAGE_ERROR  Boce 拨测点自身异常(上游 error_code 非 0「视为拨测节点未响应」,含节点下线)
--   API_ERROR      Boce 接口异常(创建被拒、创建结果不确定、取结果失败)—— 不是目标不通
--   EXPIRED        任务建了但这个拨测点没出结果(2 分钟没完成、任务 ID 失效、结果过了保留期)
CREATE TABLE probe_results_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id  INTEGER NOT NULL REFERENCES probe_targets_new(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    at         TEXT    NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('OK', 'LOSS', 'TIMEOUT', 'REFUSED', 'UNREACHABLE', 'ERROR',
                                                  'DNS_ERROR', 'BLOCKED', 'SSH_ERROR', 'TOOL_ERROR', 'SKIPPED',
                                                  'VANTAGE_ERROR', 'API_ERROR', 'EXPIRED')),
    rtt_us     INTEGER NOT NULL DEFAULT 0,
    dns_us     INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    sent       INTEGER NOT NULL DEFAULT 0,
    received   INTEGER NOT NULL DEFAULT 0,
    rtt_min_us INTEGER NOT NULL DEFAULT 0,
    rtt_max_us INTEGER NOT NULL DEFAULT 0,
    peer       TEXT    NOT NULL DEFAULT '',
    rtt_n      INTEGER NOT NULL DEFAULT 0
);

INSERT INTO probe_results_new (id, target_id, generation, source, at, status, rtt_us, dns_us, error,
                               sent, received, rtt_min_us, rtt_max_us, peer, rtt_n)
SELECT id, target_id, generation, source, at, status, rtt_us, dns_us, error,
       sent, received, rtt_min_us, rtt_max_us, peer, rtt_n
  FROM probe_results;

CREATE TABLE probe_hourly_new (
    target_id   INTEGER NOT NULL REFERENCES probe_targets_new(id) ON DELETE CASCADE,
    generation  INTEGER NOT NULL,
    hour        TEXT    NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    ok          INTEGER NOT NULL DEFAULT 0,
    lossy       INTEGER NOT NULL DEFAULT 0,
    dns_errors  INTEGER NOT NULL DEFAULT 0,
    skipped     INTEGER NOT NULL DEFAULT 0,
    ssh_errors  INTEGER NOT NULL DEFAULT 0,
    tool_errors INTEGER NOT NULL DEFAULT 0,
    blocked     INTEGER NOT NULL DEFAULT 0,
    sent        INTEGER NOT NULL DEFAULT 0,
    received    INTEGER NOT NULL DEFAULT 0,
    rtt_n       INTEGER NOT NULL DEFAULT 0,
    rtt_sum_us  INTEGER NOT NULL DEFAULT 0,
    rtt_min_us  INTEGER NOT NULL DEFAULT 0,
    rtt_max_us  INTEGER NOT NULL DEFAULT 0,
    vantage_errors INTEGER NOT NULL DEFAULT 0,
    api_errors     INTEGER NOT NULL DEFAULT 0,
    expired        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (target_id, generation, hour)
);

INSERT INTO probe_hourly_new (target_id, generation, hour, attempts, ok, lossy, dns_errors, skipped,
                              ssh_errors, tool_errors, blocked, sent, received, rtt_n,
                              rtt_sum_us, rtt_min_us, rtt_max_us)
SELECT target_id, generation, hour, attempts, ok, lossy, dns_errors, skipped,
       ssh_errors, tool_errors, blocked, sent, received, rtt_n,
       rtt_sum_us, rtt_min_us, rtt_max_us
  FROM probe_hourly;

-- 先子后父,见文件开头。
DROP TABLE probe_results;
DROP TABLE probe_hourly;
DROP TABLE probe_targets;

ALTER TABLE probe_targets_new RENAME TO probe_targets;
ALTER TABLE probe_results_new RENAME TO probe_results;
ALTER TABLE probe_hourly_new RENAME TO probe_hourly;

CREATE UNIQUE INDEX idx_probe_targets_ref ON probe_targets(node_id, ref, family, source) WHERE source != 'BOCE';
CREATE UNIQUE INDEX idx_probe_targets_boce ON probe_targets(boce_task_id, vantage_id) WHERE source = 'BOCE';
CREATE INDEX idx_probe_targets_node ON probe_targets(node_id);
CREATE INDEX idx_probe_targets_public ON probe_targets(public_id);
CREATE INDEX idx_probe_targets_task ON probe_targets(boce_task_id);
CREATE INDEX idx_probe_results_target_at ON probe_results(target_id, at);
CREATE INDEX idx_probe_results_at ON probe_results(at);
CREATE INDEX idx_probe_hourly_hour ON probe_hourly(hour);
