-- R06:主机流量的桶记下来源网卡,换网卡不再改写切换前的历史。
--
-- 原来 host_traffic 的冲突键只有 (node_id, granularity, bucket_ts):换到另一块网卡之后,
-- 新网卡的 vnstat 全量数据里同一小时的桶会直接覆盖旧网卡记下的那一行 —— 界面上说「之前的柱子
-- 来自另一块网卡」,存储里却已经全是新网卡的数。
--
-- iface 是写入这一行的网卡。存量行填当时状态行里的那块网卡(这一列加之前从没换过网卡的机器,
-- 那就是对的;已经换过的机器,旧历史在升级前就被覆盖掉了,补不回来)。
-- boundary = 1 是跨在切换时刻上的那个桶:它只保留切换之前那块网卡记下的部分,之后不再更新,
-- 也不让新网卡的整桶数顶替 —— 宁可少算那一截,不拼出两份整桶。
-- host_traffic_state.iface_since 是当前这块网卡开始计量的时刻(unix 秒,0 = 从没换过)。
ALTER TABLE host_traffic ADD COLUMN iface TEXT NOT NULL DEFAULT '';
ALTER TABLE host_traffic ADD COLUMN boundary INTEGER NOT NULL DEFAULT 0;
UPDATE host_traffic SET iface = COALESCE(
    (SELECT s.iface FROM host_traffic_state s WHERE s.node_id = host_traffic.node_id), '');
ALTER TABLE host_traffic_state ADD COLUMN iface_since INTEGER NOT NULL DEFAULT 0;
