-- 固定节点的 SSH 主机公钥(TOFU)。
--
-- 主控持有节点的 root 权限,若被中间人接管等同于全部节点失守,
-- 因此不能使用 InsecureIgnoreHostKey。首次连接时记录主机公钥,
-- 之后每次连接严格比对,不一致即拒绝连接。
ALTER TABLE nodes ADD COLUMN ssh_host_key TEXT NOT NULL DEFAULT '';

-- 部署事务需要单调递增的 revision。用节点级计数器而非全局自增,
-- 便于在单个节点的历史里直接读出第几版。
ALTER TABLE nodes ADD COLUMN config_revision INTEGER NOT NULL DEFAULT 0;

-- 记录最近一次成功部署的配置哈希,用于跳过无变化的重复部署。
ALTER TABLE nodes ADD COLUMN deployed_config_sha256 TEXT NOT NULL DEFAULT '';
