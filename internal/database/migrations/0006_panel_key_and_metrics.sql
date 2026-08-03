-- 节点 SSH 私钥改为可选。
--
-- 面板现在持有一把自己的专用密钥(存在 system_settings 里,主密钥加密),
-- 新增节点时用一次性 root 口令或主控本机的私钥把它的公钥装进节点,
-- 之后所有操作都走这把专用密钥。nodes.ssh_key_encrypted 为空即表示"用面板密钥"。
--
-- SQLite 不支持修改列约束,但 ssh_key_encrypted 本来就是 TEXT NOT NULL DEFAULT '',
-- 空串即可表达"未单独配置",不需要改表结构。存量节点各自的密钥继续有效。

-- 节点资源采样。
--
-- 只保留一段滚动窗口:这是运维看趋势用的,不是计费数据,
-- 丢了不影响任何结算,而无节制增长会把 SQLite 撑大。
-- 采集间隔默认放得比流量同步宽(5 分钟),节点上每次只跑一条 shell,
-- 128MB 的小机器也吃得住。
CREATE TABLE node_metrics (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id        INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    -- CPU 使用率百分比,两次 /proc/stat 采样之差,保留一位小数后乘 10 存整数。
    cpu_percent    REAL    NOT NULL DEFAULT 0,
    mem_total_kb   INTEGER NOT NULL DEFAULT 0,
    mem_used_kb    INTEGER NOT NULL DEFAULT 0,
    -- 网络速率,字节/秒,取全部非回环网卡之和。
    net_rx_bps     INTEGER NOT NULL DEFAULT 0,
    net_tx_bps     INTEGER NOT NULL DEFAULT 0,
    load1          REAL    NOT NULL DEFAULT 0,
    uptime_seconds INTEGER NOT NULL DEFAULT 0,
    disk_total_kb  INTEGER NOT NULL DEFAULT 0,
    disk_used_kb   INTEGER NOT NULL DEFAULT 0,
    collected_at   TEXT    NOT NULL
);
CREATE INDEX idx_node_metrics_node_time ON node_metrics(node_id, collected_at);
