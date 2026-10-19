import { test, expect, adminOnly } from './support/mock'

/**
 * 探针系统增强三项(V22.6):自建节点互测入站(PEER)、出站 HTTP(S)、卡片自定义显示名称。
 *
 * 守住的几条:PEER 与主控、Boce 分区显示、来源机器连不上记采集失败(比例是「—」不是 0% 或 100%)、
 * 新建按「来源 × 测法」各一条;HTTP 的 404 / 5xx 都算取得了响应、5xx 另外标出来、查询串不出现在列表里;
 * 显示名称只改卡片上的名字,原始名称在悬停说明里,改名与模式、顺序解耦。全部打模拟层,不连任何节点。
 */

test.beforeEach(({ api }) => {
  adminOnly(api)
  // 选中 PEER / 新建的序列时页面会去看它的历史:借 1 号的形状(固定数据里只有几条)。
  api.on('GET', /^\/api\/probe-targets\/(31|32|33|15)\/history/, { json: api.fixture('GET /api/probe-targets/1/history?range=24h') })
})

test('网络监测 · 自建节点拨测:单独一块、来源连不上不算失败、按「来源 × 测法」新建、立即检测与暂停', async ({ page, api }) => {
  api
    .on('POST', '/api/nodes/1/probe-peers', { json: { created: [{ id: 33, enabled: true }], duplicates: 0 } })
    .on('POST', '/api/nodes/1/probe-peers/run', (req) => ({
      json: {
        results: (req.body as { ids: number[] }).ids.map((id) => ({
          id, status: 'OK', label: '全部收到', rtt_ms: 47.9, error: '', recorded: true,
        })),
      },
    }))
    .on('PUT', '/api/probe-targets/31', (req) => ({ json: { id: 31, ...(req.body as object) } }))
  await page.goto('/nodes/1/network')
  const peer = page.getByTestId('probe-peer')
  await expect(peer).toContainText('间隔跟随入口检测 · 每 60 秒')
  // 两条:东京-beta 用 Ping 与 TCPing 测这台的 VLESS 入口。Ping 只测地址。
  const ping = page.getByTestId('peer-row-31')
  await expect(ping).toContainText('东京-beta')
  await expect(ping).toContainText('Ping')
  await expect(ping).toContainText('→ 上海-alpha · VLESS')
  await expect(ping).toContainText('49 ms')
  await expect(ping).toContainText('1h 丢包')
  await expect(ping.locator('.np__addr')).toHaveText('203.0.113.11')
  // 来源机器连不上:采集失败,比例是「—」,不说这台机器丢包。
  const tcping = page.getByTestId('peer-row-32')
  await expect(tcping).toContainText('TCPing')
  await expect(tcping).toContainText('采集失败')
  await expect(tcping).toContainText('1h 失败率 — · 24h —')
  await expect(tcping.locator('.np__addr')).toHaveText('203.0.113.11:8443')
  // 主控的入口连通性那一块里没有 PEER 的行。
  await expect(page.getByTestId('probe-ingress')).not.toContainText('东京-beta')

  // 按来源筛选:只看自建节点。
  await page.getByLabel('来源').click()
  await page.locator('.ant-select-item-option', { hasText: '自建节点(入站)' }).click()
  await expect(page.getByTestId('probe-outbound')).toHaveCount(0)
  await expect(page.getByTestId('probe-ingress')).toHaveCount(0)
  await expect(peer).toBeVisible()

  // 新建:被测入口只从订阅入口里选(没有管理通道),来源可多选,两种测法默认都勾。
  await page.getByTestId('peer-add-open').click()
  await page.getByTestId('peer-target').click()
  await page.keyboard.press('ArrowDown')
  const targets = page.locator('.ant-select-dropdown:visible .ant-select-item-option')
  await expect(targets).toHaveCount(2)
  await expect(targets.filter({ hasText: '管理 SSH' })).toHaveCount(0)
  await targets.filter({ hasText: '203.0.113.11:8443' }).click()
  await page.getByTestId('peer-sources').click()
  await page.keyboard.press('ArrowDown')
  await page.locator('.ant-select-dropdown:visible .ant-select-item-option', { hasText: '东京-beta' }).click()
  await page.keyboard.press('Escape')
  await page.getByTestId('peer-create').click()
  await expect.poll(() => api.requests('POST', '/api/nodes/1/probe-peers').length).toBe(1)
  expect(api.requests('POST', '/api/nodes/1/probe-peers')[0]!.body).toEqual({
    ref: 'EP:1', family: 'V4', source_node_ids: [2], methods: ['ICMP', 'TCP'], enabled: true,
  })

  // 立即检测与暂停周期检测。
  await page.getByTestId('peer-run-31').click()
  await expect.poll(() => api.requests('POST', '/api/nodes/1/probe-peers/run').length).toBe(1)
  expect(api.requests('POST', '/api/nodes/1/probe-peers/run')[0]!.body).toEqual({ ids: [31] })
  await expect(page.getByText('已检测 1 条,结果记入历史')).toBeVisible()
  await ping.getByRole('button', { name: '暂停周期检测' }).click()
  await expect.poll(() => api.requests('PUT', '/api/probe-targets/31').length).toBe(1)
  expect(api.requests('PUT', '/api/probe-targets/31')[0]!.body).toEqual({ enabled: false })

  // 历史:写明来源与测法;采集失败另计、不进比例。
  await ping.getByRole('button', { name: /东京-beta/ }).click()
  const hist = page.locator('.nn__hist')
  await expect(hist).toContainText('自建节点 东京-beta')
  await expect(hist).toContainText('Ping')
  await expect(hist).toContainText('采集失败 / 工具不可用(不计入)')
})

test('网络监测 · 出站 HTTP(S):5xx 另外标出来,列表里不出现查询串', async ({ page }) => {
  await page.goto('/nodes/1/network')
  const row = page.getByTestId('probe-public-9')
  await expect(row).toContainText('HTTP 健康检查(演示)')
  await expect(row).toContainText('https://www.example.com/health?…')
  await expect(row).not.toContainText('demo-only')
  await expect(row).toContainText('HTTP 503')
  await expect(row).toContainText('1h 请求失败')
  await expect(row.locator('[data-state="HTTP5XX"]')).not.toHaveCount(0)
  // 历史:HTTP 请求失败率与状态码分布,耗时叫「响应耗时」。
  await row.getByRole('button', { name: /HTTP 健康检查/ }).click()
  const stats = page.locator('.nn__stats')
  await expect(stats).toContainText('HTTP 请求失败率')
  await expect(stats).toContainText('状态码(都算取得了响应)')
  await expect(stats).toContainText('平均响应耗时')
})

test('目标库 · 新增 HTTP(S) 目标:主机与端口由 URL 推出来,默认超时 5 秒,登记不启用', async ({ page, api }) => {
  const nine = (api.fixture('GET /api/probe/public-targets') as { targets: { id: number }[] }).targets.find((t) => t.id === 9)
  api
    .on('POST', '/api/probe/public-targets', (req) => ({ json: { ...nine, id: 10, ...(req.body as object), metric: 'HTTP' } }))
    .on('GET', /^\/api\/probe\/public-targets\/10\/nodes$/, { json: api.fixture('GET /api/probe/public-targets/2/nodes') })
  await page.goto('/probe-library')
  const table = page.getByTestId('probe-library-table')
  await expect(table).toContainText('HTTP 健康检查(演示)')
  await expect(table).toContainText('https://www.example.com/health?…')
  await expect(table).not.toContainText('demo-only')
  await expect(table).toContainText('HTTP(S)')

  await page.getByTestId('public-target-add').click()
  const form = page.getByTestId('public-target-form')
  await form.getByTestId('ptm-proto-http').click()
  await form.getByTestId('ptm-url').fill('https://www.example.com/generate_204?key=abc')
  await expect(form).toContainText('将由各节点连接 www.example.com:443(HTTPS,校验证书)')
  await form.getByPlaceholder('例如 自建测速点').fill('示例 204')
  await page.getByRole('dialog', { name: '新增探测目标' }).getByRole('button', { name: '登记' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/public-targets').length).toBe(1)
  const body = api.requests('POST', '/api/probe/public-targets')[0]!.body as Record<string, unknown>
  expect(body).toMatchObject({
    name: '示例 204', protocol: 'HTTP', url: 'https://www.example.com/generate_204?key=abc', host: 'www.example.com',
    port: 0, timeout_ms: 5000, family: 'V4',
  })
})

test('卡片展示设置 · 显示名称:逐项改名、清空恢复、超长拦下、预览与卡片一致', async ({ page, api }) => {
  api.on('PUT', '/api/nodes/1/probe-card', (req) => ({ json: { node_id: 1, updated_at: '', ...(req.body as object) } }))
  await page.goto('/nodes/1/network')
  await page.getByTestId('card-display-open').click()
  const dlg = page.getByTestId('card-display')
  const preview = dlg.getByTestId('card-preview-ingress')
  // 已有的别名回填进输入框;预览里显示别名。
  await expect(dlg.getByTestId('card-alias-22')).toHaveValue('广东联通入口')
  await expect(preview).toContainText('广东联通入口')
  await expect(dlg.getByTestId('card-preview-outbound')).toContainText('示例网站')
  // 给主控对外服务起个名字,预览跟着变。
  await dlg.getByTestId('card-alias-1').fill('主控入口')
  await expect(preview).toContainText('主控入口')
  // 超过 24 个字符:当场说出来,保存不发请求。
  await dlg.getByTestId('card-alias-21').fill('名'.repeat(25))
  await expect(dlg).toContainText('显示名称最长 24 个字符')
  await page.getByTestId('card-display-save').click()
  expect(api.requests('PUT', '/api/nodes/1/probe-card')).toHaveLength(0)
  await dlg.getByTestId('card-alias-21').fill('')
  // 清空 = 恢复原始名称。
  await dlg.getByTestId('card-alias-reset-22').click()
  await expect(dlg.getByTestId('card-alias-22')).toHaveValue('')
  await expect(preview).toContainText('广东 联通')
  await page.getByTestId('card-display-save').click()
  await expect.poll(() => api.requests('PUT', '/api/nodes/1/probe-card').length).toBe(1)
  // 展示方式与顺序原样;只发非空的别名。
  expect(api.requests('PUT', '/api/nodes/1/probe-card')[0]!.body).toEqual({
    ingress_mode: 'MANUAL', ingress_ids: [21, 22, 1], outbound_mode: 'AUTO', outbound_ids: [],
    ingress_aliases: { 1: '主控入口' }, outbound_aliases: { 12: '示例网站' },
  })
  // 只写偏好,不碰任何检测。
  expect(api.mutations().map((m) => m.path)).toEqual(['/api/nodes/1/probe-card'])
})
