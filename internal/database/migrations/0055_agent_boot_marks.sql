-- R08:Agent 样本按「哪一次开机」各记一个序号水位,丢弃数按累计值记。
--
-- 原来 node_agents 只记最后一个 (last_boot_id, last_seq),而且样本本身不带 boot_id、整批按上报时的
-- boot_id 算。两个洞:
--   * 宿主重启前没发出去的旧队列(A 那一次开机的 99、100)跟着新开机 B 的 1 一起上报,被当成 B 的
--     99、100 收下,B 的水位直接跳到 100 —— 之后 B 的 2…100 全被当成重复丢掉;
--   * 收过 A1、A2,再收 B1,然后重放 A1、A2:因为 A ≠ 最后那个 B,水位清零,A 的样本又入库一次。
-- 现在每个样本自带 boot_id,主控按 (node_id, boot_id) 各记一个水位,每台机器保留最近 16 次开机;
-- 更早的开机在 24 小时的样本时效之外,重放的也会按时效拒收。
CREATE TABLE agent_boot_marks (
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    boot_id    TEXT    NOT NULL,
    last_seq   INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (node_id, boot_id)
);
INSERT INTO agent_boot_marks (node_id, boot_id, last_seq, updated_at)
SELECT node_id, last_boot_id, last_seq, updated_at FROM node_agents WHERE last_boot_id <> '';

-- dropped_seen 是 Agent 上报的累计丢弃数上一次的值:应答丢了、Agent 重发同一份上报时,
-- 按差值累加,不会把同一批丢弃算两遍。
ALTER TABLE node_agents ADD COLUMN dropped_seen INTEGER NOT NULL DEFAULT 0;
