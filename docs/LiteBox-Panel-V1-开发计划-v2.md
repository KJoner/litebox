# LiteBox Panel V1 详细开发计划(v2,经 Phase 0 验证修订)

修订日期:2026-08-02
依据:`docs/Phase0-技术验证报告.md`
状态:Phase 0 已通过,可进入 Phase 1

本文档取代 `LiteBox-Panel-V1-开发计划.md`。原文档的目标、约束、页面范围保持不变,
本版主要修订技术方案中被 Phase 0 证伪或需要补强的部分,并把每个阶段细化到可执行粒度。

---

## 1. 项目目标与约束

与原计划一致,不变:

* 面向个人使用,最多 10 个用户;
* 支持多台 Linux VPS,主控约 2C2G,节点可低至 128MB;
* 节点不依赖 Docker、数据库、Node.js 或常驻自研 Agent;
* V1 只支持 VLESS + REALITY + XTLS Vision;
* 允许用户变更时重启 sing-box;
* 支持流量统计、额度限制、到期停用;
* 不做注册、套餐、订单、支付、优惠券、邀请、工单等商业功能。

Phase 0 已确认 128MB 节点可行(实测 cgroup 实占 10–13MB,128M 硬限制下零 OOM)。

---

## 2. 技术栈

### 后端

* Go 单体应用,REST API,SQLite(WAL),Go embed 前端产物,systemd 运行;
* SSH / SFTP 使用 `golang.org/x/crypto/ssh`;
* gRPC 客户端读取 sing-box V2Ray Stats API,**连接通过 SSH 通道注入**,
  不使用本地端口转发(见 Phase 0 报告第 8 节)。

### 前端

* Vue 3 + TypeScript + Vite + Ant Design Vue;
* 构建产物嵌入 Go 二进制,生产环境不运行 Node.js。

### 节点

* sing-box 原生二进制(自定义精简构建)+ systemd;
* 只有一个 VLESS + REALITY 入站;
* V2Ray Stats API 仅监听 127.0.0.1;
* 无面板、无 Docker、无自研 Agent。

### 目录结构

```text
litebox/
├── cmd/litebox/
├── internal/
│   ├── auth/          管理员登录、Session、限流
│   ├── user/          代理用户 CRUD
│   ├── node/          节点管理、探测、握手目标检测
│   ├── traffic/       统计同步、ledger、每日聚合
│   ├── quota/         额度与到期检查
│   ├── subscription/  订阅生成
│   ├── deployment/    部署事务、回滚、批量合并
│   ├── singbox/       配置结构体、渲染、校验
│   ├── sshx/          SSH 连接池、SFTP、远程命令
│   ├── database/      连接、迁移
│   ├── scheduler/     定时任务
│   └── crypto/        主密钥、字段加密、Token 哈希
├── web/{src,dist}/
├── migrations/
├── scripts/
├── deploy/{systemd,nginx,docker-dev}/
├── docs/
├── CLAUDE.md
└── README.md
```

相比原计划,`ssh` 相关能力独立为 `internal/sshx`(连接池是 Phase 0 明确的需求)。

---

## 3. sing-box 自定义构建(已验证)

不修改上游源码。构建流程:

1. 固定版本 **v1.13.15**,禁止使用 `latest`;
2. 下载对应源码(`git clone --depth 1 --branch v1.13.15`);
3. 构建标签使用**精简集合**,不使用上游默认标签:

   ```
   with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0
   ```

   * `with_utls` —— REALITY 服务端实现已并入该标签,
     独立的 `with_reality_server` 标签在 1.13 中已被移除;
   * `with_v2ray_api` —— 用户级流量统计;
   * `badlinkname,tfogo_checklinkname0` —— 上游默认标签的一部分,
     与 `-checklinkname=0` 配套,缺失会导致构建失败。

4. LDFLAGS 使用上游 `release/LDFLAGS` 的内容,并追加版本串:

   ```
   -X 'github.com/sagernet/sing-box/constant.Version=v1.13.15-litebox'
   -X internal/godebug.defaultGODEBUG=multipathtcp=0
   -checklinkname=0 -s -w -buildid=
   ```

5. 目标平台 linux/amd64 与 linux/arm64,`CGO_ENABLED=0`,`-trimpath`;
6. 构建后执行 `sing-box version` 断言:版本串正确、Tags 中含 `with_v2ray_api`;
7. 生成 SHA-256,保存构建元数据(版本、标签、Go 版本、Revision、时间)。

实测体积:精简构建 27.8MB,完整默认标签 58.1MB。
精简构建 cgroup 实占内存降低约 63%,对低配节点收益明显。

V1 不需要:QUIC 入站、Hysteria2、TUIC、TUN、Clash Dashboard、大型规则集、
GeoIP/Geosite、节点端 Web UI。

---

## 4. 数据库设计

### 表清单

```text
admin_users        管理员
admin_sessions     会话
proxy_users        代理用户
nodes              节点
user_nodes         用户与节点的分配关系
node_instances     节点 sing-box 进程实例(新增)
node_counters      各节点各用户的统计基线(新增)
traffic_ledger     流量增量流水
traffic_daily      每日聚合
deployments        部署记录
audit_logs         审计
system_settings    系统设置
schema_migrations  迁移版本
```

相比原计划新增两张表,均来自 Phase 0 的流量同步验证:

**`node_instances`** —— 记录节点上 sing-box 进程的启动时刻与最后同步时刻,
用于判定进程是否换代。字段:`node_id`(主键)、`started_at`、`last_sync_at`、`updated_at`。

**`node_counters`** —— 记录各节点各用户各方向的计数器基线。
主键 `(node_id, user_code, direction)`,字段含 `last_value`、`updated_at`。

### 关键字段

`nodes` 表在原计划基础上增加:

```text
handshake_max_record_size   握手目标实测最大 TLS 记录长度
handshake_checked_at        握手目标最后检测时间
singbox_build_tags          节点上二进制的构建标签(探测所得)
```

### 关键原则

* SQLite 开启 WAL,`busy_timeout=5000`,`foreign_keys=ON`;
* 所有时间存 UTC;
* 流量单位统一字节;
* UUID 与节点 REALITY 私钥需可恢复,用主密钥加密存储;
* 订阅 Token 只存 SHA-256 哈希;
* 用户删除优先软删除;
* 流量使用追加式 ledger,不直接以节点当前计数器作为总量;
* `traffic_ledger` 建唯一索引 `(batch_id, node_id, user_code, direction)` 保证幂等。

---

## 5. sing-box 配置生成

每个节点一个 VLESS + REALITY 入站。

```text
节点配置
├── 节点级 REALITY 参数(私钥、short_id、握手目标)
├── inbound.users[]
│   ├── name  = 用户代码(user_000001)
│   ├── uuid
│   └── flow  = xtls-rprx-vision
└── experimental.v2ray_api.stats.users[]  ← 必须与上面的 name 集合完全相等
```

### 硬性规则(Phase 0 发现驱动)

1. **同源渲染。** `inbound.users[].name` 与
   `experimental.v2ray_api.stats.users` 必须由同一份用户列表渲染,
   不允许由两条代码路径分别生成。渲染完成后断言两个集合相等,不等则拒绝部署。

   理由:Phase 0 实测,用户在 inbound 中但不在 stats 白名单时,
   该用户可以正常上网却完全没有流量记录,且 `sing-box check` 通过、无任何告警。
   这是以流量计费为核心的面板最危险的失效模式。

2. **面板自行强校验,不依赖 `sing-box check`。** 配置生成前必须校验:

   * UUID 必须匹配标准 UUID 格式(sing-box 会把任意字符串哈希成 UUID,
     空串或占位符会变成一个能正常上网的意外凭据);
   * `flow` 只允许 `xtls-rprx-vision`(sing-box 只在连接时校验 flow,
     写错会导致"配置通过、服务启动、全部用户断线");
   * 用户代码必须匹配 `^user_\d{6}$`;
   * short_id 必须是 1–16 位偶数长度的十六进制;
   * REALITY 私钥必须是 32 字节的 base64url。

3. **用户代码稳定唯一。** 使用 `user_000001` 形式,与用户可修改的显示名解耦。
   用户代码一经分配不得变更(它是流量统计的唯一标识)。

4. 数据库是唯一期望状态;不在远程 JSON 上做字符串替换;每次生成完整配置;
   必须使用结构体序列化,不得用字符串模板拼接 JSON;
   字段顺序保持稳定便于 diff;每份配置保存 revision 与 SHA-256。

---

## 6. 节点部署事务

```text
获取节点互斥锁
→ 强制同步一次流量        ← 失败必须中止,不得继续
→ 生成配置 + 自校验
→ SFTP 上传临时文件
→ sing-box check
→ 备份当前配置
→ 原子替换(rename)
→ systemctl restart sing-box
→ 健康检查一:systemd is-active
→ 健康检查二:端口监听
→ 健康检查三:真实 VLESS 拨测   ← 不可省略
→ 记录部署成功
```

失败处理:

```text
恢复上一版本配置
→ 重启 sing-box
→ 再次执行三步健康检查
→ 仍失败则标记 DEPLOY_FAILED
→ 保存完整错误信息与各步骤耗时
```

### 要求

* 同一节点禁止并发部署(节点级互斥锁);
* 用户连续变更合并,延迟 3–5 秒批量部署;
* 配置校验失败不得重启;
* 至少保留最近 5 个配置版本;
* 不通过 shell 字符串拼接执行远程命令,所有远程参数严格校验;
* 部署结果可追踪,每一步记录耗时与输出。

### 关于第三步健康检查

Phase 0 实测:把 `flow` 写成非法值后,`sing-box check` 通过、systemd active、
端口正常监听,但所有用户连接全部失败。只有真实拨测能发现。

实现方式:主控在节点本机通过 SSH 启动一个临时 sing-box 客户端进程
(复用已部署的二进制),用一个专用的探测用户配置向本机 VLESS 端口发起一次请求,
成功后立即结束进程。也可由主控通过 SSH 隧道直连节点 VLESS 端口完成拨测。
两种方式在 Phase 2 各实现一版并择优。

---

## 7. 流量统计

### 数据通路

```
主控 --SSH--> 节点 --127.0.0.1:28080--> sing-box V2Ray Stats API
```

用 `ssh.Client.Dial` 作为 gRPC 的 `ContextDialer`,gRPC 流量跑在 SSH 通道内。
节点 API 只监听回环,不开放任何额外公网端口,主控也不需要开本地转发端口。

实测:SSH 建连约 1320ms,后续调用约 157ms。建连成本是单次调用的 8 倍,
因此**必须按节点维护长连接并复用**,配合健康检查与自动重连。

### gRPC 调用注意

sing-box 在 `v2rayapi/stats.go` 的 `init()` 中把服务名改注册为
`v2ray.core.app.stats.command.StatsService`,与其自带 stub 生成的
`/experimental.v2rayapi.StatsService/...` 路径不一致。直接用生成的 client 会报
`Unimplemented`。必须用 `conn.Invoke` 显式指定完整方法名:

```
/v2ray.core.app.stats.command.StatsService/QueryStats
/v2ray.core.app.stats.command.StatsService/GetStats
/v2ray.core.app.stats.command.StatsService/GetSysStats
```

### 同步算法

每个同步周期(默认 60 秒)对每个节点执行:

```
1. GetSysStats            → 取 Uptime,反推进程启动时刻 startedAt
2. QueryStats("user>>>")  → 取所有用户计数器(不使用 reset)
   任一步失败 → 立即返回,不进入数据库事务
3. 开启数据库事务:
   a. 判定进程是否换代(三个信号,任一命中):
      - startedAt 相比上次前移超过 3 秒
      - Uptime 小于两次同步的实际间隔(减去容差)
      - 某个计数器值小于其基线(兜底)
      命中则把该节点所有基线归零
   b. 对每个计数器:增量 = 当前值 - 基线
      增量 > 0 时写入 traffic_ledger(带 batch_id)并累加用户总量
      推进基线到当前值
   c. 更新 node_instances 的 startedAt 与 last_sync_at
4. 提交
```

### 硬性要求

* **同步失败绝不能把用户流量归零。** 读取失败必须在进入事务前返回。
  Phase 0 场景 G 已验证该性质;
* **计数器缺失 ≠ 计数器为 0。** 计数器按需创建,用户没产生过流量时不存在,
  同步逻辑必须跳过而不是当作归零;
* **重启判定必须用 uptime,不能只靠计数器回退。**
  Phase 0 实测,只靠回退判定会漏算整个重启前的计数值(实测漏算 1,007,534 字节);
* **面板自己触发的重启要在部署事务内显式重置基线**,不依赖推断;
* 同一批统计数据用 `batch_id` 做幂等标识;
* 计量误差约 +0.34%(协议开销),方向为宁多勿少,符合预期。

### 未同步窗口

sing-box 计数器是纯内存的,进程退出即丢失,无补救手段。
60 秒同步周期意味着意外重启(OOM、宿主机重启、崩溃)最坏丢 60 秒流量。
个人自用场景可接受,需在文档中明确告知。

### 额度检查

```
同步流量 → 更新用户总用量 → 检查额度与到期时间 → 修改用户状态
→ 重新生成受影响节点配置 → 重启受影响节点
```

### 必须编写的测试

* 计数器重置 / 节点重启 / 重启后流量超过重启前计数值(漏算回归);
* 网络超时、gRPC 不可达;
* 同一批次重复写入(幂等);
* 计数器缺失;
* stats.users 与 inbound.users 不一致的断言。

---

## 8. 节点管理

### 字段

原计划字段之外增加 `handshake_max_record_size`、`handshake_checked_at`、
`singbox_build_tags`。

### 操作

* 新增节点、测试 SSH、检测系统架构;
* 检测 sing-box 版本和构建标签(断言含 `with_v2ray_api`);
* 生成 REALITY 密钥;
* **验证握手目标**(见下);
* 部署配置、重启服务、查看最近部署结果、禁用节点。

### 验证握手目标(Phase 0 重点发现)

REALITY 在窃取目标站证书时要求目标返回的**每一个 TLS 记录都不超过 8192 字节**
(`metacubex/utls` 中 `realitySize = 8192`)。超限时握手静默失败,
服务端只报一句 `REALITY: processed invalid connection`,不给任何原因,极难排查。

因此"验证握手目标"必须是一次真实的 TLS 1.3 握手测量,判定条件:

1. 协商出 TLS 1.3;
2. 服务端密钥交换组为 X25519(REALITY 客户端会剔除 X25519MLKEM768);
3. 握手期间每个 TLS 记录 ≤ 8192 字节;
4. ALPN 支持 h2(建议项,不满足只告警)。

**检测必须通过 SSH 在节点本机执行。** CDN 按地域下发不同证书链,
同一目标在不同出口的记录长度不同(实测 `www.apple.com` 本地 3373 / 洛杉矶 4738)。

实测参考值(洛杉矶出口):

| 目标 | 最大记录 | 可用 |
|---|---|---|
| www.cloudflare.com | 2672 | 是 |
| addons.mozilla.org | 4133 | 是 |
| www.apple.com | 4738 | 是 |
| dl.google.com | 5021 | 是 |
| gateway.icloud.com | 5814 | 是 |
| www.microsoft.com | 8273 | **否** |
| www.bing.com | 8340 | **否** |

内置默认可用目标清单,保存节点前强制校验通过,并把实测值写入数据库。

Phase 0 已实现该检测工具(`phase0/statsprobe/destcheck.go`),可直接移植。

---

## 9. 用户管理与订阅

### 用户字段与状态

与原计划一致。状态:`ACTIVE / DISABLED / EXPIRED / QUOTA_EXCEEDED /
DEPLOY_PENDING / DEPLOY_FAILED`。

### 操作

新增、编辑、启用停用、分配节点、修改额度、重置已用流量、修改到期时间、
重新生成 UUID、重新生成订阅 Token、删除用户。

用户代码 `user_000001` 一经分配不可变更。

### 订阅

```
https://panel.example.com/sub/{random-token}
```

输出 VLESS URI 与 sing-box 客户端 JSON,只包含该用户已分配且可用的节点。
Token 只存 SHA-256 哈希。需要缓存控制与访问日志。

---

## 10. 开发阶段

### Phase 0:技术验证 —— 已完成

十项验证全部通过,详见 `docs/Phase0-技术验证报告.md`。
产物在 `phase0/` 目录,其中 `statsprobe/` 下的 Go 原型可直接移植进正式项目。

### Phase 1:项目骨架

* Go 项目结构、配置加载(文件 + 环境变量)、主密钥管理;
* SQLite 连接(WAL)与迁移框架,建立全部表;
* 管理员登录:密码哈希(argon2id)、Session Cookie、登录失败限流、修改密码;
* 审计日志基础设施;
* Vue 项目骨架 + 登录页 + 空白布局,Vite 构建产物经 Go embed 嵌入;
* systemd 单元文件与 Nginx 反代示例;
* 单元测试与 CI 脚本。

验收:能启动、能登录、能看到空白仪表盘、数据库迁移可重复执行。

### Phase 2:节点能力

* `internal/sshx`:SSH 连接池(按节点复用长连接)、健康检查、自动重连、SFTP;
* 远程命令执行(参数化,禁止字符串拼接);
* 节点探测:架构、内核、sing-box 版本与构建标签断言;
* **握手目标检测**(移植 `destcheck.go`,通过 SSH 在节点本机执行);
* REALITY 密钥生成;
* 配置上传、`sing-box check`、备份、原子替换、重启;
* 三步健康检查(systemd / 端口 / **真实 VLESS 拨测**);
* 自动回滚与部署记录;
* 节点二进制分发与升级。

验收:能把一个空配置部署到真实节点、能检测握手目标、
故意下发非法 flow 配置能被拨测发现并自动回滚。

### Phase 3:用户与配置生成

* 用户 CRUD、用户代码分配、UUID 生成与格式强校验;
* 节点分配;
* sing-box 配置结构体与渲染(结构体序列化,禁止模板拼接);
* **同源渲染断言**:`inbound.users[].name` 集合 == `stats.users` 集合;
* 配置 revision、SHA-256、diff 展示;
* 用户变更的批量合并部署(延迟 3–5 秒);
* 节点级互斥锁。

验收:创建用户后自动部署到指定节点并可连接;删除用户后旧 UUID 立即失效;
连续修改多个用户只触发一次部署。

### Phase 4:流量与额度

* V2Ray Stats gRPC 客户端(注意方法名不一致问题);
* 经 SSH 通道的 gRPC ContextDialer;
* 定时同步(默认 60 秒);
* 重启判定(uptime 双信号 + 计数器回退兜底);
* `traffic_ledger` 写入与幂等;
* 每日聚合 `traffic_daily`;
* 额度检查、到期检查、超额自动停用并触发重新部署;
* 完整的失败场景测试(见第 7 节测试清单)。

验收:能分别统计每个用户在每个节点的上下行;汇总为用户总流量;
节点重启后主控累计流量不丢失不回退不漏算;同步失败不污染已有数据。

### Phase 5:订阅

* 随机 Token 生成与哈希存储、Token 重置;
* VLESS URI 与 sing-box 客户端 JSON 输出;
* 节点状态过滤;
* 缓存控制、访问日志。

验收:一个订阅地址返回该用户全部可用节点,客户端导入后可直接连接。

### Phase 6:管理页面

* 仪表盘、用户管理、节点管理、流量展示、部署记录、审计日志;
* 页面范围与原计划一致。

### Phase 7:部署与加固

* 安装脚本、升级脚本、备份恢复;
* SQLite 一致性检查、日志轮转;
* SSH 权限限制(建议为面板配置专用受限密钥);
* systemd 安全参数、配置文件权限;
* 故障演练:节点断网、节点 OOM、宿主机重启、主控重启、SSH 中断;
* 低配节点压测(128MB)。

---

## 11. 本地开发方式

主要开发环境:WSL2。

```text
Go 后端:原生运行
Vue 前端:Vite 开发服务器
SQLite:本地文件
```

Docker Compose 仅用于:启动多个 sing-box 测试节点、测试 VLESS+REALITY、
测试 V2Ray Stats API、模拟多节点、自动执行集成测试。

**systemd 部署、重启、回滚相关测试必须在 Linux VM 或真实 VPS 上完成**,
Docker Compose 不适用。Phase 0 全部在真实 Debian 12 VPS 上完成。

---

## 12. CLAUDE.md 约束

在原计划基础上,增加 Phase 0 得出的强制条款。

原有条款:

* 不引入 MySQL、PostgreSQL、Redis 或消息队列;
* 不引入微服务;
* 不在节点运行常驻自研 Agent;
* 不假设 VLESS 用户可动态热更新,用户变化必须通过配置生成和安全重启生效;
* 所有重启前必须先同步流量;
* 所有配置替换前必须执行 `sing-box check`;
* 所有部署失败必须自动回滚;
* 不通过字符串拼接生成 JSON;
* 不通过字符串拼接执行远程 shell;
* 所有数据库变更必须使用迁移;
* 每个阶段完成后必须运行测试并更新文档;
* 不增加商业机场功能;
* 不以未来扩展为理由提前引入复杂框架。

新增条款:

* **不得把 `sing-box check` 当作配置正确性的唯一保证。**
  UUID 格式与 flow 取值必须由面板自行强校验;
* **部署健康检查必须包含真实 VLESS 拨测**,
  不得仅凭 systemd 状态与端口监听判定成功;
* **`inbound.users[].name` 与 `experimental.v2ray_api.stats.users`
  必须由同一份用户列表渲染,并在渲染后断言两个集合相等**;
* **节点重启判定必须使用 `GetSysStats.Uptime`**,
  不得只依赖"计数器变小";面板自身触发的重启必须在部署事务内显式重置基线;
* **流量同步读取失败必须在进入数据库事务前返回**,
  任何情况下不得把用户流量归零;
* **REALITY 握手目标必须经过实测校验**(TLS 1.3 + X25519 + 记录 ≤ 8192),
  且检测必须在节点本机执行;
* **节点构建标签固定为精简集合**,不使用上游默认标签;
* **SSH 连接必须按节点复用长连接**,不得每次同步重新建连。

---

## 13. V1 验收标准

原有标准全部保留:

* 一个主控管理至少 3 个节点;
* 创建用户后能自动同步到指定节点;
* 删除或禁用用户后连接失效;
* 用户可通过一个订阅地址获得所有分配节点;
* 可分别统计每个用户在每个节点的上下行流量;
* 所有节点流量可汇总为用户总流量;
* 超额或到期用户会被自动停用;
* 节点配置错误可以自动回滚;
* 主控重启不会丢失业务数据;
* 节点重启不会清空主控累计流量;
* 节点无需 Docker 或常驻管理 Agent;
* 1C1G 节点可稳定运行;
* 128MB 节点完成实验性测试并明确记录是否可用 —— **Phase 0 已确认可用**。

新增标准:

* 节点重启后主控累计流量**既不回退也不漏算**(含"重启后流量超过重启前计数值"场景);
* 下发一份 `flow` 非法的配置,部署流程能通过拨测发现并自动回滚;
* 配置生成时若 `stats.users` 与 `inbound.users` 不一致,必须拒绝部署;
* 添加节点时若握手目标不满足 REALITY 要求,必须拒绝保存并给出实测记录长度;
* 流量同步在节点不可达时报错退出,数据库累计值不发生任何变化。
