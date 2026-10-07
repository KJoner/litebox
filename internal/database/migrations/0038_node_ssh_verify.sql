-- V20:管理地址变更后的重新核验。
--
--   host_changed_at   最近一次改管理地址 / SSH 端口的时间。它之前的巡检、采样与探测
--                     结果说的是【旧地址】上的那台机器,界面上要标成「变更前数据」,
--                     不能被读成新地址的验证结果。
--   ssh_verify_state  '' 已验证(或从未改过);'PENDING' 管理员在新地址连不上的情况下
--                     仍然保存了,连接参数还没被验证过 —— 列表上不能显示成「连接正常」。
ALTER TABLE nodes ADD COLUMN host_changed_at TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN ssh_verify_state TEXT NOT NULL DEFAULT '' CHECK (ssh_verify_state IN ('', 'PENDING'));
