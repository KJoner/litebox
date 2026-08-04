-- 节点的可选 IPv6 地址,以及节点级的流量额度与重置周期。
--
-- IPv6 只影响订阅内容。SSH、探测、安装、部署、重启、流量同步与资源采集
-- 全部继续走 host(IPv4)—— 双栈里只要有一条路能管理节点就够了,
-- 两条路都试会让"节点连不上"这件事出现两种互相矛盾的结论。
--
-- 因此这里刻意不新增一条 nodes 记录来表示 IPv6:那会带来第二份
-- config_revision、第二串部署记录与第二套资源采样,而节点只有一台。
-- IPv6 是订阅生成时对同一条记录的逻辑展开。
ALTER TABLE nodes ADD COLUMN ipv6_address TEXT NOT NULL DEFAULT '';

-- 节点流量额度。0 表示不限量 —— 与用户额度的约定一致。
ALTER TABLE nodes ADD COLUMN traffic_quota_bytes INTEGER NOT NULL DEFAULT 0;

-- 重置周期只表示"从哪一刻开始重新计数",不删除任何历史数据:
-- traffic_ledger、traffic_daily、用户累计流量、节点计数器基线全部原样保留,
-- 当前周期用量是按时间范围现算的。
-- 这样管理员改重置日、改周期都不会毁掉历史报表,改回去也能还原。
ALTER TABLE nodes ADD COLUMN traffic_reset_cycle TEXT NOT NULL DEFAULT 'NONE'
    CHECK (traffic_reset_cycle IN ('NONE', 'MONTHLY'));

-- 每月重置日,1~31。当月没有该日时按当月最后一天处理(2 月的 31 日即 28/29 日)。
ALTER TABLE nodes ADD COLUMN traffic_reset_day INTEGER NOT NULL DEFAULT 1
    CHECK (traffic_reset_day BETWEEN 1 AND 31);

-- 周期用量按 node_id + created_at 范围汇总 ledger,已有的
-- idx_traffic_ledger_node(node_id, created_at) 正好覆盖,无需新建索引。
