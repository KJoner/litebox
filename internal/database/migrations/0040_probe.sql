-- 节点资产卡片(P0-C):主控主动 TCP 连接探测。
--
-- 只回答一件事:**从主控这台机器,此刻能不能和目标的那个端口完成 TCP 三次握手、要多久。**
-- 它不是代理协议的健康检查(那是部署里的真实拨测),也不是 ICMP 丢包 ——
-- 端口能连上不等于代理能用,界面上一律写「TCP 延迟 / TCP 连接失败率」。
--
-- 三张表:
--   probe_targets  监测目标。不存自由输入的地址,只存「引用」(管理通道 / 某条订阅地址条目),
--                  每轮按引用解析出当前的地址与端口 —— 机器改了地址,目标跟着走;
--                  引用失效就停探,绝不继续打一个可能已经属于别人的旧地址。
--   probe_results  每次尝试的原始结果,保留 7 天。
--   probe_hourly   按小时汇总,保留 90 天,给 7 天视图与长周期趋势用。
--
-- 迁移不创建任何目标:升级之后面板不会凭空开始连生产机器,由管理员在节点详情里逐台开启。

CREATE TABLE probe_targets (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- MGMT 管理通道(host + SSH 端口);SERVICE 对外服务(订阅地址条目)。分开登记、分开统计。
    kind    TEXT    NOT NULL CHECK (kind IN ('MGMT', 'SERVICE')),
    -- MGMT | EP:<inbound_endpoints.id> | ENTRY:<SINGBOX|MIERU|NGINX|REALM>:<入口 id>
    -- (后者是没有订阅地址条目的入口,按 host + 跟随端口解析,与订阅的回落一致)
    ref     TEXT    NOT NULL,
    family  TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    label   TEXT    NOT NULL DEFAULT '',
    -- 探测来源。P0 只有 MASTER(主控);P2 的多地区 / 三网测点接入前,不产生任何别的值 ——
    -- 不能用同一个主控测量值冒充「电信 / 联通 / 移动」三个来源。
    source  TEXT    NOT NULL DEFAULT 'MASTER',
    enabled INTEGER NOT NULL DEFAULT 1,
    -- 上一轮解析出来的地址与端口。变了就换代(generation + 1),新旧统计口径隔离。
    resolved_host    TEXT    NOT NULL DEFAULT '',
    resolved_port    INTEGER NOT NULL DEFAULT 0,
    generation       INTEGER NOT NULL DEFAULT 1,
    generation_since TEXT    NOT NULL,
    -- INVALID:引用解析不出来(地址或入口被删、入口停用、UDP 传输)。停探,不告警。
    ref_state TEXT NOT NULL DEFAULT 'OK' CHECK (ref_state IN ('OK', 'INVALID')),
    ref_error TEXT NOT NULL DEFAULT '',
    -- 告警状态机持久化在这里:面板重启后不重复发「连续失败」,恢复通知也不会丢。
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    alert_state          TEXT    NOT NULL DEFAULT 'OK' CHECK (alert_state IN ('OK', 'DOWN')),
    alert_since          TEXT    NOT NULL DEFAULT '',
    -- 0 = 进入 DOWN 之后还没推出去(免打扰时段里攒着);1 = 已推送。
    alert_notified       INTEGER NOT NULL DEFAULT 0,
    -- 恢复发生在免打扰时段、而「失败」那条已经推出去了:时段结束后补一条恢复。
    recovery_pending     INTEGER NOT NULL DEFAULT 0,
    last_status     TEXT NOT NULL DEFAULT '',
    last_at         TEXT NOT NULL DEFAULT '',
    last_success_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_probe_targets_ref ON probe_targets(node_id, ref, family, source);
CREATE INDEX idx_probe_targets_node ON probe_targets(node_id);

-- status:
--   OK           建连成功,rtt_us 是三次握手耗时
--   TIMEOUT / REFUSED / UNREACHABLE / ERROR   发起了连接但失败 —— 进失败率的分母与分子
--   DNS_ERROR    域名没解析出来,**没有发起连接**,不进失败率分母,单独计数
--   SKIPPED      没有执行(error 里是原因码,如 CLOUD_STOPPED = 云实例停着)
CREATE TABLE probe_results (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id  INTEGER NOT NULL REFERENCES probe_targets(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    at         TEXT    NOT NULL,
    status     TEXT    NOT NULL CHECK (status IN ('OK', 'TIMEOUT', 'REFUSED', 'UNREACHABLE', 'ERROR', 'DNS_ERROR', 'SKIPPED')),
    rtt_us     INTEGER NOT NULL DEFAULT 0,
    dns_us     INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_probe_results_target_at ON probe_results(target_id, at);
CREATE INDEX idx_probe_results_at ON probe_results(at);

CREATE TABLE probe_hourly (
    target_id  INTEGER NOT NULL REFERENCES probe_targets(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    hour       TEXT    NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    ok         INTEGER NOT NULL DEFAULT 0,
    dns_errors INTEGER NOT NULL DEFAULT 0,
    skipped    INTEGER NOT NULL DEFAULT 0,
    rtt_sum_us INTEGER NOT NULL DEFAULT 0,
    rtt_min_us INTEGER NOT NULL DEFAULT 0,
    rtt_max_us INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (target_id, generation, hour)
);
CREATE INDEX idx_probe_hourly_hour ON probe_hourly(hour);
