# UI 2.0 / 2.0.1 · 发布前最终检查

> 检查日期 2026-10-17。分支 `feat/ui-2.0`,以本地代码与 Git 状态为准。
> 敏感信息按**方案 A(向前修复,不改写公开 master 历史)**处理。
> 结论:**A. 可以合并** —— 敏感数据清理、迁移哈希兼容、CI 隐患都已解决;`feat/ui-2.0` 已推送,
> GitHub Actions 全绿,Draft PR [#1](https://github.com/KJoner/litebox/pull/1) 已创建,可以转为 Ready for Review。
> **合并 master 即发布**。经仓库所有者授权,PR #1 已于 2026-10-17 快进合并进 master(`b1caf11`,
> 原提交号不变),master 上的 CI 通过;没有打 tag、没有部署生产。

## 1. 分支与提交

| 项 | 值 |
|---|---|
| 远程 master | `7790978`,已包含 UI 2.0 五批(上一轮按要求快进合并并推送) |
| 已推送 | 只推送了 `feat/ui-2.0`(`origin/master..feat/ui-2.0` 共 20 个提交);`master` 仍是 `7790978`,没有强推,没有改写任何远程分支;`HEAD..origin/master` 为空,合并不会冲突 |
| PR | Draft PR [#1](https://github.com/KJoner/litebox/pull/1),`feat/ui-2.0` → `master`,GitHub 判定可合并 |
| 基底 | 第一个提交 `2f23204` 是独立的安全清理提交(分支 `security/scrub-public-artifacts`,直接基于 `origin/master`),UI 2.0.1 的提交整体变基在它之上 |
| `git diff --check` | 干净 |
| 后端改动范围 | `internal/traffic`(同步状态)、`internal/httpapi/traffic.go`(接口加字段)、`internal/database/migrate.go`(迁移哈希按换行归一,见第 4 节);**没有新迁移、没有安装 / 升级脚本改动** |

```
2f23204 security: 从当前版本删除含真实管理地址的截图与地址,加数据卫生约束
b269def … e685d99  UI 2.0.1 的 10 个修复 / 功能提交
cccdfd7 test(web): 引入 Playwright 前端回归测试与 CI
9323b83 / 426bad7  docs(ui): 2.0.1 验收记录
eafe29a fix(node): 「下发配置」不再强制重启之后,重新安装的提示改指「强制重新下发 / 重启」
9fbc6f6 style: 修正确认框正文那条注释被换行打断、留下行尾空格
f757de9 docs(ui): 发布前最终检查(本文件的第一版)
9cdaec6 fix(database): 迁移哈希不再因 CRLF / LF 检出不同而互相拒库
8515550 fix(ci): Go job 先放前端占位产物,否则 go vet 在干净检出上第一步就失败
(最后一个)  docs(ui): 本文件按方案 A 更新
```

本地历史整理过一次(见 2.3),提交号与上一版本文件里写的不同:原 `6253b43` / `d6aa706` 现为 `eafe29a` / `9fbc6f6`。

## 2. 敏感信息:暴露面与处理(方案 A)

### 2.1 实际暴露面

| 位置 | 内容 | 状态 |
|---|---|---|
| 公开 master 的当前版本与历史 | `docs/ui-modernization/screenshots/before|after` 64 张截图,其中 27 张含测试机公网 IPv4 与 `root@地址:端口` 管理通道 | **已公开**。UI 2.0 合并时带出去的;其中一台测试机的地址此前从未出现在仓库里 |
| 同上 | `CLAUDE.md` 与 3 个部署测试文件里的真实公网地址(UI 2.0 之前就在,含一条日志样本里的真实客户端地址);节点表单占位符里的一个真实地址 | **已公开**(早于本轮) |
| 同上 | `phase0/` 验证探针里写死的一组测试密钥材料(2026-08-02 起) | **已公开**。面板为每个入站自行生成密钥,线上节点不会用到它;若曾手工复用,需轮换 |
| 仅本地(从未推送) | 2.0.1 的两张截图(一张在遮罩后面露出半截地址);固定数据里两台测试机的真实 SSH 端口与部署详情里的主机密钥指纹;脱敏脚本里的真实端口对照表 | **已从待推送历史中清除**(2.3) |

面板凭据 —— 主密钥、管理员口令、订阅 token、节点私钥、AccessKey、推送凭据 —— **都没有出现在任何提交里**,
不需要轮换。泄露的是**测试机的管理端点**。加固与核实步骤已单独交给仓库所有者(不写进公开文档);
**仓库所有者决定本轮不对这两台测试机做 SSH 加固,也不把它作为推送与合并的前置条件**。

### 2.2 公开 master:独立的安全清理提交(待决定,未合并)

`security/scrub-public-artifacts`(`2f23204`,基于 `origin/master`)只做降低当前暴露面的事:

- 删除 `docs/ui-modernization/screenshots/before|after` 全部 64 张截图;文档里对它们的引用改成说明;
- `CLAUDE.md`、3 个测试文件与节点表单占位符里的真实地址换成 RFC 5737 文档地址(同一地址映射到同一个,
  测试语义不变);
- `CLAUDE.md` 新增「公开仓库的数据卫生约束」。

**它不改写历史**:旧截图仍可按提交号从 master 历史里取回,这是方案 A 的代价。
它可以单独开 PR 先合并(尽快缩小当前版本的暴露面),也可以随本分支一起合并 —— 本分支以它为基底,
两种顺序都不会冲突。**是否、何时合并由仓库所有者决定**,本轮没有推送、没有合并。

### 2.3 本地待推送历史的整理

- 整理前做了完整备份(本地备份分支与 bundle 文件,**都不推送**);
- 用 `fixup` + `rebase --autosquash` 把脱敏改动并进最初引入那些内容的提交,并把整段变基到 2.2 的清理提交上。
  **没有改写 `origin/master` 或任何远程分支**,没有强制推送;
- 脱敏脚本的真实端口 / 主机别名对照表移出仓库,改由本机的 `web/e2e/tools/sanitize.local.json`(已 gitignore)提供;
  用同一份原始录制重新生成,固定数据逐字节不变。

### 2.4 首次推送会上传的对象(逐个核查)

范围:远程任何分支都不可达、而 `feat/ui-2.0` 与 `security/scrub-public-artifacts` 可达的全部对象 ——
20 个提交、103 个目录树、114 个文件内容,其中图片 26 张。

| 检查 | 结果 |
|---|---|
| 图片 | 26 张全部是 `web/e2e/__screenshots__/`,由模拟层用虚构数据渲染;其余截图一张都不在待推送对象里 |
| 自定义规则(每个文件内容与提交说明:公网 IPv4、测试机端口、主机指纹、私钥、SSH 公钥、Cookie、口令) | 无真实地址、端口、指纹、密钥。剩余命中逐条核实为误报:浏览器 UA 里的 `Chrome/154.0.0.0`;默认 API 端口(早已在 master 上) |
| gitleaks v8.30.0(`origin/master..feat/ui-2.0`) | 1 处 `generic-api-key`:固定数据里外部代理的 `identity_key`,是脱敏脚本生成的假值;原值不在任何待推送对象里 |

**OCR 自动打码试过、不采用**:迭代识别加人工复核后仍有漏网。截图一律删除或改用虚构数据渲染。

### 2.5 规范

`CLAUDE.md`「公开仓库的数据卫生约束」与 `web/e2e/README.md`「数据卫生」:禁止把真实服务器管理信息、
订阅 Token、密钥与面板会话信息写进截图、测试数据、日志样本、文档与提交说明;截图只由虚构数据渲染;
示例地址用文档地址段;推送前对待推送范围跑 Secret Scanner 并按基础设施信息再扫一遍。

## 3. 核心业务变更审查

| 项 | 结论 |
|---|---|
| 配置一致时跳过重部署 | 判据(`node.Preflight` 第 7 步):渲染哈希 = 库里上次下发的哈希 = 节点上配置文件的哈希,**且**服务在跑;任一不满足都照常下发。本轮只是让卡片按钮不再误传 `force`。连带问题已修:重新安装 sing-box 后配置没变时普通下发会被跳过,提示改指「强制重新下发 / 重启」(`eafe29a`)。**测试缺口**:跳过判据没有单元测试,用授权测试节点补了正反两条证据(第 6 节) |
| 部署记录读失败 | 失败时保留上一次结果并注明;没有旧数据时显示错误态与重试 |
| 同步失败记录清理 | 只在拿到本轮同步名单之后清掉**不在名单里**的机器;名单查询失败时整轮返回、不清任何记录;有单测 |
| 仪表盘并行加载与重试 | 各来源独立加载,失败保留旧数据;「重试失败项」只重跑失败的加载并去重。**低风险竞态**:「刷新」与「重试」同时进行时同一接口可能并发两次,后返回的覆盖先返回的,两者都是新数据 |
| 门户流量 | 只改标签与说明,`internal/portal` 未改;三种订阅输出升级前后逐字节一致 |
| 路由 / 权限 / 按钮 | 路由与守卫未改;49 处链接式操作换成 `RouterLink` / `button`,按钮绑定逐个核对 |
| 数据库与 API 兼容 | 无新迁移;迁移**校验**放宽到只容忍换行差异(第 4 节);`/api/traffic/status` 只加字段 |
| 前端缓存 | `index.html` 为 `no-cache`,带哈希的资源长期缓存;已打开的旧页面导航到新的懒加载页面时可能要刷新一次 |

## 4. 迁移哈希的跨平台兼容(本轮修复)

**原因**:`internal/database/migrate.go` 对嵌入的迁移文件**原始字节**算 sha256,与 `schema_migrations.hash`
比对。仓库里存的是 LF;Windows 上 `core.autocrlf=true` 检出的是 CRLF。于是在 Windows 工作区编出来的
二进制记下 CRLF 的哈希,Linux 编的二进制随后报「迁移 … 的内容在应用后被修改」拒绝启动(反过来也一样),
而 SQL 一个字节都没变。工作区里只要有一个迁移文件被以 CRLF 重新检出过(本机就是 `0018_node_relay.sql`),
就会出现「只有那一条对不上」的混合库。

**修复**(`9cdaec6`,最小范围):

1. `.gitattributes`:`internal/database/migrations/*.sql text eol=lf` —— 任何平台、任何 autocrlf 设置下都按 LF 检出;
2. 哈希按 LF 归一后的内容算,新应用的迁移记这一份;校验时同时认 CRLF 形态与嵌入时的原始字节 ——
   **只放过换行这一种差异**,别的任何改动照样拒绝;
3. 库里已有的记录**一条不改写**,回滚到旧二进制后它仍认得自己当初那一份。

**自动化测试**(`internal/database/migrate_eol_test.go`,`go test` 即可重复执行):三种旧库(全 LF / 全 CRLF /
只有 0018 是 CRLF)都能打开且记录不变;LF 与 CRLF 两种换行下的真实内容改动都被拒;新库记 LF 哈希;
`.gitattributes` 规则存在且嵌入的迁移文件里没有 CR。

**旧数据库副本 × 修复前后的构建**(全部是 scratch 面板库的副本,每格重新拷一份;Linux 构建在容器里对
干净检出编译与运行):

| 旧库副本(记录的是) | 修复前 Windows CRLF | 修复前 Windows 混合 | 修复前 Linux | 修复后 Windows(autocrlf=true 新检出) | 修复后 Linux |
|---|---|---|---|---|---|
| 全 LF(Linux 构建写的) | ✗ 拒绝 | ✗ 拒绝 | ✓ | ✓ 记录不变 | ✓ 记录不变 |
| 只有 0018 是 CRLF(本机实际产生的) | ✗ 拒绝 | ✓ | ✗ 拒绝 | ✓ 记录不变 | ✓ 记录不变 |
| 全 CRLF(Windows autocrlf 构建写的) | ✓ | ✗ 拒绝 | ✗ 拒绝 | ✓ 记录不变 | ✓ 记录不变 |

回滚:三份库先由修复后的 Linux 构建打开,再交回各自原来的旧二进制,全部正常。
`autocrlf=true` 的新检出里 Go 源码是 CRLF、迁移文件是 LF,确认 `.gitattributes` 生效。

**限制**:修复后新应用的迁移记 LF 哈希,**旧的 Windows CRLF 构建**不认它们 —— 只在「用新版本应用了新迁移之后
又回滚到一个 Windows 上编的旧二进制」时才会遇到;本轮没有新迁移,生产在服务器上(Linux)构建,不受影响。
已有的 Windows 工作区要把迁移文件删掉重新 checkout 一次(上面那条测试会指出来)。

## 5. 测试与 CI

| 项 | 结果 |
|---|---|
| Go · Windows | `go vet ./...` 通过;`go test ./... -count=1` 24 个包全部通过 |
| Go · Linux 容器预演 | 对 `git archive` 的干净检出按 CI 的步骤跑:占位产物 → vet → gofmt(排除 `phase0/`)→ 24 个包全部通过 |
| **CI 隐患(已修)** | 干净检出上 `web/dist` 不存在(整个在 `.gitignore` 里,`embed.go` 注释里说的占位文件从没提交过),Go job 的 `go vet` 第一步就会报 `pattern all:dist: no matching files found`。容器里复现后在 Go job 加了占位步骤(`8515550`) |
| 前端 · Windows | `vue-tsc`、e2e 的 `tsc` 通过;`vite build` 通过;对构建产物跑 Playwright 功能用例 94 个全部通过;截图基线 26 张全部通过 |
| 前端 · Linux 容器预演 | 在 `mcr.microsoft.com/playwright:v1.63.0-noble` 里对 `git archive` 的干净检出按 CI 的步骤跑(`CI=1`):`npm ci` → `vue-tsc` 与 e2e 的 `tsc` → `vite build` → Playwright 功能用例 **94 passed**,没有靠重试才通过的用例 |
| GitHub Actions · push | [运行 39043997676](https://github.com/KJoner/litebox/actions/runs/39043997676) **success**:Go job(占位产物 → vet → gofmt → test)与 web job(`npm ci` → 类型检查 → 构建 → 装浏览器 → Playwright 功能用例 **94 passed**)全绿,没有靠重试才通过的用例 |
| GitHub Actions · PR | [运行 39044467146](https://github.com/KJoner/litebox/actions/runs/39044467146)(`pull_request`,对合并到 master 之后的结果跑)**success**:Go 与 web 两个 job 全绿,Playwright 功能用例 **94 passed** |
| CI 平台提示(不影响本 PR) | `actions/checkout@v4`、`setup-go@v5`、`setup-node@v4` 目标是 Node 20,被强制跑在 Node 24 上(警告);`ubuntu-latest` 从 2026-10-19 起迁到 Ubuntu 26 |
| PR | Draft [#1](https://github.com/KJoner/litebox/pull/1),正文见附录 |

**关于「120 个用例」**:Playwright 共 120 个 = **94 个功能用例 + 26 张截图基线**。CI 只跑 94 个功能用例;
截图基线按平台存放、目前只有 win32 一份,字体渲染随平台变,在 Linux 上比对必然不同 ——
要在 Linux 上跑这 26 张,得先在 Linux 上生成一份基线并逐张人工复核,而不是覆盖现有基线。本轮不做。

## 6. 预发布验证(测试数据库副本)

在本机用 scratch 面板数据库的副本演练,两个二进制都从 LF 检出构建(与服务器上一键脚本的构建方式一致):

| 步骤 | 结果 |
|---|---|
| 旧版(`7790978`)启动、只读验收 | 健康检查、管理员与门户登录、14 个管理接口、5 个门户接口、三种订阅输出全部正常 |
| 运行中 `litebox backup` | 生成 WAL 安全的备份,旧二进制另存 |
| 停服 → 新版 `migrate` | 数据库已是最新(版本 38),无迁移执行,一致性检查通过 |
| 新版启动、只读验收 | 全部正常;`check` 通过;**三种订阅输出与旧版逐字节一致** |
| 授权测试节点 · 配置一致的下发 | 返回「配置已一致」,下发步骤 SKIPPED,sing-box 进程号与运行时长不变 |
| 授权测试节点 · 配置有变化的下发 | 在副本里改掉上次下发哈希:前置检查判为需要下发 → 替换配置 → 重启(进程号变化)→ 真实拨测通过 |
| 回滚:旧二进制直接起在升级后的库上 | 全部正常,三种订阅与升级前逐字节一致 |
| 备份可用性 | 升级前备份能被旧版打开并通过 `check` |

迁移哈希的修复在这之后才做,它的验证见第 4 节。另一台测试机因主机密钥与记录不一致,未参与任何验证。

## 7. 升级步骤(合并 master 之后)

**合并 master 即发布**:一键脚本 `litebox-install.sh` 用 `git fetch origin master && reset --hard`
更新源码后现场构建,再调用 `upgrade.sh`。

1. **升级前**:另存一份 `/etc/litebox/`(`litebox.yaml` 与含主密钥的 `litebox.env`)——
   `upgrade.sh` 只备份数据库与旧二进制,主密钥要与备份分开保存。若生产库是用 Windows 上编的二进制
   初始化过的,修复后的版本同样能打开(第 4 节),不再需要预先处理
2. **升级**:在主控上重跑一键脚本(或 `make build-linux` 后 `BINARY=bin/litebox-linux-<arch> scripts/upgrade.sh`)。
   脚本会:运行中备份数据库 → 另存旧二进制 → 停服换二进制 → `migrate`(本轮为空操作)→ 启动 →
   `/api/health` 检查 → `check`;任一步失败自动换回旧二进制
3. **验收**:管理员与门户登录;节点列表、巡检、仪表盘;拉一次订阅;在**测试节点**上点一次「下发配置」,
   确认配置一致时返回「配置已一致」而不重启
4. 已打开的面板标签页刷新一次
5. `npm ci` 会多装开发依赖 `@playwright/test`(纯 JS,不下载浏览器),构建产物不含测试代码

## 8. 回滚步骤

本轮**没有新迁移,不改写任何迁移记录,也不改节点配置的渲染**,所以回滚只是换回二进制,
不需要恢复数据库、不需要重新部署任何节点。

1. 换回旧二进制(`upgrade.sh` 已把它存在数据库目录的 `backup/litebox-<时间戳>.bin`):
   ```bash
   systemctl stop litebox
   install -m 0755 /var/lib/litebox/backup/litebox-<时间戳>.bin /opt/litebox/litebox.new
   mv /opt/litebox/litebox.new /opt/litebox/litebox
   systemctl start litebox
   curl -fsS http://127.0.0.1:<端口>/api/health
   ```
2. **只有在想丢弃升级之后产生的数据时**才恢复数据库:`scripts/restore.sh <pre-upgrade-*.db>`
   (会先自检备份、要求输入 yes;升级后新增的流量账本、部署记录会丢失,主密钥必须是同一把)
3. 代码层面:`git revert` 合并提交只影响**之后**用一键脚本安装 / 更新的机器;已经升级的机器按第 1 步换二进制
4. UI 2.0 五批已在 master 上;若连它也要回退,需要回退那五个提交 —— 同样没有迁移,换二进制即可

## 9. 阻塞项与剩余风险

| # | 级别 | 内容 | 需要的动作 |
|---|---|---|---|
| 1 | 高 | 公开 master 的当前版本仍含 64 张截图与真实地址(历史里永久保留) | 本 PR 合并时随第一个提交一起清掉;想更早缩小暴露面,可把 `2f23204` 单独开 PR 先合 |
| 2 | 中 | 两台测试机未加固(口令登录开着,管理端点已公开) | 仓库所有者已决定本轮不处理、不作为前置条件;建议至少换掉 root 口令 |
| 3 | 中 | 一台测试机的 SSH 主机密钥与记录不一致 | 经服务商控制台核实身份;核实前不接受新密钥、不纳入验证 |
| 4 | 低 | ~~`ubuntu-latest` 迁到 Ubuntu 26 后 Playwright 的 `install --with-deps` 可能要跟着调~~ —— 已把 `runs-on` 固定成 `ubuntu-24.04`;几个 action 的 Node 20 弃用警告仍在 | 换发行版时与升级 Playwright 一起做;需要时升级 action 主版本 |
| 5 | 中 | 「配置一致跳过」的判据没有单元测试 | 后续把判据抽成纯函数并补测试 |
| 6 | 低 | 旧的 Windows CRLF 构建不认修复后新应用的迁移(第 4 节「限制」) | 生产在 Linux 上构建,不受影响 |
| 7 | 低 | 仪表盘「刷新」与「重试」并发时的请求先后 | 可后续加请求序号 |
| 8 | 低 | 截图基线只有 win32 一份,CI 不比对截图 | 需要时在 Linux 上生成并人工复核 |
| 9 | 低 | 本地备份分支与 bundle 里仍有清理前的原始数据 | 只在本机;合并完成后由仓库所有者删除,**任何时候都不要推送** |

## 10. 结论

**A. 可以合并。** 本地 Windows、Linux 容器与 GitHub Actions 上全部测试通过;升级、跳过重启、回滚都在
测试数据库副本与授权测试节点上验证过;迁移哈希的跨平台问题已修复并用旧库副本交叉验证;
推送的对象逐个核查过,不含真实管理地址、端口、指纹或凭据。第 9 节没有阻塞项。
Draft PR [#1](https://github.com/KJoner/litebox/pull/1) 可以转为 Ready for Review;**合并 master 即发布**,
由仓库所有者决定时机。

## 附录:PR 正文草稿

> **feat: LiteBox UI 2.0.1 稳定性改进(含公开仓库数据清理与迁移哈希修复)**
>
> UI 2.0 五批已在 master(`7c8fbd8..7790978`)。
>
> - 安全清理(第一个提交,也可单独合并):删除 `docs/ui-modernization/screenshots` 下含真实管理地址的截图,
>   文档与测试里的真实地址换成文档地址段,`CLAUDE.md` 加数据卫生约束。不改写历史
> - 稳定性与体验:仪表盘任何来源读失败都不再显示「一切正常」,巡检与同步失败进「需要处理」;
>   侧栏同步状态不再自相矛盾;门户「额度已用」与「本月 UTC」分开标注;静态弹窗跟随主题与语言包、
>   长日志框内滚动;768 宽操作列可达;49 处链接式操作改为键盘可达元素
> - 既有 bug:服务卡片「下发配置」误传 `force`,每次下发都强制重启 sing-box(V20 起);
>   节点部署记录读失败显示成「还没有部署记录」;迁移哈希因 CRLF / LF 检出不同,Windows 与 Linux 构建
>   互相拒库(只放过换行差异,不改写记录)
> - 测试:Playwright 94 个功能用例 + 26 张截图基线(全程模拟接口,写操作须显式模拟);Go 全量测试;
>   迁移换行兼容的测试
> - CI:`.github/workflows/ci.yml`(Go vet / gofmt / test;前端类型检查、构建、Playwright 功能用例)
> - 兼容与回滚:无新迁移,接口只加字段,回滚换二进制即可;详见
>   `docs/ui-modernization/04-ui-2.0.1.md` 与 `05-release-readiness.md`
