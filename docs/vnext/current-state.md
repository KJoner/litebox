# VNext 现状与差距清单(S0)

需求基线:`docs/vnext/requirements.md`(1.0,2026-11-06)。本文按需求编号逐条回答「已有、已有但不好用、确实缺失」。
核对的代码:`master` 7785523(V22.8,即需求 §02.1 审计的那个提交),工作区干净;实施分支 `feat/vnext`。

**不做的部分**:§17 Nexus Portal(用户在另一个仓库单独完成),以及需求 §01.2 明确不扩张的商业功能。

## 1. 已有、直接继承(只做回归,不重做)

| 需求 | 现有实现 | 说明 |
|---|---|---|
| §02.2-1 多来源流量 | `traffic/sync.go`(sing-box V2Ray Stats)、`mieruapi`(每实例 batch)、`hosttraffic`(vnStat)、`traffic.notChainCode` | 链式内部凭据已不进全站合计,各跳仍各记一份 |
| §02.2-2 页面优化 | 路由懒加载、仪表盘 `failedSources` 降级、`NetRateSampler` 租约 | V22.7 已把 `probe.Summaries` 改读小时汇总 |
| §02.2-7 / CONFIG01 | `Result.Unchanged`:渲染哈希 = 库里 = 节点文件 sha 且服务 active 时不重启 | 回归测试已有 |
| PROBE01 | 主控入口、PEER、出站 ICMP/TCP/SYN/HTTP、BOCE TCPing/Ping、卡片别名与 3+3 展示 | V22–V22.6 |
| PROBE03 | `netprobe/http.go`:任何响应都计时、5xx 单列、不跟随跳转、校验证书、只读响应头 | 404 记延迟已有测试 |
| PROBE04 BOCE | 幂等键 + 预算预留同事务、结果逐个落库、重启后 UNKNOWN 不重发 | V22.3 |
| CLIENT 基础 | URI / base64 / Clash / sing-box 四种格式、V5 模板、Token 重置、二维码 | `?format=sing-box` 已是 1.12+ DNS 新格式 |
| CHAIN 服务端两跳 | `node.chainservice` 两阶段有序部署、`AssertChainRouted`、`checkChainTargetsReady` | 已有,缺的是快速创建与预览 |
| SEC01 | 主密钥加密、Token 只存哈希、口令不落库 | 不变 |
| CONFIG02 | 用户列表与 stats 同源断言、真实拨测、拨测失败带回日志、重启前同步与锁顺序 | 不变 |
| PROTO01 | VLESS+REALITY、SS2022 + 传统 AEAD(V22.8)、Mieru、Snell(含共享 PSK) | 传统 AEAD 未真机验证 |

## 2. 已有但不好用(本次要改)

| 需求 | 现状 | 证据 | 计划 |
|---|---|---|---|
| PERF01/03 侧栏计数 | 侧栏为了五个数字拉 `users` / `nodes` / `external-proxies` / `deployments(50)` / `trafficStatus` 五个完整列表 | `web/src/layouts/MainLayout.vue:177` | 新增 `GET /api/sidebar-counts`,只做 COUNT |
| PERF03 会话写入 | 管理员与门户每个请求都 `UPDATE ... last_seen_at`,而 SQLite 只有一条连接,读请求也要排写队列 | `auth/auth.go:188`、`portal/auth.go:224` | 节流:距上次写入超过 60 秒才写;吊销 / 到期判断仍按库里的 `expires_at` 与 `revoked` |
| PERF03 列表配置状态 | `ConfigStatuses` 对每台机器查用户 + 完整渲染一遍配置算哈希,注释写明「不做缓存」 | `node/configstate.go:126` | 按依赖版本做缓存:键 = 节点 + 全部影响渲染的表的版本号,任一依赖变化立即失效;无法确认时不缓存 |
| CORE03 二进制分发 | `DirBinaryProvider` 把 sing-box(~36MB×2 架构)、mita、mieru 整份读进 Go 堆常驻;查哈希也触发整份读取 | `node/binaries.go:107` | 改流式:按 (路径, 大小, 修改时间) 缓存哈希,上传时 `os.Open` 流式经 SFTP 写 |
| PERF06 长任务 | 安装 / 下发 / 重启是同步 HTTP,靠 `longOperation` 把写期限放宽到 10 分钟;前端拿不到响应时只能提示「可能已跑完」 | `httpapi/longop.go`、`NodeOpProgressModal` | 后台任务表 + `task_id` + 轮询进度;旧的同步调用保留兼容 |
| UI05 状态 | 有「待部署」与配置状态,没有「已保存 / 待应用 / 应用中 / 已生效 / 失败待处理」五态统一说法 | `components/node/serviceState.ts` | 统一状态模型与组件 |
| UI06 拖拽 | V22.7 卡片拖动只存本机;入口 / 节点排序只能逐项改数字 | `EntrySortModal.vue` | 多选整体拖动 + 保存状态 + 版本冲突 |
| NODE01–05 创建流程 | 「新增节点」一个长表单:内部名、展示名、地址池、等级、资产、第一个入口混在一起;没有「外部节点」分流 | `NodeFormModal.vue` | 先选类型;基础连接置顶;默认入口第二组;其余折叠 |
| TRAFFIC01 口径 | 主机流量页已写「机器视角」;用户额度只有 U+D 一种算法;节点周期 EGRESS/BOTH 是「代理字节 × 系数」的折算,界面上没说是估算 | `traffic/nodecycle.go` | 标注三种口径;折算来源单列为「代理折算」 |
| CHAIN02/03 | 服务端链式要先建入口、再打开「出口」弹窗、选落地、再下发两台机器;没有路径预览 | `ChainModal` | 快速组链向导 + 路径预览 + 静态校验接口 |

## 3. 确实缺失(本次新增)

| 需求 | 说明 |
|---|---|
| DB01–DB11 | 只有 SQLite(`modernc.org/sqlite`,单连接);没有 PG 驱动、方言迁移、迁移工具 |
| DEPLOY01–10 | 没有 Dockerfile / Compose / 发布流水线;`litebox-install.sh` 在生产机上装 Go、Node 并现场构建 |
| CORE01 manifest | 节点二进制没有发布清单(版本、架构、能力、sha256) |
| AUTH01–04 直接授权 | 只有「入口等级继承 + 整机额外授权」,没有用户 × 入口的直接授权、「仅指定入口」模式、「仅指定用户」入口 |
| AUTH04 撤权状态 | 撤权只是「标脏 + 协调器部署」,界面上没有「撤销待下发 / 已撤销 / 失败」 |
| TRAFFIC02 计费公式 | 没有 上行 / 下行 / 合计 / 取大 + 倍率 + 规则版本 |
| TRAFFIC03 网卡选择 | vnStat 自动选网卡,不能人工指定、不排除虚拟网卡 |
| TRAFFIC08 用户周期 | 用户额度只有「每月重置日」一种 |
| AGENT01–06 | 没有 Agent(按需求只做可选,SSH 仍是默认) |
| PROTO02 | 自建入站没有 Trojan / Hysteria2 / TUIC / AnyTLS |
| PROTO03 能力注册 | 「可导入 / 可当出站 / 可自建 / 可按用户计量 / 某客户端可导出」散在 `DialableByNode`、`RelayableBy*`、`Entry.URI/Proxy/Outbound` 是否为 nil 等多处,没有一张可查询的表 |
| CLIENT P1 | 没有 Stash、Surge、Loon、Quantumult X 输出;没有「兼容预览」 |
| UI01 主题 | V21 的中性灰阶,默认浅色;不是 Nexus 系列暖炭 + 琥珀 |
| SEC04 审计筛选 | 审计已有分类,缺「按对象、只看失败」与任务 ID |
| 单活动主控锁 | 两个主控指向同一个 SQLite 文件时没有运行期互斥 |

## 4. 性能热点的静态清单(需要 S1 的基准数据确认)

需求 §11.1 点名的六项,逐条核对代码之后:

1. **单条 SQLite 连接**:`database.Open` 的 `SetMaxOpenConns(1)`。所有读写排同一个队;V22.7 已经因此把首屏请求重新编排过。
2. **列表配置状态**:见上表,每台机器一次完整渲染。50 台机器 × 每次渲染(查入站、查用户、查链路、序列化 JSON、算 sha256)。
3. **侧栏计数**:五个完整列表接口,其中 `users` 与 `nodes` 都带派生字段。
4. **认证写入**:每个请求一次 UPDATE。门户「探针」页每分钟自动刷新,管理员仪表盘默认 1 分钟自动刷新。
5. **live 接口占锁**:`/api/nodes/{id}/host-traffic/live` 走 `pool.Do`,与部署抢节点锁(V15 已限制只在流量 Tab 打开时读,2 分钟无操作停)。
6. **二进制常驻**:见上表。

**不能凭这六项断言「卡顿全是 SQLite/SSH 造成的」**,S1 用固定数据集的基准(`internal/perfbench`)逐项测量后再改。

## 5. 低资源参考栈(S0 冻结,验收在 S6)

| 档位 | 系统 | 组件 | 入口 / 用户 | 负载 |
|---|---|---|---|---|
| 64 MiB 最小栈 | Alpine 3.2x / OpenRC / dropbear 或 OpenSSH | sing-box(精简构建)1 个进程 | 1 个 VLESS+REALITY 或 SS 入口 + 1 条 realm TCP 转发;5 个用户 | 10 个并发 TCP 连接、持续 1 Mbps |
| 128 MiB 常用栈 | Alpine 或 Debian 最小 / OpenRC 或 systemd / OpenSSH | sing-box + vnstatd(+ 可选 mita 1 个实例) | 3 个入口(VLESS、SS、Snell)+ 1 条 nginx 或 realm 转发;10 个用户 | 30 个并发连接、持续 5 Mbps |

两档都分别记录无 swap 与有 swap;安装 / 更新时的临时文件与短暂双进程峰值计入。
可用的测试机(用户授权):`djj-ph`(NAT,1C / 512MB,公网 54300–54308 映射)与 `sh-ht`(NAT,1C / 128MB,公网 38081–38082 映射)。
**64 MiB 档没有真机**,只能用 cgroup 限制模拟(`systemd-run -p MemoryMax=64M` 或 OpenRC 的 cgroup),报告里必须写明。

## 6. 数据备份演练(S0)

- 现有 `internal/database/backup.go` 用 `VACUUM INTO` 做一致性快照,`scripts/` 下有备份恢复脚本;
- S0 的演练在隔离目录完成:用 master 二进制建库、灌夹具数据、备份、在另一目录恢复、新二进制打开并迁移;结果写进 `acceptance.md` A01。
