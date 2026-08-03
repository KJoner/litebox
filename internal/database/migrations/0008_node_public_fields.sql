-- 节点的对外字段:展示名称、排序、订阅开关、公开备注与维护说明。
--
-- 从此 name 只在管理后台出现,用户门户与所有订阅格式一律用 display_name。
-- 分开的理由:内部名称上往往写着机房、供应商、到期日甚至 IP 段,
-- 那是运维信息,不该随订阅发到用户设备上。

ALTER TABLE nodes ADD COLUMN display_name         TEXT    NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN sort_order           INTEGER NOT NULL DEFAULT 0;
-- 节点进维护时关掉它即可从新生成的订阅中移除,节点、历史流量与部署记录都保留。
ALTER TABLE nodes ADD COLUMN subscription_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE nodes ADD COLUMN public_remark        TEXT    NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN maintenance_message  TEXT    NOT NULL DEFAULT '';

-- 存量节点的展示名称先复制内部名称。
--
-- 不能留空:订阅里的节点名是客户端识别条目的依据,升级后突然变空或变成
-- "节点 3",客户端会把它当成一个新节点重复添加,老条目则永远留在列表里。
UPDATE nodes SET display_name = name WHERE display_name = '';
