-- Boce 外部拨测加一种测法:Ping(方案与交付见 docs/asset-probe/08-boce-ping.md)。
--
-- Boce 的 Ping 与 TCPing 两个接口形状一样,区别只在 host:TCPing 是「地址:端口」,Ping 只要地址。
-- 一个任务只有一种测法,**创建后不可改** —— 换测法就是另一条测量,旧的点阵与丢包率与它不可比。
--
-- 三处改动:
--
--   1. boce_tasks.test_type:任务的测法。0043 那一列 check_type 的 CHECK 只收 'TCPING',而 SQLite 改不了
--      CHECK;重建 boce_tasks 又要连带重建它的全部子表(boce_runs、boce_results、probe_targets 及其子表),
--      所以 check_type 就此冻结(恒为 'TCPING',没有代码再读它),测法看新加的这一列;
--   2. boce_runs.test_type:每一轮提交时的测法。取结果要按它选接口(拿 Ping 的任务 ID 去问 TCPing 的接口,
--      上游只会说任务 ID 失效);任务删了、执行记录还在结算时,也只有这一列说得清它是哪种;
--   3. 三张探测表照 0043 再重建一次,只放宽一条 CHECK:Boce 序列的 protocol 可以是 ICMP。
--      Ping 的序列记 ICMP,口径(metric)由 source + protocol 一处推出来,不必去联任务表。
--      顺序照 0043:先建新表并全量搬数据,【先删两张子表、后删父表】,最后改名 ——
--      反过来先删父表,DROP TABLE 的隐式 DELETE 会顺着 ON DELETE CASCADE 把全部历史一起删掉。
--
-- 已有的任务、执行记录、序列与历史一行不动,含义不变(全是 TCPing)。不建任何任务、不打开任何开关。

ALTER TABLE boce_tasks ADD COLUMN test_type TEXT NOT NULL DEFAULT 'TCPING' CHECK (test_type IN ('TCPING', 'PING'));
ALTER TABLE boce_runs  ADD COLUMN test_type TEXT NOT NULL DEFAULT 'TCPING' CHECK (test_type IN ('TCPING', 'PING'));

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
    -- Boce 序列:TCPing 记 TCP、Ping 记 ICMP(0044 起);主控的入口连通性仍然只有 TCP。
    CHECK (
        (direction = 'INGRESS' AND source = 'MASTER' AND kind IN ('MGMT', 'SERVICE') AND public_id IS NULL
                               AND protocol = 'TCP' AND boce_task_id IS NULL AND vantage_id = 0) OR
        (direction = 'INGRESS' AND source = 'BOCE' AND kind = 'SERVICE' AND public_id IS NULL
                               AND protocol IN ('TCP', 'ICMP') AND boce_task_id IS NOT NULL AND vantage_id != 0) OR
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
   last_status, last_at, last_success_at, created_at, updated_at, tcp_mode, boce_task_id, vantage_id
  FROM probe_targets;

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
                              rtt_sum_us, rtt_min_us, rtt_max_us, vantage_errors, api_errors, expired)
SELECT target_id, generation, hour, attempts, ok, lossy, dns_errors, skipped,
       ssh_errors, tool_errors, blocked, sent, received, rtt_n,
       rtt_sum_us, rtt_min_us, rtt_max_us, vantage_errors, api_errors, expired
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
