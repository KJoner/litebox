-- V15:主机流量(vnStat)。
--
-- 这是**机器视角**的流量:网卡的收发字节,由节点上的 vnstatd 自己按 5 分钟落库,
-- 面板定期把它的 JSON 拉回来。与 traffic_ledger 那套**用户视角**(sing-box / mita
-- 的计数器)是两件事,对不上是正常的:主机流量还包括 SSH、apt、面板自己的同步,
-- 以及「不计流量」入口(迁移 0032)的流量 —— 那正是它存在的理由之一。
--
-- 三档粒度各存各的,不从小时聚合出日:vnstat 自己的日桶按节点本机时区切,
-- 而它只保留 4 天的小时数据 —— 聚合出来的日与 vnstat 的日在时区不是 UTC 的
-- 机器上会差几个小时。bucket_ts 是 vnstat 给的桶起点(unix 秒),渲染按 UTC。
--
-- 面板每次同步全量 upsert,保留期由面板决定(不删):vnstat 自己只留
-- 4 天小时、62 天日、25 个月月,而"这台机器去年 3 月跑了多少"是要答的问题。
CREATE TABLE host_traffic (
    node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    granularity TEXT    NOT NULL CHECK (granularity IN ('HOUR', 'DAY', 'MONTH')),
    bucket_ts   INTEGER NOT NULL,
    rx_bytes    INTEGER NOT NULL DEFAULT 0,
    tx_bytes    INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL,
    PRIMARY KEY (node_id, granularity, bucket_ts)
);

-- 每台机器一行:装没装、读的是哪块网卡、上次同步与上次的错误。
--
-- installed 是"面板确认过 vnstat 在这台机器上可用",定时同步只跑这一档的机器 ——
-- 没装的机器每分钟去 SSH 一次只会拿到一句 command not found。
-- iface 是同步与实时曲线共用的那块网卡(默认路由所在的那块),两处读同一个值,
-- 否则日/月的柱子与实时的曲线可能在说两块不同的网卡。
CREATE TABLE host_traffic_state (
    node_id        INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    installed      INTEGER NOT NULL DEFAULT 0,
    iface          TEXT    NOT NULL DEFAULT '',
    vnstat_version TEXT    NOT NULL DEFAULT '',
    synced_at      TEXT    NOT NULL DEFAULT '',
    last_error     TEXT    NOT NULL DEFAULT '',
    updated_at     TEXT    NOT NULL
);
