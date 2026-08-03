# LiteBox 项目约束

本文件是 LiteBox 的强制开发约束。所有条款都必须遵守。
技术依据见 `docs/Phase0-技术验证报告.md`,详细计划见 `docs/LiteBox-Panel-V1-开发计划-v2.md`。

## 项目定位

面向个人使用的轻量级 sing-box 管理面板。最多 10 个用户,多台 Linux VPS,
节点可低至 128MB 内存。V1 只支持 VLESS + REALITY + XTLS Vision。

## 架构约束

* 不引入 MySQL、PostgreSQL、Redis 或消息队列,只用 SQLite;
* 不引入微服务,保持 Go 单体;
* 不在节点运行常驻自研 Agent,节点只有 sing-box + systemd;
* 节点不依赖 Docker、数据库或 Node.js;
* 不增加商业机场功能(注册、套餐、订单、支付、优惠券、邀请、工单);
* 不以未来扩展为理由提前引入复杂框架。

## sing-box 相关约束(来自 Phase 0 实测)

以下每一条都对应一个不会报错、不会告警的失效模式,必须严格遵守。

* **不得把 `sing-box check` 当作配置正确性的唯一保证。**
  实测它不校验 UUID 格式(任意字符串会被 `uuid.NewV5` 哈希成合法 UUID),
  也不校验 `flow`(只在连接时校验)。UUID、flow、用户代码格式必须由面板自行强校验。

* **部署健康检查必须包含真实 VLESS 拨测。**
  不得仅凭 systemd 状态与端口监听判定部署成功。实测 flow 写错时,
  check 通过、systemd active、端口在监听,但全部用户断线。

* **`inbound.users[].name` 与 `experimental.v2ray_api.stats.users`
  必须由同一份用户列表渲染,渲染后断言两个集合相等。**
  实测统计白名单缺项会导致该用户能正常上网但零流量记录,无任何报错。

* **节点重启判定必须使用 `GetSysStats.Uptime`,不得只依赖"计数器变小"。**
  实测漏判会漏算整个重启前的计数值(不是几秒流量)。
  面板自身触发的重启必须在部署事务内显式重置基线。

* **流量同步读取失败必须在进入数据库事务前返回**,
  任何情况下不得把用户流量归零。

* **重启前的强制同步必须在取得节点连接锁之前执行。**
  同步本身也要经 `pool.Do` 读节点 API,而节点级互斥锁不可重入,
  放进部署事务内部会自我死锁。

* **同步失败是否中止部署,取决于节点上的 sing-box 是否正在运行。**
  在运行则必须中止(计数器里有尚未落库的流量,重启会让它永久丢失);
  未运行则跳过并继续 —— 计数器早已随进程消失,此时中止只会让
  崩溃的节点与全新节点永远无法通过部署恢复。

* **REALITY 握手目标必须经过实测校验**:TLS 1.3 + X25519 + 每个 TLS 记录 ≤ 8192 字节。
  检测必须通过 SSH 在节点本机执行(CDN 按地域下发不同证书链)。

* **节点 sing-box 构建标签固定为** `with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0`,
  不使用上游默认标签。版本固定,禁止使用 `latest`。

* **SSH 连接必须按节点复用长连接**,不得每次同步重新建连(建连约 1.3s,单次调用约 157ms)。

* V2Ray Stats gRPC 的服务名是 `v2ray.core.app.stats.command.StatsService`,
  与 sing-box 自带 stub 的路径不一致,必须用 `conn.Invoke` 显式指定方法名。

## 工程约束

* 不假设 VLESS 用户可动态热更新,用户变化必须通过配置生成和安全重启生效;
* 所有重启前必须先同步流量,同步失败必须中止部署;
* 所有配置替换前必须执行 `sing-box check`;
* 所有部署失败必须自动回滚;
* 不通过字符串拼接生成 JSON,必须用结构体序列化;
* 不通过字符串拼接执行远程 shell,所有远程参数严格校验;
* 所有数据库变更必须使用迁移,不得修改已应用的迁移文件(迁移框架会用哈希拦截);
* 每个阶段完成后必须运行测试并更新文档。

## 代码约定

* 所有时间存 UTC 的 RFC3339 字符串;
* 流量单位统一字节;
* 需要还原的敏感字段(用户 UUID、节点 REALITY 私钥)用主密钥加密存储;
* 订阅 Token 只存 SHA-256 哈希;
* 用户删除优先软删除;
* 用户代码格式 `user_000001`,一经分配不可变更、**不可复用**
  (它是流量统计的唯一标识;复用会让新用户继承旧用户的 ledger 历史。
  因此由 system_settings 中的独立计数器分配,不从现存行推导);
* 用户的所有变更必须经 `user.Service` 而不是直接调 `user.Store`,
  否则受影响节点不会被标脏,数据库改了而节点没改;
* **一台机器只能承载一个节点**:`deployment.Layout` 的路径(`/opt/litebox`)
  与服务名(`litebox-singbox`)是全局固定的,两个节点指向同一主机会互相覆盖配置;
* **节点的两个端口含义不可混用**:`nodes.proxy_port` 是客户端连接的公网端口,
  只写进订阅;`nodes.listen_port` 是 sing-box 的 `inbound.listen_port`,
  端口监听检查与 VLESS 拨测查的也是它。NAT 主机与 nginx 转发时两者不同,
  把公网端口渲染进节点配置会让 sing-box 监听在转发链路另一端的号码上 ——
  `check`、systemd、健康检查全部通过,只有用户连不上。
  `singbox.NodeParams` 里只有 `ListenPort`,公网端口不属于节点配置;
* 节点的握手目标不走通用的 `Store.Update`,必须经 `ApplyHandshakeDest`
  实测通过后写入,否则会绕过 8192 字节记录上限的校验;
* 订阅 Token 同时保存 SHA-256 哈希与主密钥加密的密文:
  哈希供公开订阅路由查找(该路径不解密任何字段),密文供面板反复展示订阅地址;
* 注释用中文,只写代码本身表达不了的约束与原因,不写"这行做什么"。

## 常用命令

```bash
make test          # 运行 Go 测试
make lint          # go vet + gofmt
make web           # 构建前端到 web/dist
make build         # 构建二进制(需先 make web)
make build-linux   # 交叉编译 linux amd64/arm64
make run           # 本地启动后端
make dev           # 启动前端开发服务器(API 代理到 127.0.0.1:8080)

go run ./cmd/litebox genkey    # 生成主密钥
go run ./cmd/litebox migrate   # 只执行迁移并做一致性检查

bash scripts/build-singbox.sh  # 构建带 with_v2ray_api 的节点二进制到 assets/singbox
```

针对真实节点的集成测试(会重启节点服务,不要指向生产节点):

```bash
export LITEBOX_TEST_NODE_HOST=1.2.3.4
export LITEBOX_TEST_NODE_PORT=22
export LITEBOX_TEST_NODE_KEY=~/.ssh/id_ed25519
go test ./internal/deployment/ -run TestIntegration -v -timeout 15m
```

## 已知依赖问题

`modernc.org/sqlite` 锁定在 v1.39.1。更高版本依赖的 `modernc.org/libc v1.74.1`
在模块代理上的 zip 损坏,升级前需先确认可正常下载。

## 阶段状态

* Phase 0 技术验证 —— 已完成,见 `docs/Phase0-技术验证报告.md`
* Phase 1 项目骨架 —— 已完成(配置、加密、迁移、登录会话、审计、REST API、Vue 骨架、部署文件)
* Phase 2 节点能力 —— 已完成(SSH 连接池、节点探测、握手目标检测、配置渲染、部署事务、三步健康检查、自动回滚)
* Phase 3 用户与配置生成 —— 已完成(用户 CRUD、节点分配、合并部署协调器、配置 diff)
* Phase 4 流量与额度 —— 已完成(Stats gRPC 客户端、SSH 通道采集、重启判定、ledger 入账、额度检查、定时调度)
* Phase 5 订阅 —— 已完成(VLESS URI、sing-box 客户端配置、节点过滤、公开路由、访问记录)
* Phase 6 管理页面 —— 已完成(仪表盘、用户管理、节点管理、部署记录、审计日志、流量图表)
* Phase 7 部署与加固 —— 已完成(安装升级备份恢复脚本、日志轮转、故障演练、128MB 压测)

未完成当前阶段前,不要提前开发后续阶段的功能。
