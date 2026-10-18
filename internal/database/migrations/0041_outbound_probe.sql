-- 出站网络质量探测:每台自建 VPS 自己去测管理员配置的公共目标(ICMP / TCP)。
--
-- 0040 的探测是「主控 → 节点」的入口 TCP 探测,这一版把主力换成「节点 → 公共目标」:
-- 在 VPS 本机执行(经已有的 SSH 长连接临时启动一个非驻留辅助程序,跑完就退出),
-- 量的是这台机器出去的网络质量。原来的入口探测保留,改称「入口连通性」,
-- 两类数据用 direction 分开存、分开算、分开显示,绝不合进同一个数字。
--
--   probe_public_targets  公共目标库(全局):管理员登记的 IP / 域名 + 协议 + 端口 + 间隔。
--   probe_node_bindings   节点覆盖:某台机器上强制开 / 强制关;没有行 = 跟随 apply_all。
--   probe_targets         变成「监测序列」表:入口序列(0040 原样)+ 出站序列(节点 × 公共目标)。
--   probe_results / probe_hourly  复用,加 ICMP 的发包 / 收包与最小 / 最大 RTT。
--
-- **迁移不创建任何公共目标、不写任何绑定**:升级之后不会有任何一台机器开始出站探测,
-- 已有的入口目标也一个不动、继续按原样探测。旧的入口配置不会被转换成公共目标 ——
-- 两者回答的是不同的问题(主控连不连得上这台机器 / 这台机器连不连得出去)。
--
-- SQLite 改不了 CHECK,三张表只能重建。顺序是载重的:先建三张新表并把数据全量搬过去,
-- 再【先删两张子表、后删父表】,最后改名。反过来先删父表的话,foreign_keys 开着,
-- DROP TABLE 的隐式 DELETE 会顺着 ON DELETE CASCADE 把全部历史结果与小时汇总一起删掉,
-- 而迁移照样成功、一个错都不报。改名时 SQLite 会把子表里指向 probe_targets_new 的
-- 外键一并改写成 probe_targets(legacy_alter_table 默认关闭)。

CREATE TABLE probe_public_targets (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    name     TEXT    NOT NULL,
    -- 规范化之后的 IP 字面量(无方括号)或小写域名。面板不在登记时解析域名:
    -- 每一轮由节点自己解析,域名换了指向就跟着测新地址(结果里记下实际连的那个 IP)。
    host     TEXT    NOT NULL,
    -- 协议与地址族一经创建不可改:换了就是另一条测量,旧的点阵、丢包率与它不可比。
    protocol TEXT    NOT NULL CHECK (protocol IN ('ICMP', 'TCP')),
    port     INTEGER NOT NULL DEFAULT 0,
    family   TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    -- 每个目标自己的探测间隔(秒),30~3600。
    interval_sec INTEGER NOT NULL DEFAULT 60 CHECK (interval_sec BETWEEN 30 AND 3600),
    -- ICMP 每轮发几个包(1~10);TCP 每轮建连一次,这一列不用。
    count        INTEGER NOT NULL DEFAULT 4 CHECK (count BETWEEN 1 AND 10),
    -- 单个包 / 单次建连的超时(毫秒),200~3000。
    timeout_ms   INTEGER NOT NULL DEFAULT 1000 CHECK (timeout_ms BETWEEN 200 AND 3000),
    -- 1 = 对全部自建节点生效(节点上的 OFF 绑定仍然可以把它关掉)。默认 0。
    apply_all INTEGER NOT NULL DEFAULT 0,
    -- 0 = 全局暂停:哪台机器都不测,历史保留。
    enabled   INTEGER NOT NULL DEFAULT 1,
    -- 1 = 允许私网 / 回环 / 保留地址与邮件、SMB 这类端口。默认拒绝。
    allow_restricted INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    note       TEXT    NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((protocol = 'ICMP' AND port = 0) OR (protocol = 'TCP' AND port BETWEEN 1 AND 65535))
);
CREATE UNIQUE INDEX idx_probe_public_targets_key ON probe_public_targets(host, protocol, port, family);

CREATE TABLE probe_node_bindings (
    node_id   INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    public_id INTEGER NOT NULL REFERENCES probe_public_targets(id) ON DELETE CASCADE,
    mode      TEXT    NOT NULL CHECK (mode IN ('ON', 'OFF')),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (node_id, public_id)
);
CREATE INDEX idx_probe_node_bindings_public ON probe_node_bindings(public_id);

CREATE TABLE probe_targets_new (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- INGRESS = 主控 → 这台机器(入口连通性,0040 的全部目标);OUTBOUND = 这台机器 → 公共目标。
    direction TEXT NOT NULL DEFAULT 'INGRESS' CHECK (direction IN ('INGRESS', 'OUTBOUND')),
    protocol  TEXT NOT NULL DEFAULT 'TCP' CHECK (protocol IN ('TCP', 'ICMP')),
    kind    TEXT    NOT NULL CHECK (kind IN ('MGMT', 'SERVICE', 'PUBLIC')),
    -- 入口:MGMT | EP:<id> | ENTRY:<种类>:<id>(见 0040);出站:PUB:<probe_public_targets.id>
    ref     TEXT    NOT NULL,
    -- 出站序列指向的公共目标。删公共目标时它的序列与历史一起删(界面上写明)。
    public_id INTEGER REFERENCES probe_public_targets(id) ON DELETE CASCADE,
    family  TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    label   TEXT    NOT NULL DEFAULT '',
    -- MASTER = 主控本机执行;NODE = 在被测的这台 VPS 本机执行。
    source  TEXT    NOT NULL DEFAULT 'MASTER',
    -- 出站序列的 enabled 由「公共目标 × 绑定」算出来写进来,不让人直接改;停用不删行(历史保留)。
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
    -- 两类序列的字段组合写死:入口只能是主控发起的 TCP,出站只能是节点发起、挂着公共目标。
    -- 混写的话,一条「主控测的 ICMP」或「节点测的管理通道」会安静地进到另一类的统计里。
    CHECK (
        (direction = 'INGRESS'  AND kind IN ('MGMT', 'SERVICE') AND public_id IS NULL
                                AND protocol = 'TCP' AND source = 'MASTER') OR
        (direction = 'OUTBOUND' AND kind = 'PUBLIC' AND public_id IS NOT NULL AND source = 'NODE')
    )
);

INSERT INTO probe_targets_new
  (id, node_id, direction, protocol, kind, ref, public_id, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at)
SELECT id, node_id, 'INGRESS', 'TCP', kind, ref, NULL, family, label, source, enabled,
   resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
   consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
   last_status, last_at, last_success_at, created_at, updated_at
  FROM probe_targets;

-- status:
--   OK           TCP 建连成功 / ICMP 全部收到
--   LOSS         ICMP 部分丢包(收到了一部分):目标可达,丢包率另算
--   TIMEOUT / REFUSED / UNREACHABLE / ERROR   发起了但失败(ICMP 全丢记 TIMEOUT)
--   DNS_ERROR    域名没解析出来,没有发起 —— 不进丢包率 / 失败率的分母
--   BLOCKED      解析到受限地址或端口受限,没有发起
--   SSH_ERROR    采集失败:SSH 连不上 / 会话建不起来。**不是丢包**,目标那头什么都不知道
--   TOOL_ERROR   工具不可用:辅助程序没有 / 跑不起来 / 没有 ICMP 权限
--   SKIPPED      没有执行(error 是原因码,如 CLOUD_STOPPED)
-- sent / received:ICMP 是发出 / 收到的包数;TCP 是 1 / (OK ? 1 : 0)。丢包率 = Σ(sent − received) ÷ Σsent。
-- rtt_us 是平均 RTT(ICMP 按收到的包平均),rtt_min_us / rtt_max_us 是这一轮的最小 / 最大;peer 是实际连的 IP。
CREATE TABLE probe_results_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id  INTEGER NOT NULL REFERENCES probe_targets_new(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    at         TEXT    NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('OK', 'LOSS', 'TIMEOUT', 'REFUSED', 'UNREACHABLE', 'ERROR',
                                                  'DNS_ERROR', 'BLOCKED', 'SSH_ERROR', 'TOOL_ERROR', 'SKIPPED')),
    rtt_us     INTEGER NOT NULL DEFAULT 0,
    dns_us     INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    sent       INTEGER NOT NULL DEFAULT 0,
    received   INTEGER NOT NULL DEFAULT 0,
    rtt_min_us INTEGER NOT NULL DEFAULT 0,
    rtt_max_us INTEGER NOT NULL DEFAULT 0,
    peer       TEXT    NOT NULL DEFAULT ''
);

INSERT INTO probe_results_new (id, target_id, generation, source, at, status, rtt_us, dns_us, error,
                               sent, received, rtt_min_us, rtt_max_us, peer)
SELECT id, target_id, generation, source, at, status, rtt_us, dns_us, error,
       CASE WHEN status IN ('OK', 'TIMEOUT', 'REFUSED', 'UNREACHABLE', 'ERROR') THEN 1 ELSE 0 END,
       CASE WHEN status = 'OK' THEN 1 ELSE 0 END,
       CASE WHEN status = 'OK' THEN rtt_us ELSE 0 END,
       CASE WHEN status = 'OK' THEN rtt_us ELSE 0 END,
       ''
  FROM probe_results;

-- attempts / ok / lossy 按轮计;sent / received 按包(TCP 按建连)计;rtt_n 是参与 RTT 平均的样本数
-- (ICMP 是收到的包数,TCP 是成功的建连数)—— 平均值 = rtt_sum_us ÷ rtt_n,不能拿 ok 当分母。
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
    PRIMARY KEY (target_id, generation, hour)
);

INSERT INTO probe_hourly_new (target_id, generation, hour, attempts, ok, dns_errors, skipped,
                              sent, received, rtt_n, rtt_sum_us, rtt_min_us, rtt_max_us)
SELECT target_id, generation, hour, attempts, ok, dns_errors, skipped,
       attempts, ok, ok, rtt_sum_us, rtt_min_us, rtt_max_us
  FROM probe_hourly;

-- 先子后父,见文件开头。
DROP TABLE probe_results;
DROP TABLE probe_hourly;
DROP TABLE probe_targets;

ALTER TABLE probe_targets_new RENAME TO probe_targets;
ALTER TABLE probe_results_new RENAME TO probe_results;
ALTER TABLE probe_hourly_new RENAME TO probe_hourly;

CREATE UNIQUE INDEX idx_probe_targets_ref ON probe_targets(node_id, ref, family, source);
CREATE INDEX idx_probe_targets_node ON probe_targets(node_id);
CREATE INDEX idx_probe_targets_public ON probe_targets(public_id);
CREATE INDEX idx_probe_results_target_at ON probe_results(target_id, at);
CREATE INDEX idx_probe_results_at ON probe_results(at);
CREATE INDEX idx_probe_hourly_hour ON probe_hourly(hour);
