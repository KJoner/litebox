-- V15:sing-box 入口的「不计流量」开关。
--
-- 开着的入口渲染 users[] 时不写 name。sing-box 只在 name 非空时才设
-- metadata.User,而那正是 v2ray_api 建计数器的依据 —— 于是这个入口上的
-- 流量不产生任何 user>>> 计数器:不计入任何用户的额度,也不计入这台机器的
-- 周期用量与全站合计(机器视角的真相由 vnStat 主机流量给,见 0034)。
-- 凭据、撤销、订阅、门户全部照旧 —— 变的只有"没有计数器"这一件事。
--
-- 它**不是**去掉多用户配置:VLESS 没有"无 UUID"的模式,Shadowsocks 2022 的
-- 客户端密码是 serverPSK:userPSK 拼的,退成单用户之后用户手上那份全部连不上。
--
-- 默认 0(计量):存量入口一个字节都不变,compat_test 盯着。
ALTER TABLE node_inbounds ADD COLUMN unmetered INTEGER NOT NULL DEFAULT 0;

-- 节点上【当前正在生效】的那一份,只在部署成功时写入。
--
-- 采集要按它判:共享 psk 的 Snell 入口同时勾了不计流量时,inbound>>> 那份
-- 计数器也不能采 —— 而采集读的是节点上此刻那个进程的计数器,进程跑的是
-- 上一次部署下发的那份配置。按期望值判的话,刚改还没下发的那段时间要么
-- 静默丢一段、要么把一段记两遍。
ALTER TABLE node_inbounds ADD COLUMN deployed_unmetered INTEGER NOT NULL DEFAULT 0;
