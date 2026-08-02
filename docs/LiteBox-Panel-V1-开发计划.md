# LiteBox Panel V1 开发计划

## 1. 项目目标

开发一个面向个人使用的轻量级 sing-box 管理面板。

约束：

* 最多管理 10 个用户；
* 支持多台 Linux VPS；
* 主控服务器配置约为 2C2G；
* 节点服务器可能低至 128MB 内存；
* 节点不得依赖 Docker、数据库、Node.js 或常驻管理 Agent；
* 第一版只支持 VLESS + REALITY + XTLS Vision；
* 允许用户变更时重启 sing-box；
* 支持用户流量统计、额度限制和到期停用；
* 不开发注册、套餐、订单、支付、优惠券、邀请、工单等商业功能。

## 2. 技术栈

### 后端

* Go 单体应用；
* REST API；
* SQLite；
* 数据库迁移脚本；
* Go embed 内嵗前端静态文件；
* SSH、SFTP 和 SSH Tunnel；
* gRPC 客户端读取 sing-box V2Ray Stats API；
* systemd 运行。

建议目录：

```text
litebox/
├── cmd/
│   └── litebox/
├── internal/
│   ├── auth/
│   ├── user/
│   ├── node/
│   ├── traffic/
│   ├── quota/
│   ├── subscription/
│   ├── deployment/
│   ├── singbox/
│   ├── database/
│   ├── scheduler/
│   └── crypto/
├── web/
│   ├── src/
│   └── dist/
├── migrations/
├── scripts/
├── deploy/
│   ├── systemd/
│   ├── nginx/
│   └── docker-dev/
├── docs/
├── CLAUDE.md
└── README.md
```

### 前端

* Vue 3；
* TypeScript；
* Vite；
* Ant Design Vue；
* 构建后嵌入 Go 二进制；
* 生产环境不运行 Node.js。

### 数据节点

* sing-box 原生二进制；
* systemd；
* VLESS + REALITY；
* V2Ray Stats API；
* 不安装管理面板；
* 不安装 Docker；
* 不运行自研 Agent。

## 3. V1 功能范围

### 管理员

* 单管理员账号；
* 密码登录；
* Session Cookie；
* 修改密码；
* 登录失败限制；
* 操作审计。

### 用户管理

字段：

```text
ID
用户名称
备注
UUID
状态
流量额度
累计使用流量
到期时间
重置周期
订阅 Token
创建时间
更新时间
```

状态：

```text
ACTIVE
DISABLED
EXPIRED
QUOTA_EXCEEDED
DEPLOY_PENDING
DEPLOY_FAILED
```

操作：

* 新增用户；
* 编辑用户；
* 启用或停用；
* 分配节点；
* 修改流量额度；
* 重置已用流量；
* 修改到期时间；
* 重新生成 UUID；
* 重新生成订阅 Token；
* 删除用户。

### 节点管理

字段：

```text
ID
节点名称
主机地址
SSH端口
SSH用户名
代理监听端口
V2Ray API本地端口
架构
sing-box版本
REALITY握手目标
REALITY私钥
REALITY公钥
short ID
状态
最后心跳时间
最后部署版本
```

操作：

* 新增节点；
* 测试 SSH；
* 检测系统架构；
* 检测 sing-box 版本和构建标签；
* 生成 REALITY 密钥；
* 验证握手目标；
* 部署配置；
* 重启节点服务；
* 查看最近部署结果；
* 禁用节点。

### 订阅

每个用户生成随机订阅地址：

```text
https://panel.example.com/sub/{random-token}
```

第一版输出：

* VLESS URI；
* sing-box 客户端 JSON。

订阅内容只包含该用户已分配且处于可用状态的节点。

### 流量和额度

* 按用户、节点统计上行和下行流量；
* 用户总流量为所有节点流量之和；
* 支持总额度；
* 支持固定日期重置或手动重置；
* 到期自动禁用；
* 超额自动禁用；
* 记录每日流量；
* 流量同步失败不得覆盖已有统计值。

## 4. 数据库表

至少包含：

```text
admin_users
admin_sessions
proxy_users
nodes
user_nodes
traffic_ledger
traffic_daily
deployments
audit_logs
system_settings
schema_migrations
```

关键原则：

* SQLite 开启 WAL；
* 所有时间存 UTC；
* 流量单位统一使用字节；
* UUID 及节点私钥需要可恢复，因此使用主密钥加密存储；
* 订阅 Token 只保存 SHA-256 哈希；
* 删除用户优先使用软删除；
* 流量使用追加式 ledger，不直接依赖节点当前计数器作为总量。

## 5. sing-box 配置策略

每个节点只有一个 VLESS + REALITY 入站。

```text
节点配置
├── 节点级 REALITY 参数
└── users[]
    ├── name
    ├── uuid
    └── flow=xtls-rprx-vision
```

`users.name` 必须使用稳定且唯一的内部用户代码，例如：

```text
user_000001
```

不得直接使用用户可修改的显示名称作为流量统计标识。

生成配置时：

* 数据库是唯一期望状态；
* 不在远程 JSON 上做字符串替换；
* 每次生成完整配置；
* JSON 生成必须使用结构体序列化；
* 不使用字符串模板拼接 JSON；
* 配置应保持字段顺序相对稳定，便于审计和 diff；
* 每份配置保存 revision 和 SHA-256。

## 6. 节点部署事务

必须实现以下流程：

```text
获取节点互斥锁
→ 强制同步一次流量
→ 生成配置
→ 上传临时文件
→ sing-box check
→ 备份当前配置
→ 原子替换
→ systemctl restart sing-box
→ 检查systemd状态
→ 检查端口
→ 记录部署成功
```

失败处理：

```text
恢复上一版本
→ 重启sing-box
→ 再次检查
→ 将节点标记为DEPLOY_FAILED
→ 保存完整错误信息
```

要求：

* 同一节点禁止并发部署；
* 用户连续变更可合并，延迟3～5秒批量部署；
* 配置验证失败不得重启；
* 部署结果必须可追踪；
* 至少保留最近5个配置版本；
* 不通过 shell 字符串拼接用户输入；
* 所有远程参数必须严格校验。

## 7. 流量统计策略

使用 sing-box V2Ray Stats API。

要求：

* API 仅监听节点的 127.0.0.1；
* 主控通过 SSH Tunnel 访问；
* 统计上行和下行；
* 定时同步周期默认60秒；
* 重启前强制同步；
* 同步结果先写 traffic_ledger，再更新用户聚合值；
* 任何同步失败都不能把用户流量归零；
* 节点重启后重新建立统计基线；
* 同一批统计数据必须具备幂等标识；
* 对统计计数器重置、节点重启和网络超时编写测试。

额度检查流程：

```text
同步流量
→ 更新用户总用量
→ 检查额度与到期时间
→ 修改用户状态
→ 重新生成受影响节点配置
→ 重启受影响节点
```

## 8. sing-box 自定义构建

不要修改 sing-box 源码。

构建流程：

1. 固定一个经过测试的 sing-box 正式版本；
2. 下载对应源码；
3. 读取上游 `release/DEFAULT_BUILD_TAGS`；
4. 在默认标签基础上追加 `with_v2ray_api`；
5. 使用上游 `release/LDFLAGS`；
6. 构建 linux/amd64 和 linux/arm64；
7. 执行版本和构建标签检查；
8. 生成 SHA-256 校验值；
9. 保存构建元数据；
10. 禁止使用 `latest` 作为生产版本。

第一版不需要：

* QUIC 入站；
* Hysteria2；
* TUIC；
* TUN；
* Clash Dashboard；
* 大型规则集；
* GeoIP/Geosite；
* 节点端 Web UI。

## 9. 页面范围

### 登录页

* 管理员用户名和密码。

### 仪表盘

* 用户总数；
* 在线节点数；
* 今日流量；
* 本月流量；
* 超额用户；
* 即将到期用户；
* 最近部署失败。

### 用户列表

* 用户名；
* 状态；
* 节点数量；
* 已用流量；
* 总额度；
* 到期时间；
* 操作。

### 用户详情

* 基本信息；
* 节点分配；
* 流量趋势；
* 订阅地址；
* VLESS URI；
* 最近操作记录。

### 节点列表

* 节点名称；
* IP；
* 资源规格；
* sing-box版本；
* 状态；
* 今日流量；
* 最后同步时间；
* 最后部署状态。

### 部署记录

* 节点；
* revision；
* 配置哈希；
* 开始时间；
* 完成时间；
* 状态；
* 错误信息；
* 回滚结果。

## 10. 开发阶段

### Phase 0：技术验证

先不开发完整页面。

必须验证：

1. 构建带 `with_v2ray_api` 的 sing-box；
2. 单节点启动 VLESS + REALITY；
3. 两个 VLESS 用户均能连接；
4. 可以按用户名读取上行和下行流量；
5. 修改用户列表后执行 `sing-box check`；
6. 重启后新增用户生效；
7. 删除用户后旧 UUID 失效；
8. 重启前流量可以成功落库；
9. 128MB VPS 的实际内存占用；
10. 无效配置能够自动回滚。

Phase 0 未全部通过前，不进入完整 UI 开发。

### Phase 1：项目骨架

* Go 项目；
* SQLite迁移；
* 配置加载；
* 管理员登录；
* Vue基础页面；
* Go embed；
* 单元测试；
* systemd文件；
* Nginx示例。

### Phase 2：节点能力

* SSH连接；
* SFTP上传；
* 远程命令执行；
* 节点探测；
* 配置验证；
* 服务重启；
* 健康检查；
* 自动回滚；
* 部署记录。

### Phase 3：用户和配置生成

* 用户CRUD；
* UUID生成；
* 节点分配；
* sing-box配置结构体；
* 配置revision；
* 配置diff；
* 用户变更批量部署。

### Phase 4：流量和额度

* V2Ray Stats gRPC客户端；
* SSH Tunnel；
* 定时同步；
* traffic ledger；
* 每日聚合；
* 额度检查；
* 到期检查；
* 超额用户自动停用。

### Phase 5：订阅

* 随机Token；
* VLESS URI；
* sing-box JSON；
* Token重置；
* 节点状态过滤；
* 缓存控制；
* 访问日志。

### Phase 6：管理页面

* 仪表盘；
* 用户管理；
* 节点管理；
* 流量展示；
* 部署记录；
* 审计日志。

### Phase 7：部署和加固

* 安装脚本；
* 升级脚本；
* 备份恢复；
* SQLite一致性检查；
* 日志轮转；
* SSH权限限制；
* systemd安全参数；
* 配置文件权限；
* 故障演练；
* 实际低配节点压测。

## 11. 本地开发方式

建议使用 WSL2 作为主要开发环境。

日常启动：

```text
Go后端：原生运行
Vue前端：Vite开发服务器
SQLite：本地文件
```

Docker Compose 仅用于：

* 启动两个 sing-box 测试节点；
* 测试 VLESS + REALITY；
* 测试 V2Ray Stats API；
* 模拟多节点；
* 自动执行集成测试。

Docker Compose 不用于验证 systemd 部署和回滚。systemd 相关测试必须在 Linux VM 或测试 VPS 上完成。

## 12. CLAUDE.md 约束

Claude Code 必须遵守：

* 不引入 MySQL、PostgreSQL、Redis或消息队列；
* 不引入微服务；
* 不在节点运行常驻自研Agent；
* 不假设 VLESS 用户可动态热更新；
* 用户变化必须通过配置生成和安全重启生效；
* 所有重启前必须先同步流量；
* 所有配置替换前必须执行 `sing-box check`；
* 所有部署失败必须自动回滚；
* 不通过字符串拼接生成 JSON；
* 不通过字符串拼接执行远程 shell；
* 所有数据库变更必须使用迁移；
* 每个阶段完成后必须运行测试并更新文档；
* 未完成 Phase 0 前不得开发复杂 UI；
* 不增加商业机场功能；
* 不以未来扩展为理由提前引入复杂框架。

## 13. V1 验收标准

V1完成时必须满足：

* 一个主控管理至少3个节点；
* 创建用户后能自动同步到指定节点；
* 删除或禁用用户后连接失效；
* 用户可通过一个订阅地址获得所有分配节点；
* 可以分别统计每个用户在每个节点的上下行流量；
* 所有节点流量可以汇总为用户总流量；
* 超额或到期用户会被自动停用；
* 节点配置错误可以自动回滚；
* 主控重启不会丢失业务数据；
* 节点重启不会清空主控累计流量；
* 节点无需Docker或常驻管理Agent；
* 1C1G节点可稳定运行；
* 128MB节点完成实验性测试并明确记录是否可用。
