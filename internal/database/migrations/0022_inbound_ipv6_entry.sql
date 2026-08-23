-- 入口的 IPv6 条目:开关与独立名称。
--
-- 在此之前 IPv6 条目是订阅生成时对入口的【无条件】展开:只要这台机器填了
-- ipv6_address,它上面每个入口都会多出一条,名字严格是「展示名称-IPV6」
-- (subscription.IPv6NameSuffix)。那条规则当初写死的理由现在仍然成立 ——
-- 客户端靠节点名区分条目,改一次名字,所有已经导入订阅的人客户端里都会
-- 多出一份新节点,而旧的那份永远留在列表里、永远连得上。
--
-- 这一版把「谁都不能改」放宽成「改的人要知道后果」,而不是把那个代价
-- 当成不存在。两列的默认值因此选得很小心:
--
--   ipv6_enabled 默认 1 —— 默认 0 的话,升级完成的那一刻,全部双栈机器的
--   IPv6 条目会从所有人的订阅里同时消失,而管理员什么都没做过、
--   面板也不会报任何错。可用性的静默收缩与权限的静默放大一样坏。
--
--   ipv6_display_name 默认空串,表示「跟随 IPv4 名字 + -IPV6 后缀」,
--   **不在这里把当前算出来的名字固化进去**。理由与 public_port 存 0
--   表示跟随监听端口一字不差:固化之后管理员改了 IPv4 名字,IPv6 条目会
--   继续停在旧名上,而他看到的是一个已经有内容的输入框、不会想到那里
--   存着一个几个月前的值;而且固化之后,「改回跟随」这个动作再也没有
--   办法表达。回落只有 subscription.IPv6EntryName 一处实现。
--
-- 两列都只进订阅,一个字节都不进节点配置 —— 所以它们既不置 NeedsDeploy
-- 也不置 SSHChanged,与 V2.1「改 IPv6 不重启 sing-box」是同一条道理。

ALTER TABLE node_inbounds ADD COLUMN ipv6_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE node_inbounds ADD COLUMN ipv6_display_name TEXT NOT NULL DEFAULT '';
