import { test, expect, adminOnly, type ApiMock } from './support/mock'

/**
 * 资产卡片:仪表盘卡片视图、套餐与续费弹窗、节点详情「网络监测」Tab。
 *
 * 守住需求书里那几条不可妥协的:不为美观伪造数据(外部线路的资源写「不可用」、没有样本的窗口
 * 画成空心)、不冒充三网测点、多币种不相加、卡片不按卡片逐个请求、TCP 失败率不叫丢包;
 * 出站网络质量(节点本机测公共目标)与入口连通性(主控测节点)分开显示,采集失败不算丢包。
 */

test.beforeEach(({ api }) => {
  adminOnly(api)
})

const card = (page: import('@playwright/test').Page, key: string) => page.getByTestId(`asset-card-${key}`)

test('默认是卡片视图;切到原有视图后刷新仍是原有视图', async ({ page }) => {
  await page.goto('/dashboard')
  const toggle = page.getByTestId('dashboard-view-toggle')
  await expect(toggle.getByRole('tab', { name: '卡片视图' })).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByTestId('asset-overview')).toBeVisible()
  await toggle.getByRole('tab', { name: '原有视图' }).click()
  await expect(page.getByText('节点健康', { exact: true })).toBeVisible()
  await page.reload()
  await expect(page.getByTestId('dashboard-view-toggle').getByRole('tab', { name: '原有视图' })).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByTestId('asset-overview')).toHaveCount(0)
})

test('三类卡片:自建节点有真实资源与探测,外部线路明确标「不可用」', async ({ page }) => {
  await page.goto('/dashboard')
  const a = card(page, 'NODE-1')
  await expect(a).toContainText('上海-alpha')
  await expect(a).toContainText('CN')
  // 出站在前(节点本机测公共目标),一个目标一行;入口连通性单列在后。
  await expect(a).toContainText('节点本机 · 出站')
  const icmp = a.getByTestId('probe-row-11')
  await expect(icmp).toHaveAttribute('data-direction', 'OUTBOUND')
  await expect(icmp).toContainText('公共 DNS(演示)')
  await expect(icmp).toContainText('ICMP')
  await expect(icmp).toContainText('25 ms')
  await expect(icmp).toContainText('1h 丢包 0.4%')
  await expect(icmp.locator('[data-state="PARTIAL"]')).toHaveCount(1)
  const tcp = a.getByTestId('probe-row-12')
  await expect(tcp).toContainText('TCP 443')
  await expect(tcp).toContainText('1h 失败率 0%')
  await expect(tcp).not.toContainText('丢包')
  await expect(a).toContainText('入口连通性')
  await expect(a).toContainText('主控探测 · TCP')
  await expect(a.getByTestId('probe-row-1')).toHaveAttribute('data-direction', 'INGRESS')
  await expect(a.getByTestId('probe-row-1')).toContainText('上海-alpha · VLESS')
  // 10 ms 以上取整显示。
  await expect(a.getByTestId('probe-row-1')).toContainText('37 ms')
  // 没有样本的窗口是空心点,不是绿点。
  await expect(a.getByTestId('probe-row-1').locator('[data-state="NONE"]')).toHaveCount(1)
  await expect(a.getByTestId('probe-row-1').locator('[data-state="FAIL"]')).toHaveCount(1)
  // 套餐:月付 5.00 USD,剩余未摊销价值按时间估算;不限量节点不给按流量估算。
  await expect(a).toContainText('月付')
  await expect(a).toContainText('5.00 USD')
  await expect(a).toContainText('剩余未摊销价值(估算)')
  await expect(a).toContainText('3.95 USD')

  // 东京只有出站(公共 DNS 对全部节点启用),没有入口目标:不出现「入口连通性」那一小块。
  const b = card(page, 'NODE-2')
  await expect(b.getByTestId('probe-row-13')).toContainText('最近一轮丢包 1/4')
  await expect(b.getByTestId('probe-row-13')).toContainText('88 ms')
  // 采集失败的窗口单独画,不按失败画。
  await expect(b.getByTestId('probe-row-13').locator('[data-state="SSH"]')).toHaveCount(1)
  await expect(b.getByTestId('probe-row-13').locator('[data-state="FAIL"]')).toHaveCount(0)
  await expect(b).not.toContainText('入口连通性')
  await expect(b).toContainText('未设置套餐')

  const x = card(page, 'EXTERNAL_PROXY-1')
  await expect(x).toContainText('机场 HK01')
  await expect(x).toContainText('CPU / 内存 / 磁盘不可用')
  await expect(x).toContainText('外部线路不做定时探测')
  await expect(x).not.toContainText('0%')
  await expect(x).toContainText('120.00 CNY')
})

test('不冒充三网测点;只有 ICMP 叫丢包,TCP 一律叫失败率', async ({ page }) => {
  await page.goto('/dashboard')
  await expect(page.getByTestId('asset-overview')).toBeVisible()
  for (const isp of ['电信', '联通', '移动']) {
    await expect(page.locator('.acv')).not.toContainText(isp)
  }
  for (const row of await page.locator('[data-direction="INGRESS"]').all()) {
    await expect(row).not.toContainText('丢包')
  }
  await expect(page.getByTestId('probe-row-12')).not.toContainText('丢包')
})

test('卡片不按卡片逐个请求:聚合接口一次,不碰任何单节点接口', async ({ page, api }) => {
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toBeVisible()
  expect(api.requests('GET', '/api/dashboard/cards')).toHaveLength(1)
  expect(api.requests('GET', /^\/api\/nodes\/\d+/)).toHaveLength(0)
  expect(api.requests('GET', /^\/api\/probe-targets\//)).toHaveLength(0)
  expect(api.mutations()).toHaveLength(0)
})

test('概览:成本按币种分列,不相加;7 天内到期', async ({ page }) => {
  await page.goto('/dashboard')
  const ov = page.getByTestId('asset-overview')
  await expect(ov).toContainText('5.00 USD')
  await expect(ov).toContainText('10.00 CNY')
  await expect(ov).not.toContainText('15.00')
  await expect(ov).toContainText('0 / 5 失败')
  await expect(ov).toContainText('出站 3 · 入口 2')
})

test('筛选与排序:地区、状态、搜索', async ({ page }) => {
  await page.goto('/dashboard')
  await expect(page.locator('.ac')).toHaveCount(3)
  await page.getByLabel('按名称搜索').fill('东京')
  await expect(page.locator('.ac')).toHaveCount(1)
  await expect(card(page, 'NODE-2')).toBeVisible()
  await page.getByRole('button', { name: '清除筛选' }).click()
  await expect(page.locator('.ac')).toHaveCount(3)
  // 状态:网络检测失败 —— 固定数据里没有,显示「没有符合筛选条件的卡片」,不是「还没有任何资产」。
  await page.getByLabel('状态').click()
  await page.locator('.ant-select-item-option', { hasText: '网络检测失败' }).click()
  await expect(page.getByText('没有符合筛选条件的卡片')).toBeVisible()
})

test('出站探测连续失败:卡片标红、进「需要处理」,说法是「本机测的」', async ({ page, api }) => {
  calmish(api).patch('GET /api/dashboard/cards', (d: { outbound: Record<string, unknown>[] }) => {
    Object.assign(d.outbound[0]!, {
      state: 'DOWN', state_text: '连续 3 次失败', consecutive_failures: 3, alert_state: 'DOWN', last_status: 'TIMEOUT',
    })
  })
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toContainText('出站探测失败')
  await expect(page.getByTestId('dashboard-headline')).toContainText('1 条预警')
  await expect(page.getByText(/到「公共 DNS\(演示\)」\(ICMP,IPv4\)连续 3 次全部丢包/)).toBeVisible()
  await expect(page.getByText(/在这台机器本机测的,不代表用户连不上它/)).toBeVisible()
})

test('出站采集失败:不算丢包、不进失败计数、不进「需要处理」', async ({ page, api }) => {
  calmish(api).patch('GET /api/dashboard/cards', (d: { outbound: Record<string, unknown>[] }) => {
    Object.assign(d.outbound[2]!, {
      state: 'COLLECT_FAILED', state_text: '采集失败:面板连不上这台机器,不代表目标不可达', last_status: 'SSH_ERROR',
    })
  })
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-2')).toContainText('出站采集失败')
  await expect(card(page, 'NODE-2').getByTestId('probe-row-13')).toContainText('不代表目标不可达')
  await expect(card(page, 'NODE-2')).not.toContainText('出站探测失败')
  await expect(page.getByTestId('asset-overview')).toContainText('0 / 5 失败')
  await expect(page.getByTestId('dashboard-headline')).toContainText('一切正常')
})

test('入口连通性连续失败:卡片标红、进「需要处理」(预警,不说一切正常)', async ({ page, api }) => {
  calmish(api).patch('GET /api/dashboard/cards', (d: { targets: Record<string, unknown>[] }) => {
    Object.assign(d.targets[0]!, { state: 'DOWN', state_text: '连续 4 次失败', consecutive_failures: 4, alert_state: 'DOWN' })
  })
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toContainText('入口检测失败')
  await expect(page.getByTestId('dashboard-headline')).toContainText('1 条预警')
  await expect(page.getByText(/连续 4 次 TCP 连接失败/)).toBeVisible()
  await expect(page.getByRole('link', { name: /查看探测/ })).toHaveAttribute('href', '/nodes/1/network')
})

test('聚合接口读失败:点名、不说一切正常,卡片上估值与探测写「不可用」', async ({ page, api }) => {
  calmish(api).fail('GET', '/api/dashboard/cards')
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-partial-failure')).toContainText('网络探测与套餐估值')
  await expect(page.getByTestId('dashboard-headline')).not.toContainText('一切正常')
  await expect(page.getByText('套餐估值、地区标签与网络探测没有读到')).toBeVisible()
  await expect(card(page, 'NODE-1')).toContainText('网络探测数据没有读到')
  await expect(card(page, 'NODE-1')).toContainText('套餐与估值没有读到')
  await expect(card(page, 'NODE-1')).not.toContainText('未设置套餐')
})

/** 「什么都不用处理」的基线,与 dashboard.spec 的 calm 同一套。 */
function calmish(api: ApiMock) {
  return api
    .on('GET', '/api/dashboard/alerts', { json: { items: [] } })
    .on('GET', '/api/deployments', { json: { items: [] } })
    .on('GET', '/api/proxy-sources', { json: { items: [] } })
    .on('GET', '/api/traffic/status', { json: { failing_nodes: [], running: false } })
    .patch('GET /api/nodes', (d: { items: { expiry: unknown }[] }) => {
      for (const n of d.items) n.expiry = null
    })
    .patch('GET /api/nodes/health', (d: { items: Record<string, unknown>[] }) => {
      for (const h of d.items) Object.assign(h, { singbox: 'RUNNING', singbox_detail: 'started', recover_error: undefined })
    })
}

function expiryDetail(api: ApiMock) {
  const node = api.fixture<{ items: { id: number; expiry: Record<string, unknown> }[] }>('GET /api/nodes').items[0]!
  const billing = {
    plan_name: '1C1G-CN2', billing_cycle: 'MONTHLY', cycle_count: 0, cycle_unit: '', currency: 'USD', price_minor: 500,
    purchased_at: '', period_start: '', period_paid_minor: -1, period_carry_minor: 0, traffic_unlimited: false,
    cycle_label: '月付', price: '5.00', period_paid: '', period_carry: '', has_billing: true,
  }
  return {
    view: { ...node.expiry, billing },
    renewals: [],
    valuation: api.fixture<{ billing: { valuation: unknown }[] }>('GET /api/dashboard/cards').billing[0]!.valuation,
    labels: { kind: 'NODE', object_id: 1, region: 'CN', tags: ['CN2', '生产'] },
    lead_days: [7, 3, 1],
    send_time: '09:00',
    timezone: 'Asia/Shanghai',
  }
}

test('编辑套餐:保存时带上计费字段与标签,金额是十进制字符串', async ({ page, api }) => {
  const detail = expiryDetail(api)
  api
    .on('GET', '/api/expiry/NODE/1', { json: detail })
    .on('POST', '/api/expiry/NODE/1/renew', (req) => ({
      json: {
        plan: { old_expires_at: '2026-11-10T16:00:00Z', new_expires_at: '2026-12-10T16:00:00Z', base: 'CURRENT', base_at: '2026-11-10T16:00:00Z', method_text: '从原到期时间起延长 1 个月', method: 'MONTHS:1' },
        period: { start: '2026-10-17T11:46:21Z', paid_minor: (req.body as { amount?: string }).amount ? 480 : -1, paid: '4.80', carry_minor: 395, carry: '3.95', currency: 'USD' },
        duplicate: false, renewal: null, view: null,
      },
    }))
    .on('PUT', '/api/expiry/NODE/1', { json: detail.view })
    .on('PUT', '/api/asset-labels/NODE/1', { json: { kind: 'NODE', object_id: 1, region: 'CN', tags: ['CN2', '生产', 'BGP'] } })
  await page.goto('/dashboard')
  await card(page, 'NODE-1').getByRole('button', { name: '编辑套餐' }).click()
  const modal = page.locator('.ant-modal').filter({ hasText: '套餐与续费' })
  await expect(modal).toContainText('剩余未摊销价值')
  // 有固定周期的套餐默认「续一期」,预览把新的本期与结转说出来。
  await expect(modal.getByRole('button', { name: '续一期(月付)' })).toBeVisible()
  await expect(modal).toContainText('上一期没用完的 3.95 USD 结转进来')

  await modal.getByPlaceholder('留空 = 与套餐价格相同').fill('4.5')
  await modal.getByPlaceholder(/逗号分隔,最多 8 个/).fill('CN2, 生产, BGP')
  await modal.getByRole('button', { name: '保存档案' }).click()
  await expect(page.getByText('套餐与到期档案已保存')).toBeVisible()

  const put = api.requests('PUT', '/api/expiry/NODE/1')[0]!.body as { billing: Record<string, unknown> }
  expect(put.billing).toMatchObject({ billing_cycle: 'MONTHLY', currency: 'USD', price: '5.00', period_paid: '4.5' })
  const labels = api.requests('PUT', '/api/asset-labels/NODE/1')[0]!.body as { tags: string[] }
  expect(labels.tags).toContain('BGP')
  // 只做了预览,没有真的续费。
  expect(api.requests('POST', '/api/expiry/NODE/1/renew').every((r) => (r.body as { preview?: boolean }).preview)).toBe(true)
})

test('网络监测 Tab · 入口连通性:只能从候选里选目标;历史按 24 小时显示失败点与口径', async ({ page, api }) => {
  api
    .on('POST', '/api/nodes/1/probe-targets', (req) => ({
      json: { id: 9, node_id: 1, label: '上海-alpha · VLESS', ...(req.body as object) },
    }))
    // 新建之后页面会立刻去看它的历史(固定数据里没有 9 号,借 1 号的形状)。
    .on('GET', /^\/api\/probe-targets\/9\/history/, { json: api.fixture('GET /api/probe-targets/1/history?range=24h') })
  await page.goto('/nodes/1/network')
  await expect(page.getByRole('tab', { name: '网络监测' })).toHaveAttribute('aria-selected', 'true')
  const ingress = page.getByTestId('probe-ingress')
  await expect(ingress).toContainText('主控探测 · TCP · 每 60 秒')
  // 已在监测的两个不再出现在候选里,只剩 IPv6 那一条。
  // 固定时钟下(page.clock.setFixedTime)AntD 的空值下拉框点一下不会展开 —— 撤掉固定时钟就正常,
  // 是测试环境的副作用,不是界面问题。这里点一下让它拿到焦点,再用方向键展开。
  await page.locator('.nn__pick').click()
  await page.keyboard.press('ArrowDown')
  const opts = page.locator('.ant-select-item-option')
  await expect(opts).toHaveCount(1)
  await expect(opts.first()).toContainText('IPv6')
  await opts.first().click()
  await page.getByRole('button', { name: '开始监测' }).click()
  const body = api.requests('POST', '/api/nodes/1/probe-targets')[0]!.body
  expect(body).toEqual({ ref: 'EP:7', family: 'V6' })

  // 历史:选中对外服务那一行,24 小时。
  await page.getByRole('button', { name: /上海-alpha · VLESS/ }).first().click()
  await expect(page.getByText('最近的异常')).toBeVisible()
  await expect(page.getByText('DNS 失败(不计入)')).toBeVisible()
  await expect(page.getByText(/统计口径从/)).toBeVisible()
  await expect(page.locator('.nn__stats')).toContainText('TCP 连接次数')
  await expect(ingress).not.toContainText('丢包')
})

test('网络监测 Tab · 出站:每个公共目标一行,本机开关三态,历史按包算丢包、采集失败另计', async ({ page, api }) => {
  api.on('PUT', '/api/nodes/1/probe-public/1', { json: { message: '已保存' } })
  await page.goto('/nodes/1/network')
  const out = page.getByTestId('probe-outbound')
  await expect(out).toContainText('节点本机')
  const dns = page.getByTestId('probe-public-1')
  await expect(dns).toContainText('公共 DNS(演示)')
  await expect(dns).toContainText('ICMP')
  await expect(dns.getByRole('radio', { name: '跟随全局' })).toHaveAttribute('aria-checked', 'true')
  await expect(page.getByTestId('probe-public-2').getByRole('radio', { name: '启用' })).toHaveAttribute('aria-checked', 'true')

  // 默认选中第一条出站序列:ICMP 的历史按包算丢包,采集失败另列、不计入。
  await expect(page.locator('.nn__stats')).toContainText('ICMP 丢包率(按包)')
  await expect(page.locator('.nn__stats')).toContainText('采集失败 / 工具不可用(不计入)')
  await expect(page.locator('.nn__fails')).toContainText('采集失败')
  await expect(page.locator('.nn__fails')).toContainText('3/4 收到')

  // 在这台机器上停用:发的是 OFF,不是去改公共目标本身。
  await dns.getByRole('radio', { name: '停用' }).click()
  await expect.poll(() => api.requests('PUT', '/api/nodes/1/probe-public/1').length).toBe(1)
  expect(api.requests('PUT', '/api/nodes/1/probe-public/1')[0]!.body).toEqual({ mode: 'OFF' })
  expect(api.requests('PUT', /^\/api\/probe\/public-targets/)).toHaveLength(0)
})

test('系统设置 · 公共探测目标搬到了独立的目标库页,旧锚点落在一句指路的话上', async ({ page }) => {
  await page.goto('/settings#probe-public')
  const moved = page.getByTestId('public-probe-moved')
  await expect(moved).toContainText('探测目标库')
  await moved.getByRole('link', { name: '打开探测目标库' }).click()
  await expect(page).toHaveURL(/\/probe-library$/)
  await expect(page.getByRole('heading', { name: '探测目标库' })).toBeVisible()
})
