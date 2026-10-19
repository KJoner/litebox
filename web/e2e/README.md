# 前端回归测试(Playwright)

UI 2.0.1 引入。**全程不连任何后端、不碰任何节点**:页面发出的每一个请求都经
`support/mock.ts` 拦下来,由固定数据回答。

## 运行

```bash
cd web
npm run test:e2e          # 功能回归(CI 跑的就是这个)
npm run test:e2e:visual   # 截图基线比对(只在本机跑,见下)
npm run test:e2e:update   # 界面有意改动之后更新截图基线
npm run typecheck:e2e     # 测试代码本身的类型检查
E2E_PREVIEW=1 npm run test:e2e   # 对 vite build 的产物跑(CI 的方式,要先 npx vite build)
```

第一次运行前装一次浏览器:`npx playwright install chromium`。

## 安全保证(改测试之前先读)

| 规矩 | 落在哪里 |
|---|---|
| 同源静态资源放行,`/api/` 与 `/sub/` 一律模拟,其它域名直接中止 | `ApiMock.handle` |
| 没模拟到的 GET 回 404 并记进 `unmocked`;用例结束时断言它为空 | `test` 夹具 |
| 写操作(POST / PUT / PATCH / DELETE)**没有显式 `api.on(...)` 就回 501**,同样判失败 —— 点「确认」打到的永远是模拟层 | `ApiMock.handle` |
| 拦截层是 `auto` 夹具,用例忘了写 `api` 参数也照样装上 | `test` 夹具 |
| vite 的代理目标指向 `127.0.0.1:9`(没人监听),万一有请求漏过拦截层,拿到的是连接拒绝 | `playwright.config.ts` |
| 时钟固定在录制那一刻(`CAPTURED_AT`),相对时间与截图才稳定 | `page.clock.setFixedTime` |

写操作的用例要断言**发了什么**:`api.requests('POST', '/api/nodes/1/deploy')` 取请求体,
`api.mutations()` 取全部写请求。「取消」类用例断言 `api.mutations()` 为空。

## 用例

| 文件 | 覆盖 |
|---|---|
| `auth.spec.ts` | 管理端登录、错误口令、登录后回跳、守卫;门户守卫、强制改密只能停在安全设置 |
| `layout.spec.ts` | 主题三档与跟随系统、侧栏逐项导航与面包屑、折叠偏好、手机抽屉、退出;侧栏同步状态的五种说法 |
| `dashboard.spec.ts` | 任一来源读失败都不说「一切正常」、严重项优先、只重试失败项、刷新失败保留旧结果;巡检与同步失败进清单 |
| `assets.spec.ts` | 资产卡片:默认卡片视图与偏好持久化、三类卡片、外部线路标「不可用」、不出现三网、聚合接口只请求一次、成本按币种不相加、筛选、TCP 失败进清单、读失败说「没有读到」、编辑套餐的请求体、网络监测 Tab 的白名单与历史 |
| `nodes.spec.ts` | 节点列表、搜索、行内菜单、名字是真链接、列表读失败、768 宽操作可达 |
| `node-detail.spec.ts` | 六个 Tab 与地址同步、新增入口菜单、下发配置的取消 / 确认 / 失败回滚 / 连接中断 |
| `modals.spec.ts` | 探测、TCP 调优、配置比对、确认框在长内容与 1440 / 390 两种宽度下;重新引导的多段说明;深色确认框 |
| `portal.spec.ts` | 「额度已用」与「本月 UTC」的口径标签、门户各页、读失败、不出现地址 |
| `probe-library.spec.ts` | 探测目标库:来源 / 方式 / 生效范围分列、按来源筛选;授权确认要先勾「已阅读」、未授权的目标启用前就提醒;Zstatic 预置的请求体与「导入不启用」;TcpQuality 上游不可用时目标库不动、上游改动默认不勾;节点详情里握手近似叫「无应答」、目标选择器与本机暂停只改这一台;节点列表批量应用 |
| `boce.spec.ts` | Boce 外部拨测:Key 只显示末 4 位、启用先确认、请求体不带 Key;校验 / 同步走免费接口、只读打开不发写请求;网络监测里任务与拨测点分行、拨测点异常不算失败、手动检测先确认并带请求 ID;按来源筛选;新建任务时 IPv6 入口选不了、已消失的拨测点不能选、默认不开自动检测;执行记录里预算拦下的写明「未执行」;卡片展示设置只写偏好、不碰 Boce;节点列表批量应用 |
| `responsive.spec.ts` | 768 宽各表操作列可达、仪表盘平板卡片、390 宽各页不横向溢出、键盘可达 |
| `visual.spec.ts` | `@visual` 截图基线:8 个页面(仪表盘原有视图与卡片视图各一组)× 浅色 1440 / 768 / 390 + 深色 1440,另有六张状态图(含 Boce 设置浅 / 深、新建任务弹窗、卡片展示设置) |

## 截图基线

存在 `__screenshots__/`,文件名带平台后缀(`-win32`、`-linux`)。字体渲染随平台变,
混用一份只会天天失败,所以 **CI 不跑 `@visual`**。容差按像素数(50)给而不是按比例 ——
整页截图有两百万像素,1% 的比例能放过一整句结论的变化,UI 2.0.1 里真的放过过一次。

界面有意改动后:`npm run test:e2e:update`,然后**逐张看新图**再提交。

两条已知的测试环境副作用(都不是界面问题):

* 时钟固定(`page.clock.setFixedTime`)时,AntD 的**空值**下拉框点一下不会展开,撤掉固定时钟就正常 ——
  用例里先点一下拿到焦点,再按方向键展开;
* 390 宽下节点详情的六个 Tab 放不下,AntD 有时把选中的那个滚进视野、有时不滚,截图时遮住 Tab 栏
  (它由 768 / 1440 两组基线盯着)。

仪表盘的视图偏好存在 `localStorage['lb.dashboard.view']`(`cards` / `classic`),默认是卡片视图;
测原有视图的用例用 `page.addInitScript` 先写进去。

## 固定数据

- `fixtures/api.json`:录到的 GET 响应,经脱敏;键是「方法 + 路径 + 排过序的查询串」
- `fixtures/manual.json`:录制那天拿不到、手工补的几条,同名以它为准。资产卡片的三条
  (`/api/dashboard/cards`、`/api/nodes/1/probe`、`/api/probe-targets/1/history`)是手写的演示数据,
  地址只用文档地址段

查找顺序:精确键 → 去掉查询串 → 同路径的任意一份(区间、条数不同的请求共用)。
用例里要别的状态时用 `api.patch(key, fn)` 改一份,或 `api.on(...)` 直接覆盖,不要改 json。

### 重新录制

对着**本机 scratch 面板**(不是生产)录,只读:录制脚本中止页面发出的一切写操作。

```bash
# 1. 起 scratch 面板(默认 127.0.0.1:18100),登录管理端与门户各一次,拿到两个会话 Cookie
# 2. 录
E2E_RECORD=1 E2E_ADMIN_COOKIE=<litebox_session> E2E_PORTAL_COOKIE=<litebox_portal_session> \
  E2E_RECORD_OUT=e2e/fixtures/raw.json npx playwright test
# 3. 脱敏(先照 tools/sanitize.local.example.json 写好 tools/sanitize.local.json)
node e2e/tools/sanitize.mjs e2e/fixtures/raw.json > e2e/fixtures/api.json
# 4. 人工核对:真实 IP、端口、主机指纹、主机别名、密钥一个都不能剩
grep -nE "<真实 IP 前缀>|<真实端口>|<主机别名>" e2e/fixtures/api.json
rm e2e/fixtures/raw.json     # raw*.json 在 .gitignore 里,但别留着
```

脱敏规则见 `tools/sanitize.mjs` 开头:公网 IPv4 → 203.0.113.0/24,IPv6 → 2001:db8::/32,
名字像凭据的字段(公钥、short id、哈希、token、口令……)→ 同长度同字符集的假值,
订阅地址里的 token 与 `SHA256:` 主机指纹换掉。**真实端口与主机别名的对照表不进仓库**:
放在 `tools/sanitize.local.json`(已 gitignore),新测试机要把它的端口与别名补进去;
没有这个文件时脚本照跑,但会在 stderr 警告「端口与主机别名不会被替换」。

## 数据卫生(必须遵守)

这是公开仓库,这个目录里的一切 —— 固定数据、截图基线、脚本、注释 —— 都按公开发布对待。

- **禁止**把真实服务器的管理信息(公网地址、SSH 端口、主机密钥指纹、`root@地址:端口`)、
  订阅 Token 与订阅地址、私钥 / 主密钥 / AccessKey、管理面板与门户的会话 Cookie
  写进截图、固定数据、测试代码或它们的注释;
- **截图基线只由模拟层渲染**,数据全部来自上面两份固定数据。不从连着真机的面板截图进仓库,
  也不拿 OCR 自动打码当唯一的脱敏手段 —— 实测迭代识别之后仍有漏网;
- 示例值用文档地址段:IPv4 `192.0.2.0/24` / `198.51.100.0/24` / `203.0.113.0/24`,
  IPv6 `2001:db8::/32`,域名 `example.com`;
- 提交前在待推送的范围上跑一次 Secret Scanner(如 `gitleaks git --log-opts="origin/master..HEAD"`),
  再按上面几类自己 grep 一遍 —— 通用扫描器不认 IP、端口、主机指纹。
录完之后时钟固定在新的 `capturedAt`,相对时间类断言若有变化要一起看。
