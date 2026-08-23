-- Mieru 入口的出口去向(V13 第三步)。
--
-- **这一版推翻了迁移 0024 里那句「mieru 入口不能被链式指向,自己也没有出口去向」。**
-- 当时的判断基于「sing-box 没有 mieru 出站」,那一半仍然成立(mieru 入口确实
-- 不能当别人的落地);但反过来那一半错了 —— mita 自己带 egress 代理,
-- 真机实测「一台机器上三个 mieru 入口,分别直连 / 走 VLESS 落地 / 走 SS 落地」
-- 三条并存且各自计数正确。
--
-- ---------- 链路是怎么走的 ----------
--
--   用户 ──mieru──► mita 实例 ──socks5──► 本机 sing-box 的 socks 入站
--                                          └─(route.rules 按入站 tag 分流)
--                                             └──► chain 出站(VLESS/SS)──► 落地
--
-- 中间那一跳 socks5 是**上游定死的**:`ProxyProtocol` 枚举里只有
-- SOCKS5_PROXY_PROTOCOL 一个值,mita 的 egress 拨不出 VLESS 或 Shadowsocks。
-- 好处是一个 sing-box 进程就能服务这台机器上全部链式 mieru 入口 ——
-- 按入站 tag 分流是 V8 已经在做的事,不需要新机制。
--
-- ---------- 为什么出口要挂在【入口】上而不是机器上 ----------
--
-- mita 的 egress 是**实例级**的:`ServerConfig.egress` 挂在整份配置上,
-- 而 `EgressRule` 只按目的地(ipRanges / domainNames)匹配 —— `PortBinding`
-- 里没有 egress 字段,`User` 里也没有。所以「入口 1 直连、入口 2 走 A」
-- 在单个 mita 实例里表达不出来。
--
-- **于是一个 mieru 入口 = 一个 mita 实例。** 这不是我们选的粒度,
-- 是上游的数据模型逼出来的。代价是每实例约 13MB 内存(实测,跑过流量后 17MB),
-- 界面上要按机器内存给出提示。

-- egress_socks_port 是 mita 与本机 sing-box 之间那一跳的回环端口。
--
-- 存 0 表示**直连**(这个入口的 mita 配置里整个 egress 段不渲染)。
-- 它一律监听 127.0.0.1 —— 绑到 :: 上等于在公网开了一个无认证的 socks5 代理。
--
-- 它照样要进端口冲突检测:回环端口一样会与 V2Ray API 的端口、
-- 别的 mieru 入口的 socks 端口撞车,而撞车的表现是 sing-box 起不来。
ALTER TABLE node_mieru_inbounds ADD COLUMN egress_socks_port INTEGER NOT NULL DEFAULT 0;

-- 链式去向。三列的含义与 node_inbounds 上那三列一字不差
-- (迁移 0019),取值 '' / 'INBOUND' / 'EXTERNAL' 由 Go 侧校验 ——
-- ALTER TABLE ADD COLUMN 加不了表级 CHECK,而列级 CHECK 在这里没有意义
-- (三列之间的一致性是一个跨列约束)。
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_inbound_id INTEGER;
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_target_external_id INTEGER;

-- 链路凭据。与 node_inbounds.chain_* 完全同构:它作为一个用户出现在
-- 【落地入站】的 users 与 stats.users 里,名字是 chain_000001 这种独立计数器,
-- 不是 proxy_users 里的一行 —— 放进用户表的话,额度检查会把链路凭据停掉,
-- 整条链当场断掉而面板上一切正常。
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_code TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_uuid_encrypted TEXT NOT NULL DEFAULT '';
ALTER TABLE node_mieru_inbounds ADD COLUMN chain_ss_password_encrypted TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_node_mieru_chain_inbound ON node_mieru_inbounds(chain_target_inbound_id)
    WHERE chain_target_kind = 'INBOUND';
CREATE INDEX idx_node_mieru_chain_external ON node_mieru_inbounds(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';

-- config_in_ram 那条约束对 mieru 同样成立,但 **mita 的配置不跟着进内存**:
-- 它由 mita 自己写成 protobuf 存到 MITA_CONFIG_FILE,面板下发的是一份临时
-- JSON(apply 完就能删)。真正长期躺在磁盘上的是 mita 自己那份 .pb ——
-- 那里面只有 hashedPassword(sha256),不是明文口令,实测 describe config
-- 里 password 字段是空的。所以它与 sing-box 的 config.json 不是同一档风险,
-- 这一版不为它做「不落盘」。界面上要写明这个差别,不然管理员会以为
-- 开了「配置不落盘」之后这台机器上就没有 mieru 凭据了。
