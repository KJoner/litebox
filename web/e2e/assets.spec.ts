import { test, expect, adminOnly, type ApiMock } from './support/mock'

/**
 * 资产卡片:仪表盘卡片视图、套餐与续费弹窗、节点详情「网络监测」Tab。
 *
 * 守住需求书里那几条不可妥协的:不为美观伪造数据(没有样本的窗口画成空心)、卡片视图只画自建节点、
 * 不冒充三网测点、多币种不相加、卡片不按卡片逐个请求、TCP 失败率不叫丢包;
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

test('只画自建节点:真实资源与探测;外部代理与代理源不在卡片视图', async ({ page }) => {
  await page.goto('/dashboard')
  const a = card(page, 'NODE-1')
  await expect(a).toContainText('上海-alpha')
  // 地区是旗帜(读屏读「地区:CN」);内部名称、类型、自建不再占行,收进名称的悬停说明。
  await expect(a.getByTestId('card-flag')).toHaveAttribute('aria-label', '地区:CN')
  await expect(a).not.toContainText('sh-node')
  await expect(a).not.toContainText('落地节点')
  await expect(a.getByRole('link', { name: '上海-alpha' })).toHaveAttribute('title', /内部名称:sh-node[\s\S]*落地节点 · 自建/)
  // 网络:入站在左、出站在右,一个目标一格;协议与地址族收进名字的悬停说明。
  const net = a.getByTestId('card-net')
  await expect(net.getByText('入站', { exact: true })).toHaveAttribute('title', '入站:主控 · TCP 建连 / Boce · TCPing')
  await expect(net.getByText('出站', { exact: true })).toBeVisible()
  const icmp = a.getByTestId('probe-row-11')
  await expect(icmp).toHaveAttribute('data-direction', 'OUTBOUND')
  await expect(icmp).toContainText('公共 DNS(演示)')
  await expect(icmp).not.toContainText('IPv4')
  await expect(icmp.getByText('公共 DNS(演示)')).toHaveAttribute('title', /节点本机 · ICMP · IPv4/)
  await expect(icmp).toContainText('25 ms')
  await expect(icmp).toContainText('1h 丢包 0.4%')
  await expect(icmp.locator('[data-state="PARTIAL"]')).toHaveCount(1)
  const tcp = a.getByTestId('probe-row-12')
  await expect(tcp).not.toContainText('TCP 443')
  await expect(tcp.locator('[title*="TCP 443"]')).toHaveCount(1)
  await expect(tcp).toContainText('1h 失败率 0%')
  await expect(tcp).not.toContainText('丢包')
  // 入站一组:按这台节点的「卡片展示设置」(手动选了 Boce 上海电信、广东联通与主控对外服务)。
  const boce = a.getByTestId('probe-row-21')
  await expect(boce).toHaveAttribute('data-source', 'BOCE')
  await expect(boce).toContainText('上海 电信')
  await expect(boce.getByText('上海 电信')).toHaveAttribute('title', /Boce 第三方拨测点 · TCPing/)
  await expect(boce).toContainText('168 ms')
  await expect(boce).toContainText('TCPing 丢包 0%')
  // 入站与出站在同一张两列的栅格里:入站那一格在出站左边。
  const boxIn = await boce.boundingBox()
  const boxOut = await icmp.boundingBox()
  expect(boxIn && boxOut && boxIn.x < boxOut.x).toBeTruthy()
  await expect(a.getByTestId('probe-row-22')).toContainText('最近一轮丢包 1/10')
  // 卡片别名(V22.6):「广东 联通」在这台节点的卡片上叫「广东联通入口」,悬停说明里仍有原始名称与来源;
  // 口径名(TCPing 丢包)不被别名覆盖。出站「示例网站 443(演示)」同理叫「示例网站」。
  const aliased = a.getByTestId('probe-row-22').getByTestId('probe-row-name')
  await expect(aliased).toHaveText('广东联通入口')
  await expect(aliased).toHaveAttribute('title', /显示名称「广东联通入口」,原始名称[\s\S]*广东 联通[\s\S]*Boce 第三方拨测点/)
  await expect(a.getByTestId('probe-row-22')).toContainText('TCPing 丢包')
  await expect(a.getByTestId('probe-row-12').getByTestId('probe-row-name')).toHaveText('示例网站')
  // 没选上的照样在测:卡片只说「还有几个」,不画出来。拨测点异常的那一条(北京移动)没选上,
  // 两条自建节点拨测(东京-beta → 这台)也没选上。
  await expect(a.getByTestId('probe-row-23')).toHaveCount(0)
  await expect(a.getByTestId('probe-row-14')).toHaveCount(0)
  await expect(a.getByTestId('probe-row-31')).toHaveCount(0)
  await expect(a).toContainText('还有 5 个目标')
  await expect(a.getByTestId('probe-row-1')).toHaveAttribute('data-direction', 'INGRESS')
  await expect(a.getByTestId('probe-row-1')).toContainText('上海-alpha · VLESS')
  // 10 ms 以上取整显示。
  await expect(a.getByTestId('probe-row-1')).toContainText('37 ms')
  // 没有样本的窗口是空心点,不是绿点。
  await expect(a.getByTestId('probe-row-1').locator('[data-state="NONE"]')).toHaveCount(1)
  await expect(a.getByTestId('probe-row-1').locator('[data-state="FAIL"]')).toHaveCount(1)
  // 套餐两行:「月付 5.00 USD · 下次续费」一行,「剩余 N 天 · 剩余价值(估算)」一行;不限量节点不给按流量估算。
  await expect(a.getByTestId('card-bill-plan')).toContainText('月付')
  await expect(a.getByTestId('card-bill-plan')).toContainText('5.00 USD')
  await expect(a.getByTestId('card-bill-value')).toContainText('剩余 25 天')
  await expect(a.getByTestId('card-bill-value')).toContainText('剩余价值(估算)')
  await expect(a.getByTestId('card-bill-value')).toContainText('3.95 USD')
  await expect(a.getByTestId('card-bill-value')).not.toContainText('按流量')

  // 东京只有出站(公共 DNS 对全部节点启用),没有入站序列:入站那一列只写「未启用」。
  const b = card(page, 'NODE-2')
  await expect(b.getByTestId('probe-row-13')).toContainText('最近一轮丢包 1/4')
  await expect(b.getByTestId('probe-row-13')).toContainText('88 ms')
  // 采集失败的窗口单独画,不按失败画。
  await expect(b.getByTestId('probe-row-13').locator('[data-state="SSH"]')).toHaveCount(1)
  await expect(b.getByTestId('probe-row-13').locator('[data-state="FAIL"]')).toHaveCount(0)
  await expect(b.locator('[data-direction="INGRESS"]')).toHaveCount(0)
  await expect(b.getByTestId('card-net')).toContainText('未启用')
  await expect(b).toContainText('未设置套餐')

  // 外部代理(机场 HK01)与代理源不画卡片:它们没有资源数据、不做定时探测,到期照样进「需要处理」。
  await expect(card(page, 'EXTERNAL_PROXY-1')).toHaveCount(0)
  await expect(page.locator('[data-testid^="asset-card-PROXY_SOURCE-"]')).toHaveCount(0)
  await expect(page.locator('.ac')).toHaveCount(2)
})

test('运营商只来自 Boce 拨测点;ICMP 叫丢包,主控 TCP 叫失败率,Boce 叫 TCPing 丢包', async ({ page }) => {
  await page.goto('/dashboard')
  await expect(page.getByTestId('asset-overview')).toBeVisible()
  // 主控与节点本机的行、概览都不冒充三网:运营商只出现在 Boce 拨测点那几行(接口原样给的地区与运营商)。
  for (const isp of ['电信', '联通', '移动']) {
    await expect(page.getByTestId('asset-overview')).not.toContainText(isp)
    for (const row of await page.locator('[data-source="MASTER"], [data-source="NODE"]').all()) {
      await expect(row).not.toContainText(isp)
    }
  }
  for (const row of await page.locator('[data-direction="INGRESS"][data-source="MASTER"]').all()) {
    await expect(row).not.toContainText('丢包')
  }
  for (const row of await page.locator('[data-source="BOCE"]').all()) {
    await expect(row).toContainText('TCPing 丢包')
  }
  await expect(page.getByTestId('probe-row-12')).not.toContainText('丢包')
})

test('头部两行:第一行状态带开机时长,第二行是异常标记与自定义标签', async ({ page }) => {
  await page.goto('/dashboard')
  // 上海有实时读数(开机 13 天),东京退回资源采样(开机 49 天,采样没过期)。
  await expect(card(page, 'NODE-1').getByTestId('card-state')).toHaveText('在线 13 天')
  await expect(card(page, 'NODE-2').getByTestId('card-state')).toHaveText('在线 49 天')
  await expect(card(page, 'NODE-1')).not.toContainText('运行中')
  // 上海:实时网速(16 秒前);东京:资源采样里的网速。
  await expect(card(page, 'NODE-1').getByTestId('card-rates')).toContainText('1.23 KB/s')
  await expect(card(page, 'NODE-1').getByTestId('card-rates')).toContainText('640 B/s')
  await expect(card(page, 'NODE-1').getByTestId('card-rates')).toHaveAttribute('title', /最近 30 秒的平均网速/)
  await expect(card(page, 'NODE-2').getByTestId('card-rates')).toHaveAttribute('title', /资源采样里的网速/)
  // 只有自定义标签,没有「落地节点」「自建」;没有标签也没有异常的卡片不留那一行。
  await expect(card(page, 'NODE-1').getByTestId('card-chips')).toContainText('CN2')
  await expect(card(page, 'NODE-1').getByTestId('card-chips')).toContainText('生产')
})

test('标签一行放不下就截掉,悬停浮出全部,卡片不变高', async ({ page, api }) => {
  api.patch<{ labels: { kind: string; object_id: number; tags: string[] }[] }>('GET /api/dashboard/cards', (v) => {
    v.labels.find((x) => x.kind === 'NODE' && x.object_id === 1)!.tags = [
      'CN2 GIA', '三网精品', '电联 CN2', '移动 CMI', '原生 IP', '流媒体解锁', '大带宽', '备用线路',
    ]
  })
  await page.goto('/dashboard')
  const a = card(page, 'NODE-1')
  const chips = a.getByTestId('card-chips')
  await expect(chips).toHaveClass(/ac__chips--clipped/)
  await expect(chips).toHaveAttribute('title', /备用线路/)
  const before = await a.boundingBox()
  const rowBefore = await chips.locator('.ac__chips-row').boundingBox()
  await chips.hover()
  const rowAfter = await chips.locator('.ac__chips-row').boundingBox()
  expect(rowAfter!.height).toBeGreaterThan(rowBefore!.height)
  await expect(chips.getByText('备用线路')).toBeVisible()
  const after = await a.boundingBox()
  expect(Math.round(after!.height)).toBe(Math.round(before!.height))
})

test('实时网速只读主控内存:打开卡片视图请求一次,切到原有视图不再轮询', async ({ page, api }) => {
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toBeVisible()
  expect(api.requests('GET', '/api/metrics/net-rates').length).toBeGreaterThanOrEqual(1)
  await page.getByTestId('dashboard-view-toggle').getByRole('tab', { name: '原有视图' }).click()
  const n = api.requests('GET', '/api/metrics/net-rates').length
  await page.waitForTimeout(300)
  expect(api.requests('GET', '/api/metrics/net-rates')).toHaveLength(n)
})

test('卡片不按卡片逐个请求:聚合接口一次,不碰任何单节点接口', async ({ page, api }) => {
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toBeVisible()
  expect(api.requests('GET', '/api/dashboard/cards')).toHaveLength(1)
  expect(api.requests('GET', /^\/api\/nodes\/\d+/)).toHaveLength(0)
  expect(api.requests('GET', /^\/api\/probe-targets\//)).toHaveLength(0)
  expect(api.mutations()).toHaveLength(0)
})

test('概览:成本只算自建节点、按币种分列,不相加;7 天内到期', async ({ page, api }) => {
  // 外部代理那 10.00 CNY 只在 cost_summary 里(留给别处用);概览读只算节点的 node_cost_summary。
  api.patch('GET /api/dashboard/cards', (d: { node_cost_summary: unknown[] }) => {
    d.node_cost_summary.push({
      currency: 'CNY', monthly_cost_minor: 2000, monthly_cost: '20.00', recurring_count: 1,
      unamortized_minor: 0, unamortized: '0.00', unamortized_count: 0, other_count: 0,
    })
  })
  await page.goto('/dashboard')
  const ov = page.getByTestId('asset-overview')
  await expect(ov).toContainText('5.00 USD')
  await expect(ov).toContainText('20.00 CNY')
  await expect(ov).not.toContainText('10.00 CNY')
  await expect(ov).not.toContainText('25.00')
  await expect(ov).toContainText('0 / 5 失败')
  await expect(ov).toContainText('出站 3 · 入口 2')
})

test('筛选与排序:地区、状态、搜索', async ({ page }) => {
  await page.goto('/dashboard')
  await expect(page.locator('.ac')).toHaveCount(2)
  await page.getByLabel('按名称搜索').fill('东京')
  await expect(page.locator('.ac')).toHaveCount(1)
  await expect(card(page, 'NODE-2')).toBeVisible()
  await page.getByRole('button', { name: '清除筛选' }).click()
  await expect(page.locator('.ac')).toHaveCount(2)
  // 状态:网络检测失败 —— 固定数据里没有,显示「没有符合筛选条件的卡片」,不是「还没有任何资产」。
  await page.getByLabel('状态').click()
  await page.locator('.ant-select-item-option', { hasText: '网络检测失败' }).click()
  await expect(page.getByText('没有符合筛选条件的卡片')).toBeVisible()
})

const cardOrder = (page: import('@playwright/test').Page) =>
  page.locator('[data-testid^="asset-card-"]').evaluateAll((els) => els.map((e) => e.getAttribute('data-testid')))

test('拖动排序:按住把手拖到别的卡片前面,顺序存在本机,刷新后还在;可以恢复默认', async ({ page, api }) => {
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-2')).toBeVisible()
  expect(await cardOrder(page)).toEqual(['asset-card-NODE-1', 'asset-card-NODE-2'])
  const grip = card(page, 'NODE-2').getByTestId('card-grip')
  const from = (await grip.boundingBox())!
  const to = (await card(page, 'NODE-1').boundingBox())!
  await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2)
  await page.mouse.down()
  // 拖到上海那张的左半边:放到它前面。
  await page.mouse.move(to.x + 30, to.y + 60, { steps: 8 })
  await page.mouse.up()
  await expect.poll(() => cardOrder(page)).toEqual(['asset-card-NODE-2', 'asset-card-NODE-1'])
  // 只存在本机:没有任何写请求。
  expect(api.mutations()).toHaveLength(0)
  expect(await page.evaluate(() => localStorage.getItem('lb.dashboard.cardOrder'))).toBe('["NODE-2","NODE-1"]')
  await page.reload()
  await expect.poll(() => cardOrder(page)).toEqual(['asset-card-NODE-2', 'asset-card-NODE-1'])
  await page.getByTestId('card-order-reset').click()
  await expect.poll(() => cardOrder(page)).toEqual(['asset-card-NODE-1', 'asset-card-NODE-2'])
})

test('拖动排序也能用键盘:聚焦把手,方向键前后挪;换成别的排序方式时没有把手', async ({ page }) => {
  await page.goto('/dashboard')
  await card(page, 'NODE-1').getByTestId('card-grip').focus()
  await page.keyboard.press('ArrowRight')
  await expect.poll(() => cardOrder(page)).toEqual(['asset-card-NODE-2', 'asset-card-NODE-1'])
  await expect(card(page, 'NODE-1').getByTestId('card-grip')).toBeFocused()
  await page.getByLabel('排序', { exact: true }).click()
  await page.locator('.ant-select-item-option', { hasText: '按到期时间' }).click()
  await expect(page.getByTestId('card-grip')).toHaveCount(0)
})

test('自动刷新:默认每分钟只重读库里的数据(节点、采样、探测汇总),不碰单节点接口;选项存在本机', async ({ page, api }) => {
  await page.clock.install({ time: new Date('2026-10-17T11:46:21.737Z') })
  await page.goto('/dashboard')
  await expect(card(page, 'NODE-1')).toBeVisible()
  await expect(page.getByTestId('dashboard-auto-refresh')).toContainText('每 1 分钟刷新')
  expect(api.requests('GET', '/api/dashboard/cards')).toHaveLength(1)
  await page.clock.runFor(61_000)
  await expect.poll(() => api.requests('GET', '/api/dashboard/cards').length).toBe(2)
  expect(api.requests('GET', '/api/metrics/nodes-latest').length).toBeGreaterThanOrEqual(2)
  // 部署记录与流量趋势不跟着刷;也不碰任何单节点接口,不触发任何写操作(探测、采集)。
  // 仪表盘读 100 条(侧栏另读 50 条做计数,与这里无关)。
  expect(api.requests('GET', /^\/api\/deployments\?limit=100$/)).toHaveLength(1)
  expect(api.requests('GET', /^\/api\/nodes\/\d+/)).toHaveLength(0)
  expect(api.mutations()).toHaveLength(0)

  await page.getByTestId('dashboard-auto-refresh').click()
  await page.locator('.ant-select-item-option', { hasText: '不自动刷新' }).click()
  expect(await page.evaluate(() => localStorage.getItem('lb.dashboard.autoRefresh'))).toBe('0')
  await page.clock.runFor(120_000)
  expect(api.requests('GET', '/api/dashboard/cards')).toHaveLength(2)
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
