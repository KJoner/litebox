-- 节点中转(V7)。两种形态:
--
--   Nginx 透传   A 上跑 nginx stream,把 TCP 字节搬到 B。A 不解密、不认证、
--                不计流量。客户端与 B 之间的协议完全端到端,A 只是一跳。
--                一台 A 可以有 0..N 条(node_relays)。
--
--   sing-box 链式 A 是一个完整的落地节点,只是出站从 direct 换成指向 B。
--                客户端根本不知道 A 后面还有一跳。一台 A 至多一条
--                (nodes 上的 chain_* 列)。
--
-- 为什么中转主机复用 nodes 而不像 external_proxies 那样独立建表:
-- 那边独立的理由是「我们没有它的 SSH 和 root」,而中转主机恰恰相反 ——
-- 它需要 SSH、探测、安装、部署、监控、额度、访问等级,全是 nodes 已有的能力。

-- ---------- 节点角色 ----------

-- LANDING 落地节点。V7 之前的全部节点,行为一个字节不变。
-- RELAY   纯中转机。上面不跑 sing-box 服务,只跑 nginx。
--
-- 必须有这一列,而不是把中转机做成「用户数为 0 的落地节点」:后者会照常渲染
-- sing-box 配置、启服务、跑拨测,而一个空用户列表的 VLESS 入站谁都连不上,
-- 拨测必然失败、部署必然回滚 —— 管理员做的事情明明是对的。更根本的是,
-- 透传方案的全部价值就是「A 尽可能轻」,强迫每台中转机常驻一个 sing-box
-- 等于把这个价值取消掉。
--
-- 默认 LANDING:存量节点升级后渲染出的配置与升级前【逐字节相同】。
ALTER TABLE nodes ADD COLUMN role TEXT NOT NULL DEFAULT 'LANDING'
    CHECK (role IN ('LANDING', 'RELAY'));

-- ---------- sing-box 链式出站 ----------

-- 空串表示直连(默认)。存量节点取默认值时 Config.Route 整项不渲染,
-- 出站列表仍然只有一个 direct —— 否则升级后十几台机器同时被判成
-- 「需要部署」,而那次重启换不来任何配置变化,只会踢掉全部在线连接。
ALTER TABLE nodes ADD COLUMN chain_target_kind TEXT NOT NULL DEFAULT ''
    CHECK (chain_target_kind IN ('', 'NODE', 'EXTERNAL'));
ALTER TABLE nodes ADD COLUMN chain_target_node_id     INTEGER;
ALTER TABLE nodes ADD COLUMN chain_target_external_id INTEGER;

-- 链路凭据的计数器名,格式 chain_000001。空串表示还没分配过。
--
-- 它会作为一个用户出现在【落地 B】的 inbound.users 与 stats.users 里,
-- 因此是 B 的流量统计中的一个计数器名。由 system_settings 里的独立计数器
-- 分配,不复用、不从现存行推导 —— 复用会让新链路继承旧链路的 ledger 历史,
-- 与用户代码是同一条规矩。
--
-- 刻意用 chain_ 前缀而不是 user_:两个空间必须永远不撞,而「撞了会怎样」是
-- 一个真实用户的流量被算进链路、或者反过来,两种都不报错。
--
-- 为什么链路凭据不是 proxy_users 里的一行:那样每一处查用户的地方都要重新
-- 判断一次「这是不是链路凭据」—— 用户列表、门户、订阅、额度检查、到期检查、
-- 活跃用户数、批量操作、删除受影响节点。判断写漏一处,轻则一个不存在的
-- 「用户」出现在管理员列表里,重则额度检查把链路凭据停掉,整条链当场断掉
-- 而面板上一切正常。与门户认证「不做成同一套加个角色字段」是同一条道理。
ALTER TABLE nodes ADD COLUMN chain_code TEXT NOT NULL DEFAULT '';

-- 两套链路凭据都存,不按当前落地协议分叉 —— 与用户的两套凭据同理:
-- B 今天是 VLESS、明天改成 SS,链路不该因此重新签发。
-- 主密钥加密;空值必须存空串而不是加密后的空串(后者不为空,
-- 会被当成一把解不开的密钥,与 nodes.ssh_key_encrypted 的既有约定一致)。
ALTER TABLE nodes ADD COLUMN chain_uuid_encrypted        TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN chain_ss_password_encrypted TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_nodes_chain_node     ON nodes(chain_target_node_id)
    WHERE chain_target_kind = 'NODE';
CREATE INDEX idx_nodes_chain_external ON nodes(chain_target_external_id)
    WHERE chain_target_kind = 'EXTERNAL';

-- ---------- nginx 转发规则 ----------

CREATE TABLE node_relays (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    -- 中转主机 A。
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- 订阅与门户里显示的名字。**刻意没有内部名称字段** —— 不是「记得别填」,
    -- 而是压根没有位置可填。
    display_name TEXT NOT NULL,

    -- listen_port 是 nginx 在 A 上实际监听的端口;
    -- public_port 是客户端连接的公网端口,0 表示跟随 listen_port。
    --
    -- 两者必须分开,与 nodes 的两个端口是同一条规矩:NAT 小鸡上公网 443
    -- 映射到主机的 20443,把公网端口写进 nginx 的 listen 会让 nginx 监听在
    -- 转发链路另一端的号码上 —— nginx -t 通过、服务 active、端口监听检查
    -- 也通过(它查的就是那个错端口),只有用户连不上。
    --
    -- public_port 存 0 而不是写库时固化成当时的 listen_port:固化之后管理员
    -- 再改监听端口,订阅条目会继续停在旧端口上,而他当初看到的是一个空输入框。
    -- 解析放在订阅生成时,与 nodes.ipv6_proxy_port 一模一样。
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    public_port INTEGER NOT NULL DEFAULT 0
        CHECK (public_port = 0 OR public_port BETWEEN 1 AND 65535),

    -- 落地去向。两个目标列靠 CHECK 保证互斥:两列都填或都不填的行会让
    -- user_effective_relays 视图 JOIN 出重复或零行 —— 前者表现为订阅里
    -- 同一条线路出现两次,后者表现为线路凭空消失,两种都不报错。
    target_kind        TEXT NOT NULL CHECK (target_kind IN ('NODE', 'EXTERNAL')),
    target_node_id     INTEGER,
    target_external_id INTEGER,

    -- 这条线路自己的访问等级。不做 user_nodes 那种额外授权 —— 再加一张
    -- 关系表意味着「哪些人能看到这条线路」有两个来源,而两个来源迟早分叉。
    access_tier_id INTEGER NOT NULL DEFAULT 1,

    sort_order           INTEGER NOT NULL DEFAULT 0,
    subscription_enabled INTEGER NOT NULL DEFAULT 1,
    public_remark        TEXT    NOT NULL DEFAULT '',
    -- 关掉后 nginx 里不再渲染这个 server 块(与软删除不同:配置还留着)。
    enabled              INTEGER NOT NULL DEFAULT 1,

    deleted_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- 表级约束必须排在全部列定义之后(SQLite 一旦开始表约束就不能再回到列)。
    CHECK (
        (target_kind = 'NODE'     AND target_node_id     IS NOT NULL
                                  AND target_external_id IS NULL) OR
        (target_kind = 'EXTERNAL' AND target_external_id IS NOT NULL
                                  AND target_node_id     IS NULL)
    )
);

-- 同一台 A 上监听端口不能重复,否则 nginx 起不来而部署要到健康检查才发现。
CREATE UNIQUE INDEX idx_node_relays_listen
    ON node_relays(node_id, listen_port) WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_node     ON node_relays(node_id)     WHERE deleted_at IS NULL;
CREATE INDEX idx_node_relays_target   ON node_relays(target_node_id)
    WHERE deleted_at IS NULL AND target_kind = 'NODE';
CREATE INDEX idx_node_relays_external ON node_relays(target_external_id)
    WHERE deleted_at IS NULL AND target_kind = 'EXTERNAL';

-- ---------- 中转条目的可见性 ----------

-- 中转条目的可见性 = 规则自己的等级 ∩ 用户在落地 B 上确实有凭据。
--
-- 后半条是这个功能最容易写错、也最难看出来的地方:透传模式下客户端出示的是
-- 【B 的凭据】,一个在 B 上无效的用户拿到这条中转条目,订阅里看得见、
-- 连上去握手直接被拒 —— 正是 user_effective_nodes 那条约束在防的失效模式,
-- 而且这一次它跨了两台机器,排查的人会先去查 A。
--
-- b.subscription_enabled 刻意不参与:管理员把 B 的订阅开关关掉、只让用户经 A
-- 访问 B,是这个功能最典型的用法之一。拿订阅开关去卡中转条目,会让
-- 「隐藏落地机」这个动作顺手把中转线路也一起关掉,而他完全不会往那儿想。
-- 卡的是【凭据是否存在】,不是 B 自己进不进订阅。
--
-- b.deployed_config_sha256 != '' 必须有:没部署过的 B 上根本没有任何人的凭据。
--
-- 规则若要变更,必须新增迁移 DROP 后重建这个视图,不得改本文件。
CREATE VIEW user_effective_relays AS
    -- 落地是自建节点
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays  r
      JOIN nodes        a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users  u  ON u.deleted_at IS NULL
      JOIN access_tiers ut ON ut.id = u.access_tier_id
      JOIN access_tiers rt ON rt.id = r.access_tier_id
      JOIN nodes        b  ON b.id = r.target_node_id AND b.deleted_at IS NULL
      JOIN user_effective_nodes en_b
             ON en_b.node_id = b.id AND en_b.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'NODE'
       AND rt.level <= ut.level
       AND b.status != 'DISABLED'
       AND b.deployed_config_sha256 != ''
    UNION
    -- 落地是外部代理
    SELECT u.id AS proxy_user_id, r.id AS relay_id
      FROM node_relays      r
      JOIN nodes            a  ON a.id = r.node_id AND a.deleted_at IS NULL
      JOIN proxy_users      u  ON u.deleted_at IS NULL
      JOIN access_tiers     ut ON ut.id = u.access_tier_id
      JOIN access_tiers     rt ON rt.id = r.access_tier_id
      JOIN external_proxies p  ON p.id = r.target_external_id AND p.deleted_at IS NULL
      JOIN user_effective_external_proxies en_p
             ON en_p.external_proxy_id = p.id AND en_p.proxy_user_id = u.id
     WHERE r.deleted_at IS NULL
       AND r.target_kind = 'EXTERNAL'
       AND rt.level <= ut.level;

-- ---------- 链路凭据的代码计数器 ----------

-- 与 user_code_sequence 分开:两个空间必须永远不撞。
INSERT INTO system_settings (key, value, updated_at)
VALUES ('chain_code_sequence', '0', datetime('now'));

-- ---------- 部署记录区分种类 ----------

-- 同一台机器上有两种互不相干的下发:sing-box 配置(重启服务)与
-- nginx 转发配置(reload,不打断在途连接)。混在一张表里而不加以区分的话,
-- 部署记录页上会出现两条相邻的记录,一条写着"重启",一条写着"reload",
-- 而管理员分不清刚才那次断线是哪一条造成的。
--
-- 默认 SINGBOX:存量记录本来就全是 sing-box 部署。
ALTER TABLE deployments ADD COLUMN kind TEXT NOT NULL DEFAULT 'SINGBOX'
    CHECK (kind IN ('SINGBOX', 'RELAY'));
