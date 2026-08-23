-- Mieru 落地协议(V13):第三类入口。
--
-- **它不是 node_inbounds 的一种 protocol。** 那张表的每一行都渲染成
-- sing-box 配置里的一个 inbound,而 mieru 的服务端是另一个进程(mita):
-- 凭据靠 `mita apply config` 下发、流量走另一条管理通道(Unix socket 上的
-- gRPC)、计数器语义也不一样(持久化、跨重启保留)。混进去的话,配置渲染、
-- 部署事务、拨测、流量采集四条路径每一处都要先判断"这一行是不是真的入站",
-- 而判断写漏的表现是渲染器把一个 mieru 入口当成 sing-box 入站写进 config.json
-- —— sing-box 认不出 type,服务起不来,而报错指向的是别的入口。
--
-- 与 node_relays 是同一条道理的第三次应用:**数据层三张表,界面上一张列表**。
-- 三类入口回答的是同一个问题(用户连哪个端口、连上之后去哪),
-- 分成三处看才拼得出这台机器的全貌;但它们改动之后的后果完全不同
-- (sing-box 要重启、nginx 只 reload、mita 是 apply+reload),
-- 所以操作按钮与确认档次仍然各走各的。
--
-- ---------- 端口:这一版唯一真正新的东西 ----------
--
-- 多端口跳跃是 mieru 的主要抗封锁特性,所以端口在这里是【范围】而不是单值。
-- 三层的含义与 node_inbounds 那三列一一对应,只是每层都变成了一对起止:
--
--   listen_port_start/end        mita 实际监听的一批端口
--   public_port_start/end        IPv4 条目在订阅里用的端口,0 表示跟随监听
--   ipv6_public_port_start/end   IPv6 条目用的端口,0 表示跟随 IPv4 条目
--
-- start = end 表示只有一个端口 —— 订阅那一侧据此二选一:相等时渲染成
-- mihomo 的 `port: N`,不等时渲染成 `port-range: A-B`(两者在 mihomo 里
-- **互斥**,同时出现会被拒)。监听侧不做这个区分,它总是一批。
--
-- **订阅范围与监听范围刻意不要求相等,也不要求包含。** NAT 机器上服务商
-- 映射的外部端口段与本机监听段完全可以是两个不相干的号码段
-- (40000-40010 → 30000-30010),而那正是 public_port 这一层存在的理由。
-- 加一条"必须相等"的校验会让那种机器一个入口都配不出来。
--
-- **端口冲突检测落在 Go 侧,不在这里。** SQLite 的唯一索引表达不了区间重叠,
-- 而这台机器上真正会撞的是三类入口的端口:node_inbounds.listen_port(单值)、
-- node_relays.listen_port(单值)、以及这里的一段区间。三者两两都要查,
-- 而且新建 sing-box 入站与转发规则时也要反过来查有没有落进某个 mieru 区间 ——
-- 只查一半的表现是 mita 或 sing-box 其中一个 bind 失败,整个服务起不来,
-- 而要到部署的健康检查才发现,那时配置已经换过去了。
--
-- ---------- 不做的事 ----------
--
-- **mieru 入口不能当中转(nginx 透传)的落地。** nginx stream 只渲染 TCP 的
-- server 块,而 mieru 的 UDP 传输透传不了;更根本的是端口跳跃与单端口转发
-- 对不上 —— 一条 proxy_pass 只指一个上游端口,而客户端会在整段里跳。
-- 所以 node_relays.target_kind 不新增取值,选落地时压根不列 mieru 入口。
--
-- 链式出站同理不支持:sing-box 拨不动 mieru(它没有 mieru 出站),
-- 所以 mieru 入口既不能被 chain_target_inbound_id 指向,自己也没有出口去向。

CREATE TABLE node_mieru_inbounds (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 订阅与门户里显示的名字。与另外两张入口表一样,**刻意没有内部名称字段**。
    display_name TEXT NOT NULL,

    -- 监听端口段。start = end 表示只有一个端口。
    listen_port_start INTEGER NOT NULL CHECK (listen_port_start BETWEEN 1 AND 65535),
    listen_port_end   INTEGER NOT NULL CHECK (listen_port_end   BETWEEN 1 AND 65535),

    -- 订阅端口段。两个都为 0 表示跟随监听段,解析放在订阅生成时 ——
    -- 与 node_inbounds.public_port 存 0 一字不差的理由:写库时固化成当时的
    -- 监听段之后,管理员再改监听段,订阅条目会继续停在旧号码上,
    -- 而他当初看到的是两个空输入框。
    public_port_start INTEGER NOT NULL DEFAULT 0
        CHECK (public_port_start = 0 OR public_port_start BETWEEN 1 AND 65535),
    public_port_end   INTEGER NOT NULL DEFAULT 0
        CHECK (public_port_end   = 0 OR public_port_end   BETWEEN 1 AND 65535),

    -- IPv6 条目:开关、独立名称与独立端口段,与迁移 0022 给 sing-box 入站
    -- 做的那一套完全同构(默认 1 / 空串,理由见 0022)。
    ipv6_enabled           INTEGER NOT NULL DEFAULT 1,
    ipv6_display_name      TEXT    NOT NULL DEFAULT '',
    ipv6_public_port_start INTEGER NOT NULL DEFAULT 0
        CHECK (ipv6_public_port_start = 0 OR ipv6_public_port_start BETWEEN 1 AND 65535),
    ipv6_public_port_end   INTEGER NOT NULL DEFAULT 0
        CHECK (ipv6_public_port_end   = 0 OR ipv6_public_port_end   BETWEEN 1 AND 65535),

    -- 传输层。mieru 自己在 UDP 与 TCP 上都能跑,取值直接用上游的写法
    -- (大写 TCP/UDP)—— 它同时是 mita 配置里 portBindings.protocol 的值
    -- 与 mihomo 里 transport 的值,换一种拼写就要在两处各翻一次。
    transport TEXT NOT NULL DEFAULT 'TCP' CHECK (transport IN ('TCP', 'UDP')),

    -- 多路复用档位。取值同样照抄上游,理由同上。
    -- 默认 LOW 与 mieru 自己的默认一致 —— 不主动挑一个"更好"的值:
    -- 档位越高越省握手,但也越容易在流量特征上聚成一团。
    multiplexing TEXT NOT NULL DEFAULT 'MULTIPLEXING_LOW'
        CHECK (multiplexing IN ('MULTIPLEXING_OFF', 'MULTIPLEXING_LOW',
                                'MULTIPLEXING_MIDDLE', 'MULTIPLEXING_HIGH')),

    -- MTU 只对 UDP 传输有意义,0 表示不写、用 mieru 的默认值(1400)。
    -- 与 udp_timeout 那条同理:写一个与默认值相同的数字,行为一个字节不变,
    -- 却会让配置每次都被判成"变了"。
    mtu INTEGER NOT NULL DEFAULT 0 CHECK (mtu = 0 OR mtu BETWEEN 1280 AND 1500),

    -- 访问等级挂在入口上,与 node_inbounds 同(迁移 0020)。
    -- 没有外键,access.Store.Validate 是唯一拦截点。
    access_tier_id INTEGER NOT NULL DEFAULT 1,

    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,

    -- 节点上【当前正在生效】的那几项,只在部署成功时写入。
    --
    -- deployed_transport 为空串表示「这个入口还没真正上过节点」,订阅据此过滤,
    -- 与 node_inbounds.deployed_protocol 一模一样的用法。**订阅只反映这几列**
    -- —— 改配置到部署成功之间有一个窗口(可能是永远,如果部署失败),
    -- 按期望值渲染会让用户拉到一份与节点上不符的参数,而数据库、节点、面板
    -- 三方都是"对的",只有订阅站在中间说了假话。
    --
    -- 监听段也要存一份已生效的:订阅端口留空时回落到它,而回落到【期望的】
    -- 监听段会在改端口的窗口里下发一批还没人监听的号码。
    deployed_transport         TEXT    NOT NULL DEFAULT '',
    deployed_multiplexing      TEXT    NOT NULL DEFAULT '',
    deployed_mtu               INTEGER NOT NULL DEFAULT 0,
    deployed_listen_port_start INTEGER NOT NULL DEFAULT 0,
    deployed_listen_port_end   INTEGER NOT NULL DEFAULT 0,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- 起止不能倒过来。倒过来的区间在 mita 那边是一个空集合,
    -- 服务照常起来但一个端口都不听,而面板显示"已部署"。
    CHECK (listen_port_end >= listen_port_start),
    CHECK (public_port_end >= public_port_start),
    CHECK (ipv6_public_port_end >= ipv6_public_port_start),
    -- 订阅端口段要么两个都是 0(跟随),要么两个都非 0。只填一半的话
    -- 「跟随」与「指定」两种含义会在同一行里同时成立,而渲染时只能挑一个。
    CHECK ((public_port_start = 0) = (public_port_end = 0)),
    CHECK ((ipv6_public_port_start = 0) = (ipv6_public_port_end = 0))
);

CREATE INDEX idx_node_mieru_inbounds_node ON node_mieru_inbounds(node_id)
    WHERE deleted_at IS NULL;

-- ---------- 用户凭据 ----------
--
-- mieru 的用户是 {name, password} 两项,没有像 SS2022 那样的服务端 PSK ——
-- 客户端用的就是这个 password 本身。name 用面板的用户代码(user_000001),
-- 与 sing-box 那一侧同一个口径:同一个用户在同一台机器上的流量因此合并到
-- 同一条 traffic_ledger 记录上,而那正是入账要的口径。
--
-- 与 ss_password_encrypted 并列而不是复用它:两套凭据各管一种协议,
-- 复用的话,重置其中一种会连带作废另一种,而管理员点的是"重置 Shadowsocks 密码"。
-- 存量行留空,由服务启动时的一次性 backfill 补齐(主密钥在 Go 侧,
-- 迁移里生成不了;也不在渲染路径上懒补 —— 那会把只读的配置比对变成数据变更)。
ALTER TABLE proxy_users ADD COLUMN mieru_password_encrypted TEXT NOT NULL DEFAULT '';

-- ---------- 可见性 ----------
--
-- 与 user_effective_inbounds 同构:建立在 user_effective_nodes 之上
-- (那一层含 user_nodes 的额外授权,重写等级条件必然漏掉它),
-- 再按入口自己的等级收一次。
--
-- 规则若要变更,必须新增迁移 DROP 后重建,不得改本文件。
CREATE VIEW user_effective_mieru_inbounds AS
    SELECT en.proxy_user_id AS proxy_user_id,
           i.id             AS mieru_inbound_id,
           i.node_id        AS node_id
      FROM node_mieru_inbounds i
      JOIN user_effective_nodes en ON en.node_id = i.node_id
      JOIN proxy_users  u  ON u.id = en.proxy_user_id
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers it ON it.id = i.access_tier_id
     WHERE i.deleted_at IS NULL
       AND i.enabled = 1
       AND it.level <= ut.level;
