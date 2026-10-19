-- 探针增强三项(方案见 docs/asset-probe/09-probe-enhance-plan.md):
--
--   1. 自建节点互测入站(PEER):另一台自建 VPS 用节点上的 litebox-probe 对这台机器的订阅入口做 Ping / TCPing;
--   2. 出站 HTTP(S) 请求响应检测:目标库多一种测法,目标是完整 URL,取得任何 HTTP 响应都记耗时与状态码;
--   3. 卡片展示名称:每台节点卡片上入站 / 出站展示项的别名。
--
-- 前两项都要放宽 CHECK,SQLite 改不了 CHECK,只能重建:
--   probe_public_targets  protocol 多 HTTP、加 url、HTTP 的超时上限放宽到 10 秒;唯一键带上 url
--                         (同一主机端口上的两个路径是两个目标);
--   probe_node_bindings   它是上一张的子表,父表要重建它就得跟着重建;
--   probe_targets         加 peer_node_id(PEER 序列的拨测来源)与 resolved_url(HTTP 序列这一代测的 URL);
--                         CHECK 多一个 PEER 的组合;protocol 多 HTTP(只能是出站);
--   probe_results         加 http_status(HTTP 序列取得响应时的状态码,别的序列为 0);
--   probe_hourly          加 http_2xx / 3xx / 4xx / 5xx 四个计数(只有 HTTP 序列有值,都不改任何比例)。
--
-- 顺序照 0043 / 0044:先建全部新表并全量拷贝,再【先删子表、后删父表】,最后改名。反过来先删父表的话,
-- foreign_keys 开着,DROP TABLE 的隐式 DELETE 会顺着 ON DELETE CASCADE 把节点覆盖、全部序列与历史一起删掉,
-- 而迁移照样成功、一个错都不报。
--
-- 存量数据一行不少、含义不变:已有目标全是 ICMP / TCP、url 为空;已有序列 peer_node_id 为空;
-- 已有明细状态码为 0。**迁移不建任何 PEER 序列、不建任何 HTTP 目标、不打开任何开关。**

CREATE TABLE probe_public_targets_new (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    name     TEXT    NOT NULL,
    -- 规范化之后的 IP 字面量(无方括号)或小写域名。HTTP 目标是 URL 里的主机。
    host     TEXT    NOT NULL,
    -- 协议、TCP 方式与地址族一经创建不可改:换了就是另一条测量,旧的点阵、比例与它不可比。
    protocol TEXT    NOT NULL CHECK (protocol IN ('ICMP', 'TCP', 'HTTP')),
    -- HTTP 目标是 URL 里的端口(没写时按 80 / 443)。
    port     INTEGER NOT NULL DEFAULT 0,
    family   TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    interval_sec INTEGER NOT NULL DEFAULT 60 CHECK (interval_sec BETWEEN 30 AND 3600),
    count        INTEGER NOT NULL DEFAULT 4 CHECK (count BETWEEN 1 AND 10),
    -- ICMP / TCP 的上限仍是 3 秒(程序里拦);HTTP 一次请求要走 DNS + TCP + TLS + 等响应头,放宽到 10 秒。
    timeout_ms   INTEGER NOT NULL DEFAULT 1000 CHECK (timeout_ms BETWEEN 200 AND 10000),
    apply_all INTEGER NOT NULL DEFAULT 0,
    enabled   INTEGER NOT NULL DEFAULT 1,
    allow_restricted INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    note       TEXT    NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('MANUAL', 'ZSTATIC', 'TCPQUALITY')),
    source_key TEXT NOT NULL DEFAULT '',
    tcp_mode TEXT NOT NULL DEFAULT 'CONNECT' CHECK (tcp_mode IN ('CONNECT', 'SYN')),
    region   TEXT NOT NULL DEFAULT '',
    province TEXT NOT NULL DEFAULT '',
    city     TEXT NOT NULL DEFAULT '',
    isp      TEXT NOT NULL DEFAULT '' CHECK (isp IN ('', 'CT', 'CU', 'CM', 'CERNET', 'OTHER')),
    role TEXT NOT NULL DEFAULT 'PRIMARY' CHECK (role IN ('PRIMARY', 'BACKUP')),
    verify_state TEXT NOT NULL DEFAULT 'UNVERIFIED' CHECK (verify_state IN ('UNVERIFIED', 'OK', 'WARN', 'FAILED')),
    verified_at   TEXT NOT NULL DEFAULT '',
    verify_detail TEXT NOT NULL DEFAULT '',
    upstream TEXT NOT NULL DEFAULT '',
    manual_fields TEXT NOT NULL DEFAULT '',
    upstream_missing_since TEXT NOT NULL DEFAULT '',
    -- HTTP 目标的完整 URL(http:// 或 https://,不含账号密码);ICMP / TCP 为空串。
    url TEXT NOT NULL DEFAULT '',
    CHECK ((protocol = 'ICMP' AND port = 0 AND url = '') OR
           (protocol = 'TCP' AND port BETWEEN 1 AND 65535 AND url = '') OR
           (protocol = 'HTTP' AND port BETWEEN 1 AND 65535 AND url != '' AND tcp_mode = 'CONNECT')),
    CHECK (protocol = 'HTTP' OR timeout_ms <= 3000)
);

INSERT INTO probe_public_targets_new
  (id, name, host, protocol, port, family, interval_sec, count, timeout_ms, apply_all, enabled, allow_restricted,
   sort_order, note, created_at, updated_at, source, source_key, tcp_mode, region, province, city, isp, role,
   verify_state, verified_at, verify_detail, upstream, manual_fields, upstream_missing_since, url)
SELECT id, name, host, protocol, port, family, interval_sec, count, timeout_ms, apply_all, enabled, allow_restricted,
   sort_order, note, created_at, updated_at, source, source_key, tcp_mode, region, province, city, isp, role,
   verify_state, verified_at, verify_detail, upstream, manual_fields, upstream_missing_since, ''
  FROM probe_public_targets;

CREATE TABLE probe_node_bindings_new (
    node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    public_id INTEGER NOT NULL REFERENCES probe_public_targets_new(id) ON DELETE CASCADE,
    mode      TEXT    NOT NULL CHECK (mode IN ('ON', 'OFF')),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, public_id)
);

INSERT INTO probe_node_bindings_new (node_id, public_id, mode, updated_at)
SELECT node_id, public_id, mode, updated_at FROM probe_node_bindings;

CREATE TABLE probe_targets_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 被测的机器。PEER 序列也是:结果归属被测节点的入站。
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    direction TEXT NOT NULL DEFAULT 'INGRESS' CHECK (direction IN ('INGRESS', 'OUTBOUND')),
    protocol  TEXT NOT NULL DEFAULT 'TCP' CHECK (protocol IN ('TCP', 'ICMP', 'HTTP')),
    kind    TEXT    NOT NULL CHECK (kind IN ('MGMT', 'SERVICE', 'PUBLIC')),
    ref     TEXT    NOT NULL,
    public_id INTEGER REFERENCES probe_public_targets_new(id) ON DELETE CASCADE,
    family  TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    label   TEXT    NOT NULL DEFAULT '',
    -- MASTER = 主控本机;NODE = 被测 VPS 本机;BOCE = Boce 的拨测点;PEER = 另一台自建 VPS(peer_node_id)
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
    boce_task_id INTEGER REFERENCES boce_tasks(id) ON DELETE CASCADE,
    vantage_id   INTEGER NOT NULL DEFAULT 0,
    -- PEER 序列的拨测来源(另一台自建 VPS)。来源机器硬删时连序列与历史一起删;软删除由程序显式清。
    peer_node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    -- HTTP 序列这一代测的 URL:目标库里改了 URL 就换代,新旧样本不混进同一个比例。
    resolved_url TEXT NOT NULL DEFAULT '',
    -- 四类序列的字段组合写死:混写的话,一条「Boce 测的」「别的节点测的」或「主控测的 ICMP」
    -- 会安静地进到另一类的统计里。HTTP 只能是出站。
    CHECK (
        (direction = 'INGRESS' AND source = 'MASTER' AND kind IN ('MGMT', 'SERVICE') AND public_id IS NULL
                               AND protocol = 'TCP' AND boce_task_id IS NULL AND vantage_id = 0 AND peer_node_id IS NULL) OR
        (direction = 'INGRESS' AND source = 'BOCE' AND kind = 'SERVICE' AND public_id IS NULL
                               AND protocol IN ('TCP', 'ICMP') AND boce_task_id IS NOT NULL AND vantage_id != 0
                               AND peer_node_id IS NULL) OR
        (direction = 'INGRESS' AND source = 'PEER' AND kind = 'SERVICE' AND public_id IS NULL
                               AND protocol IN ('TCP', 'ICMP') AND boce_task_id IS NULL AND vantage_id = 0
                               AND peer_node_id IS NOT NULL AND peer_node_id != node_id) OR
        (direction = 'OUTBOUND' AND kind = 'PUBLIC' AND public_id IS NOT NULL AND source = 'NODE'
                                AND boce_task_id IS NULL AND vantage_id = 0 AND peer_node_id IS NULL)
    )
);

INSERT INTO probe_targets_new
  (id, node_id, direction, protocol, kind, ref, public_id, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at, tcp_mode, boce_task_id, vantage_id,
   peer_node_id, resolved_url)
SELECT id, node_id, direction, protocol, kind, ref, public_id, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at, tcp_mode, boce_task_id, vantage_id,
   NULL, ''
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
    rtt_n      INTEGER NOT NULL DEFAULT 0,
    -- HTTP 序列取得响应时的状态码(100~599);没取得响应、或不是 HTTP 序列时为 0。
    http_status INTEGER NOT NULL DEFAULT 0 CHECK (http_status = 0 OR http_status BETWEEN 100 AND 599)
);

INSERT INTO probe_results_new (id, target_id, generation, source, at, status, rtt_us, dns_us, error,
                               sent, received, rtt_min_us, rtt_max_us, peer, rtt_n, http_status)
SELECT id, target_id, generation, source, at, status, rtt_us, dns_us, error,
       sent, received, rtt_min_us, rtt_max_us, peer, rtt_n, 0
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
    -- HTTP 序列取得的响应按状态码分类计数(极少见的 1xx 计入 2xx)。
    http_2xx INTEGER NOT NULL DEFAULT 0,
    http_3xx INTEGER NOT NULL DEFAULT 0,
    http_4xx INTEGER NOT NULL DEFAULT 0,
    http_5xx INTEGER NOT NULL DEFAULT 0,
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
DROP TABLE probe_node_bindings;
DROP TABLE probe_public_targets;

ALTER TABLE probe_public_targets_new RENAME TO probe_public_targets;
ALTER TABLE probe_node_bindings_new RENAME TO probe_node_bindings;
ALTER TABLE probe_targets_new RENAME TO probe_targets;
ALTER TABLE probe_results_new RENAME TO probe_results;
ALTER TABLE probe_hourly_new RENAME TO probe_hourly;

CREATE UNIQUE INDEX idx_probe_public_targets_key ON probe_public_targets(host, protocol, tcp_mode, port, family, url);
CREATE UNIQUE INDEX idx_probe_public_targets_source_key ON probe_public_targets(source, source_key)
    WHERE source_key != '';
CREATE INDEX idx_probe_public_targets_source ON probe_public_targets(source);
CREATE INDEX idx_probe_node_bindings_public ON probe_node_bindings(public_id);

-- 主控与出站序列:同一台机器同一个引用只能登记一次。Boce 与 PEER 各有自己的唯一键。
CREATE UNIQUE INDEX idx_probe_targets_ref ON probe_targets(node_id, ref, family, source)
    WHERE source NOT IN ('BOCE', 'PEER');
CREATE UNIQUE INDEX idx_probe_targets_boce ON probe_targets(boce_task_id, vantage_id) WHERE source = 'BOCE';
-- 同一个被测入口、同一个来源、同一种测法只能有一条。
CREATE UNIQUE INDEX idx_probe_targets_peer ON probe_targets(node_id, ref, family, peer_node_id, protocol)
    WHERE source = 'PEER';
CREATE INDEX idx_probe_targets_node ON probe_targets(node_id);
CREATE INDEX idx_probe_targets_public ON probe_targets(public_id);
CREATE INDEX idx_probe_targets_task ON probe_targets(boce_task_id);
CREATE INDEX idx_probe_targets_peer_node ON probe_targets(peer_node_id);
CREATE INDEX idx_probe_results_target_at ON probe_results(target_id, at);
CREATE INDEX idx_probe_results_at ON probe_results(at);
CREATE INDEX idx_probe_hourly_hour ON probe_hourly(hour);

-- 卡片展示名称:序列 id → 别名(JSON)。只影响这台节点的卡片,与展示方式、排序、检测都无关。
ALTER TABLE probe_card_prefs ADD COLUMN ingress_aliases  TEXT NOT NULL DEFAULT '{}';
ALTER TABLE probe_card_prefs ADD COLUMN outbound_aliases TEXT NOT NULL DEFAULT '{}';
