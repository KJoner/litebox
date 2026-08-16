-- 节点入站的两个监听选项:TCP Fast Open 与 UDP 会话超时。
--
-- 三列的默认值都让存量节点渲染出的配置与升级前【逐字节相同】——
-- 两个字段在配置里都带 omitempty,取默认值时整项不出现。
-- 不这样的话,升级后十几台机器同时被判成"需要部署",而那次重启
-- 换不来任何配置变化,只会踢掉全部在线连接。

-- tcp_fast_open 是节点级开关,默认关。
--
-- 它必须两端一致才有意义:节点入站开了而客户端不请求,什么都不会发生;
-- 反过来客户端发带数据的 SYN 而服务端不认,白多一次回落。所以这一个开关
-- 同时控制两边 —— 拆成两个开关只是提供了一种把它配错的方式。
--
-- 默认关,也不按机器规格自动开:TFO 的成败取决于用户到节点这一段路径上的
-- 中间设备(不少会丢弃带数据的 SYN 或把 TFO 选项剥掉),而那条路径面板
-- 看不到 —— 探测是从节点本机做的,与用户走的路无关。此外 TFO cookie 是一个
-- 稳定的、可跨连接关联的标识,对一个刻意把流量伪装成正常 TLS 的节点来说
-- 是白送的指纹。开不开由管理员按机器决定。
ALTER TABLE nodes ADD COLUMN tcp_fast_open INTEGER NOT NULL DEFAULT 0;

-- 节点上【当前正在生效】的 TFO 状态,只在部署成功时写入。
--
-- 与 deployed_protocol 同一条道理:改开关到部署成功之间存在一个窗口。
-- 订阅若按期望值下发,这个窗口里客户端会对一个还没开 TFO 的服务端发
-- 带数据的 SYN,而数据库、节点、面板三方都是"对的"。
ALTER TABLE nodes ADD COLUMN deployed_tcp_fast_open INTEGER NOT NULL DEFAULT 0;

-- 节点内存,由探测写入,单位 MB。0 表示还没探测过。
--
-- 探测本来就读了这个值(ProbeResult.MemTotalMB),只是一直没落库。
-- 配置渲染要用它算 udp_timeout,而渲染必须是确定性的:不能去读
-- node_metrics 的最新采样 —— 那个值每五分钟变一次,而且采集可以整个关掉,
-- 配置哈希会跟着抖,「已同步」与「待部署」两个状态来回跳。
--
-- 0 时不写 udp_timeout,由 sing-box 用自己的默认值(5 分钟)。
-- 没探测过就不猜 —— 与 TCP 调优里"读不到内存就中止"是同一条规矩。
ALTER TABLE nodes ADD COLUMN mem_total_mb INTEGER NOT NULL DEFAULT 0;
