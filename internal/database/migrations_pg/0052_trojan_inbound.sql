-- (PostgreSQL 版,与 SQLite 的 0052 同一份逻辑结构)
-- VNext PROTO02:node_inbounds.protocol 多收一个 TROJAN。
-- PG 可以直接换约束,不用像 SQLite 那样重建整张表;约束名是基线里未命名 CHECK 的默认名。
ALTER TABLE node_inbounds DROP CONSTRAINT IF EXISTS node_inbounds_protocol_check;
ALTER TABLE node_inbounds ADD CONSTRAINT node_inbounds_protocol_check
    CHECK (protocol IN ('VLESS_REALITY', 'SHADOWSOCKS', 'SNELL', 'TROJAN'));
