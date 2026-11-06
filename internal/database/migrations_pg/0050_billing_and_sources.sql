-- (PostgreSQL 版,与 SQLite 的 0050 同一份逻辑结构)
-- VNext S6:用户计费规则、节点额度的数据来源、主机流量的计量网卡(TRAFFIC02 / 03 / 07)。
--
-- 一、用户计费流量 = 已封存段 + 倍率 × f(本段上行, 本段下行),f ∈ 上行 / 下行 / 双向合计 / 双向取大,
--     公式只在 internal/billing 一处。默认值(SUM、100%、封存 0、起点 0)让结果对任何数据都等于 U + D ——
--     存量用户的额度判定逐字节不变。billing_next_* 是「下一周期生效」的待生效规则(默认路径),
--     由周期重置时套用并把 billing_rule_version 加一;立即生效时旧段按旧规则算好封存,新段从此刻的 U / D 起算。
-- 二、节点额度的数据来源:PROXY_ESTIMATE(默认)是 V2.1 以来的「代理折算」(sing-box 计数 × 口径倍数),
--     HOST_NIC 是 vnStat 网卡实测。切换时记下分界(traffic_quota_source_since),之前的那一段仍按旧估算算并标明,
--     不把旧估算改写成网卡实测;用户额度与用户历史账本一个字节都不受影响。
-- 三、主机流量的计量网卡:iface_pinned 为空 = 跟随默认路由(升级前的行为);指定之后同步与实时曲线都读它,
--     iface_changed_at 是换网卡的分界,之前的柱子来自另一块网卡。

ALTER TABLE proxy_users ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'SUM'
    CHECK (billing_mode IN ('SUM', 'UP', 'DOWN', 'MAX'));
ALTER TABLE proxy_users ADD COLUMN billing_multiplier_pct BIGINT NOT NULL DEFAULT 100
    CHECK (billing_multiplier_pct BETWEEN 1 AND 10000);
ALTER TABLE proxy_users ADD COLUMN billing_rule_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE proxy_users ADD COLUMN billing_next_mode TEXT NOT NULL DEFAULT ''
    CHECK (billing_next_mode IN ('', 'SUM', 'UP', 'DOWN', 'MAX'));
ALTER TABLE proxy_users ADD COLUMN billing_next_multiplier_pct BIGINT NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN billing_sealed_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN billing_base_uplink BIGINT NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN billing_base_downlink BIGINT NOT NULL DEFAULT 0;
ALTER TABLE proxy_users ADD COLUMN billing_changed_at TEXT NOT NULL DEFAULT '';

ALTER TABLE nodes ADD COLUMN traffic_quota_source TEXT NOT NULL DEFAULT 'PROXY_ESTIMATE'
    CHECK (traffic_quota_source IN ('PROXY_ESTIMATE', 'HOST_NIC'));
ALTER TABLE nodes ADD COLUMN traffic_quota_source_since TEXT NOT NULL DEFAULT '';

ALTER TABLE host_traffic_state ADD COLUMN iface_pinned TEXT NOT NULL DEFAULT '';
ALTER TABLE host_traffic_state ADD COLUMN iface_changed_at TEXT NOT NULL DEFAULT '';
