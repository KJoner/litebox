-- V16:一台机器多个订阅地址,每个入口按地址各配端口与订阅名。
--
-- 在此之前一台机器的订阅地址是固定的两栏:sub_ipv4_address(空则跟随 host)
-- 与 ipv6_address,每个入口再各带一个 public_port 与一组 IPv6 字段。现在改成:
--
--   node_addresses     一台机器的【额外】订阅地址池(host 之外的 IPv4 / IPv6)。
--                      host 仍是唯一的管理通道,同时兼作默认 IPv4(NULL 地址引用)。
--   inbound_endpoints  一个入口在订阅里下发的每一条地址(选了哪个地址、公网端口、
--                      订阅里显示的名字)。一个入口可以有多条。
--
-- 与旧模型的关系:旧的单个 sub_ipv4_address / ipv6_address 迁成 node_addresses
-- 的第一行;每个入口的 public_port / ipv6_* 迁成一到两条默认 endpoint。回填的
-- 目标是**升级后订阅逐字节不变**:一个只有 host + 单 IPv6 的入口,渲染出来的
-- 两条(IPv4 + IPv6)与迁移前完全一样。渲染期若某入口一条 endpoint 都没有,
-- 兜底合成「host + 跟随端口 + 跟随名」——所以老代码路径新建的入口也不会消失。

-- ---------- 地址池 ----------
--
-- 存无方括号的标准化地址或域名(动态 DNS),与 nodes.ipv6_address 同一规矩:
-- 方括号是 URI 语法的一部分,由 hostForURI 按需要加。family 决定订阅名默认后缀
-- (V6 加 -IPV6)与——仅字面量时——URI 里的方括号(方括号仍按内容判定,
-- 域名不加)。ON DELETE CASCADE:删地址连带撤掉引用它的 endpoint,
-- 那正是「清空某个地址就把它从订阅撤下来」。
CREATE TABLE node_addresses (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    family     TEXT    NOT NULL CHECK (family IN ('V4', 'V6')),
    address    TEXT    NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);
CREATE INDEX idx_node_addresses_node ON node_addresses(node_id);

-- ---------- 入口的订阅地址条目 ----------
--
-- entry_kind + entry_id 指向四类入口之一(不做外键:它们在三张表里,
-- 与 external_proxies 复用一张表同一条道理)。node_id 冗余存一份并挂 CASCADE,
-- 是为了机器被硬删时能连带清掉这些条目 —— entry_id 没有外键兜不住这件事。
-- address_id 为 NULL 表示管理 IP(host);否则引用 node_addresses,同样 CASCADE。
--
-- public_port 为 0 表示跟随入口的监听端口(单端口的 SINGBOX / NGINX / REALM);
-- Mieru 是端口段,public_port 是段起点、public_port_end 是段终点,两端都为 0
-- 表示跟随监听段。display_name 为空表示跟随入口名(V6 条目加 -IPV6 后缀)。
CREATE TABLE inbound_endpoints (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id         INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    entry_kind      TEXT    NOT NULL CHECK (entry_kind IN ('SINGBOX', 'MIERU', 'NGINX', 'REALM')),
    entry_id        INTEGER NOT NULL,
    address_id      INTEGER REFERENCES node_addresses(id) ON DELETE CASCADE,
    public_port     INTEGER NOT NULL DEFAULT 0,
    public_port_end INTEGER NOT NULL DEFAULT 0,
    display_name    TEXT    NOT NULL DEFAULT '',
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);
CREATE INDEX idx_inbound_endpoints_entry ON inbound_endpoints(entry_kind, entry_id);
CREATE INDEX idx_inbound_endpoints_node  ON inbound_endpoints(node_id);

-- ---------- 回填:旧的单个订阅 IPv4 / IPv6 迁成地址池的第一行 ----------
INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
SELECT id, 'V4', sub_ipv4_address, 0,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM nodes
 WHERE sub_ipv4_address != '' AND deleted_at IS NULL;

INSERT INTO node_addresses (node_id, family, address, sort_order, created_at, updated_at)
SELECT id, 'V6', ipv6_address, 0,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM nodes
 WHERE ipv6_address != '' AND deleted_at IS NULL;

-- ---------- 回填:SINGBOX 入口 ----------
-- V4:sub_ipv4_address 非空时指向那条迁移过来的额外 IPv4,否则 NULL(host)——
-- 与旧的 SubscriptionIPv4(host, sub_ipv4) 解析出的地址完全一致。
-- 软删除的入口不回填(它不会再进订阅,也不会被重新启用)。停用(enabled=0)的
-- 要回填 —— 它可能被重新打开。
INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT i.node_id, 'SINGBOX', i.id,
       CASE WHEN n.sub_ipv4_address != '' THEN
            (SELECT a.id FROM node_addresses a
              WHERE a.node_id = n.id AND a.family = 'V4' AND a.address = n.sub_ipv4_address
              LIMIT 1)
       ELSE NULL END,
       i.public_port, 0, '', 0,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_inbounds i JOIN nodes n ON n.id = i.node_id
 WHERE i.deleted_at IS NULL AND n.deleted_at IS NULL;

-- V6:仅当机器填了 IPv6 且这个入口开着 ipv6_enabled(V11)。名字取入口的
-- ipv6_display_name 原始值(空则渲染期回落到入口名 + 后缀)。
INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT i.node_id, 'SINGBOX', i.id,
       (SELECT a.id FROM node_addresses a
         WHERE a.node_id = n.id AND a.family = 'V6' AND a.address = n.ipv6_address
         LIMIT 1),
       i.ipv6_public_port, 0, i.ipv6_display_name, 1,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_inbounds i JOIN nodes n ON n.id = i.node_id
 WHERE i.deleted_at IS NULL AND n.deleted_at IS NULL
   AND n.ipv6_address != '' AND i.ipv6_enabled = 1;

-- ---------- 回填:MIERU 入口(端口是段) ----------
INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT m.node_id, 'MIERU', m.id,
       CASE WHEN n.sub_ipv4_address != '' THEN
            (SELECT a.id FROM node_addresses a
              WHERE a.node_id = n.id AND a.family = 'V4' AND a.address = n.sub_ipv4_address
              LIMIT 1)
       ELSE NULL END,
       m.public_port_start, m.public_port_end, '', 0,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_mieru_inbounds m JOIN nodes n ON n.id = m.node_id
 WHERE m.deleted_at IS NULL AND n.deleted_at IS NULL;

INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT m.node_id, 'MIERU', m.id,
       (SELECT a.id FROM node_addresses a
         WHERE a.node_id = n.id AND a.family = 'V6' AND a.address = n.ipv6_address
         LIMIT 1),
       m.ipv6_public_port_start, m.ipv6_public_port_end, m.ipv6_display_name, 1,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_mieru_inbounds m JOIN nodes n ON n.id = m.node_id
 WHERE m.deleted_at IS NULL AND n.deleted_at IS NULL
   AND n.ipv6_address != '' AND m.ipv6_enabled = 1;

-- ---------- 回填:中转规则(nginx / realm) ----------
-- 「指定地址」(target_kind=ADDRESS)不进订阅,不给它 endpoint。V6 端口跟随
-- (旧行为里中转的 IPv6 端口写死 0),名字跟随(中转无 per-relay IPv6 名)。
INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT r.node_id, r.engine, r.id,
       CASE WHEN n.sub_ipv4_address != '' THEN
            (SELECT a.id FROM node_addresses a
              WHERE a.node_id = n.id AND a.family = 'V4' AND a.address = n.sub_ipv4_address
              LIMIT 1)
       ELSE NULL END,
       r.public_port, 0, '', 0,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_relays r JOIN nodes n ON n.id = r.node_id
 WHERE r.deleted_at IS NULL AND n.deleted_at IS NULL AND r.target_kind != 'ADDRESS';

INSERT INTO inbound_endpoints
    (node_id, entry_kind, entry_id, address_id, public_port, public_port_end,
     display_name, sort_order, created_at, updated_at)
SELECT r.node_id, r.engine, r.id,
       (SELECT a.id FROM node_addresses a
         WHERE a.node_id = n.id AND a.family = 'V6' AND a.address = n.ipv6_address
         LIMIT 1),
       0, 0, '', 1,
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM node_relays r JOIN nodes n ON n.id = r.node_id
 WHERE r.deleted_at IS NULL AND n.deleted_at IS NULL
   AND r.target_kind != 'ADDRESS' AND n.ipv6_address != '';
