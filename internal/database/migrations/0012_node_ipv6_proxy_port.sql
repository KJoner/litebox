-- IPv6 条目在订阅里使用的公网端口。
--
-- 0 表示"跟随 IPv4 的 proxy_port"。**这个 0 必须原样留在库里,不能在写入时
-- 就解析成当时的 proxy_port** —— 那样以后把 IPv4 公网端口从 443 改成 8443,
-- IPv6 条目会继续停在 443 上,而管理员当初看到的是一个空输入框,
-- 完全不会想到那里固化了一个值。解析放在订阅生成时做。
--
-- 为什么需要它:双栈机器的两个协议栈未必映射到同一个外部端口。
-- NAT 小鸡尤其常见 —— IPv4 是服务商映射的高位端口,IPv6 是直连的 443。
-- 而 listen_port(sing-box 实际监听的那个)只有一个,两条链路指向同一个入站。
ALTER TABLE nodes ADD COLUMN ipv6_proxy_port INTEGER NOT NULL DEFAULT 0
    CHECK (ipv6_proxy_port BETWEEN 0 AND 65535);
