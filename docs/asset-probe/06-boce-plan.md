# Boce 外部入站拨测与卡片展示自定义 · 技术方案

需求见 `docs/开发计划/v21/探针系统增强.md`。本文是动手前的方案与影响范围,交付记录另见 `07-boce-delivery.md`。

## 一、Boce 接口事实(官方文档,2026-10-19 核对)

| 接口 | 要点 |
|---|---|
| `GET /v3/node/list?key=&area=` | `area` 不传为中国大陆、`oversea` 为海外;返回 `data.list[]{id, node_name, isp_name, isp_code}` |
| `GET /v3/task/create/tcping?key=&node_ids=31,32&host=域名或IP:端口` | 一次只测一个 host,多个拨测点逗号分隔;成功返回 `data.id`(任务 ID) |
| `GET /v3/task/tcping/{id}?key=` | 顶层 `{done, id, max_node, list[]}`(没有 `data` 包装);每项有 `node_id / node_name / ip / ip_region / ip_isp / packet_loss / packets_transmitted / packets_received / round_trip_avg / min / max / error_code / error` |
| `GET /v3/balance?key=` | `data.balance`(元)、`data.point`(剩余波点) |
| 公共错误码 | `-1` 节点异常、`1` 鉴权失败、`2` 参数错误、`3` 波点不足或未配置、`4` 生成任务 ID 失败或任务 ID 失效 |

文档里几条直接决定实现的话:

- 「当 `error_code` 不为 0,视为**拨测节点未响应**」—— 单个拨测点出错是拨测点的事,不是目标不通;
- 「`error` 含 `not found node` 表示节点下线,**不扣波点**」;
- 「建议每 10 秒获取一次,直到 `done=true` 或 2 分钟为止」;
- 「**任务结果只保留五分钟**」—— 结果必须在拿到的那一刻落库,面板停机超过 5 分钟,那一轮的结果就没了;
- `packet_loss` 的单位(百分数还是比例)文档没写,示例里是 `0`。**丢包率由 `packets_transmitted / packets_received` 自己算**,
  原始的 `packet_loss` 只存档不参与统计;
- TCPing 文档没有提 IPv6,侧栏里的「IPv6 检测」是网站检测、不能选拨测点。**首版不提交 IPv6 目标**,
  选到 IPv6 入口时明确说明,不改用 IPv4 测(那测的就不是这个入口了);
- 文档没有写计费单价。按需求给的规则估算(每个拨测点每次 1 波点、1 波点约 ¥0.002),两个数都做成设置项,
  并在界面上写明「估算,以 Boce 账单为准」。网页版的每日免费次数不计入。

## 二、复用与新增

**复用 `internal/probe` 的序列模型**:每个「Boce 任务 × 拨测点」就是 `probe_targets` 里的一条序列
(`direction = INGRESS`、`source = BOCE`、`kind = SERVICE`),结果写 `probe_results`,小时汇总写 `probe_hourly`。
于是点阵、1h / 24h / 7d 历史、换代(入口地址或端口变了 generation + 1)、告警状态机、7 天 / 90 天保留、
卡片汇总全部沿用,不另起一套历史。

| 新增 | 内容 |
|---|---|
| `internal/boce` | 纯供应商客户端:四个接口、字段与长度校验、错误分类、凭据脱敏。不依赖 probe |
| `internal/probe/boce_*.go` | 任务与拨测点缓存、调度时间表、预算预留与结算、引擎(创建 / 轮询 / 恢复)、费用估算 |
| 迁移 0043 | 见下 |
| `internal/settings/boce.go` | Key(主密钥加密)、全局开关、预算上限、单价、时区、默认计划 |
| `internal/httpapi/boceapi.go` | 设置、校验、余额、同步、任务、手动检测、执行记录、估算、卡片展示偏好 |
| 前端 | 系统设置「Boce 外部拨测」、节点网络监测的任务管理与执行记录、拨测点选择器、卡片展示设置、批量应用 |

### 迁移 0043(SQLite 改不了 CHECK,按 0041 的顺序重建)

- `probe_targets` 重建:放开「入口只能是主控」那条 CHECK,允许 `source = BOCE`(必须挂 `boce_task_id` 与 `vantage_id`);
  原唯一索引改成两条部分索引(非 BOCE 仍按 `(node_id, ref, family, source)`,BOCE 按 `(boce_task_id, vantage_id)`);
- `probe_results` 重建:状态加 `VANTAGE_ERROR`(拨测点异常)、`API_ERROR`(接口异常)、`EXPIRED`(任务未取得结果);
- `probe_hourly` 重建:加这三种的计数列(都不进分母);
- 顺序照 0041:先建新表、全量搬数据,**先删子表后删父表**,最后改名 —— 反过来的话外键的 CASCADE 会把全部历史删掉;
- 新表:`boce_vantages`(拨测点缓存)、`boce_tasks`(任务定义)、`boce_runs`(每一轮 = 一次 Boce 任务,含幂等键与预算)、
  `boce_results`(拨测点明细,主键 `(run_id, vantage_id)` 即「Boce 任务 ID + 拨测点 ID」去重)、`boce_state`(单行运行状态)、
  `probe_card_prefs`(每台节点入站 / 出站各最多 3 项的卡片展示偏好)、`probe_node_settings.boce_paused`;
- **迁移不建任何任务、不打开开关**:升级之后一次 Boce 请求都不会发。

## 三、关键规则

1. **未明确启用不花钱**:全局开关默认关;没有 Key、日 / 月预算没设(为 0)、自动暂停中,一律不创建付费任务,手动检测也一样;
   节点列表同步与余额查询是免费接口,配置阶段就能用。
2. **一轮 = 一次 Boce 任务**:同一个目标这一轮的全部拨测点放在一次 `create/tcping` 里,不按拨测点拆。
3. **幂等**:`boce_runs.idem_key` 唯一(定时 `S:<任务>:<时间槽>`、手动 `M:<任务>:<请求 ID>`)。预算预留与插入 run 在同一个事务里
   (数据库只有一条连接,事务天然串行 —— 并发不会超额)。发请求之前 run 已经是 `CREATING`:
   - 请求**确定没发出去**(用 `httptrace.WroteRequest` 判断:DNS 失败、拒绝连接、TLS 失败)→ 释放预留,最多重试 2 次;
   - 发出去了但没拿到可信响应(超时、断开)→ `UNKNOWN`,**不重试**,预留按已花算;
   - 面板在 `CREATING` 时重启 → 同样记 `UNKNOWN`。同一个时间槽永远只有一行,重启、并发、编辑都不会再建第二次。
4. **结果**:`POLLING` 每 10 秒问一次,`done=true` 收尾,超过 2 分钟记 `TIMEOUT`(已拿到的照存),
   任务 ID 失效(错误码 4)记 `EXPIRED`;拿到一个拨测点的结果就落库,按 `(run_id, vantage_id)` 去重。
   没出结果的拨测点在那条序列上记 `EXPIRED`;`not found node` 记 `VANTAGE_ERROR` 且不计波点。
5. **状态分开**:目标连不上(拨测点 0 收)= `TIMEOUT`(进比例、进告警);部分丢包 = `LOSS`(可达);
   拨测点自身异常 = `VANTAGE_ERROR`、接口异常 = `API_ERROR`、任务未取得结果 = `EXPIRED`、预算 / 关机未执行 = `SKIPPED`,
   这四种都**不进分母、不改连续失败计数**。第三方故障不会被记成 VPS 100% 丢包。
6. **口径分开**:Boce 序列的 metric 是 `BOCE`:「TCPing 丢包率(Boce)」= Σ(发 − 收) ÷ Σ发(按 Boce 报告的包数);
   与 ICMP 丢包率、主控的 TCP 连接失败率名字不同、不相加。
7. **时间表**:固定间隔(从运行窗口起点对齐,窗口支持跨午夜,`08:00–24:00` 每小时 = 08:00…23:00 共 16 轮,24:00 不跑)、
   固定时间点(窗口不参与)、星期掩码、每个任务自己的时区(默认 Asia/Shanghai)。错过的时间槽只补最近一个、10 分钟内才补。
   按任务 ID 在时间槽后 0~3 分钟里错峰(不越过窗口终点),两次创建请求之间至少间隔 3 秒。
8. **预算**:每轮预留 = 拨测点数 × 每点波点;依次检查单次上限、单任务月上限、日上限、月上限(日 / 月按设置的时区切)。
   到 80% 推一次、用完推一次;用完之后新的付费检测一律不建(记 `SKIPPED`,点阵上画成「未执行」),结果查询不受影响。
   鉴权失败、波点不足、连续 5 次接口异常 → 自动暂停,要管理员在设置页恢复。
9. **卡片只控制展示**:`probe_card_prefs` 每个方向 `AUTO` / `MANUAL`;手动时最多 3 项、有序、不重复、只能是这台节点的序列;
   自动推荐优先在测且有效的、尽量覆盖不同运营商 / 地区。没有偏好行 = 自动(升级后卡片不空)。
   改展示只写这一张表,与 Boce 引擎没有任何调用关系。
10. **安全**:Key 用主密钥加密存 `system_settings`,接口只回「已配置 + 末 4 位」;错误文本里擦掉 Key 与带 Key 的 URL;
    Boce 地址写死在代码里;被测地址只能从这台节点的订阅入口里选(与入口连通性同一份白名单),服务端解析;响应体限 2 MiB、
    逐字段校验长度与数值范围。

## 四、影响范围

- `internal/probe`:主控入口引擎只取 `source = MASTER`(漏了这一条,主控会自己去测 Boce 的序列);`List` / `ListAll` 的入口
  不含 BOCE(另有 `ListBoce`);`Update` / `Delete` 拒绝 BOCE 序列;`Status` 三种新状态;`Bucket` / `Stats` 三个新计数;
  `Summary` 的 BOCE 间隔、过期判据与暂停原因;推送文案分来源。
- `internal/httpapi`:节点网络监测响应加 `boce` 与 `card`;仪表盘聚合加 `boce` 与 `card_picks`;Boce 一组新路由与审计动作。
- `internal/notify`:两种事件 `BOCE_ALERT`(接口异常 / 自动暂停 / 恢复)、`BOCE_BUDGET`(预算 80% / 用完 / 余额低)。
- 前端:`client.ts` 类型;`probeView.ts` 的 BOCE 口径;点阵三种新状态;`assetModel.ts` 按 `card_picks` 取行;
  `NodeNetworkPanel.vue` 加来源筛选、Boce 任务区与卡片展示设置;`SettingsView.vue` 加一节;`NodesView.vue` 批量应用。
- 不改:计费 / 续费、订阅、部署、出站探测的门禁与预算(两套预算各管各的:出站按「每分钟尝试次数」,Boce 按「波点」)。

## 五、验证

单元 / 接口测试全部用假的 Boce 服务(`httptest`)与临时库;Playwright 用模拟层;**CI 与测试不调用真实 Boce 接口**。
真实接口只在管理员授权后用 1 个拨测点、1 个目标、最少次数验证,并记录实际波点消耗。
