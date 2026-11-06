-- VNext PERF03:账本的两个范围索引改成覆盖索引。
--
-- 节点周期用量(预警、资产卡片、周期流量三个接口各算一遍)与用户区间流量都是
-- 「按 node_id 或 user_code + 时间范围,把 direction / delta_bytes 加起来」。
-- 原来的索引只有 (node_id, created_at) 与 (user_code, created_at),每一行都要再回表读
-- direction 与 delta_bytes:50 台机器、10 个用户、30 天逐小时的账本(72 万行)上,
-- 一次节点周期用量要 ~100ms,首屏三个接口加起来就是 300ms 的单连接独占。
-- 把这两列放进索引之后查询只读索引。原来的两个索引是新索引的前缀,能用它们的查询
-- 新索引一样能用,所以同名重建而不是另加一个 —— 少一个索引就少一份写入开销。
--
-- 不改任何算法:周期用量仍然只从 traffic_ledger 按时间范围现算(V2.1 的规矩)。
DROP INDEX idx_traffic_ledger_node;
CREATE INDEX idx_traffic_ledger_node ON traffic_ledger(node_id, created_at, direction, delta_bytes);

DROP INDEX idx_traffic_ledger_user;
CREATE INDEX idx_traffic_ledger_user ON traffic_ledger(user_code, created_at, node_id, direction, delta_bytes);
