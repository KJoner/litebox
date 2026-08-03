-- 把"客户端连接的端口"与"sing-box 在节点上监听的端口"拆成两列。
--
-- NAT 主机与自建 nginx/stream 转发的场景里两者不同:公网 443 转发到主机 20443。
-- 此前只有 proxy_port 一列同时充当两个角色,这类节点无法配置。
--
-- 列语义:
--   proxy_port  —— 公网代理端口,写进订阅链接,面板自己不监听它;
--   listen_port —— 主机代理端口,sing-box 的 inbound.listen_port,健康检查也查它。
--
-- 存量节点两者本就相同,回填后行为与升级前完全一致。
ALTER TABLE nodes ADD COLUMN listen_port INTEGER NOT NULL DEFAULT 0;

UPDATE nodes SET listen_port = proxy_port;
