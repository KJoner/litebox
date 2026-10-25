import { test, expect, portalOnly } from './support/mock'

/**
 * 用户门户。重点是 P0-3 的流量口径:概览的「额度已用」是额度周期内的用量,
 * 「我的节点」的「本月」是 UTC 自然月 —— 两个时间范围必须各自写清楚。
 */

test.describe('门户流量口径', () => {
  test('概览:叫「额度已用」并写出周期,不再叫「本月流量」', async ({ page, api }) => {
    portalOnly(api).patch('GET /api/portal/dashboard', (d: Record<string, unknown>) => {
      d.last_reset_at = '2026-10-05T00:00:00Z'
      d.next_reset_at = '2026-11-05T00:00:00Z'
    })
    await page.goto('/user/dashboard')
    await expect(page.locator('.pd__metric-label').first()).toContainText('额度已用')
    await expect(page.getByText('本月流量')).toHaveCount(0)
    await expect(page.getByTestId('quota-period')).toHaveText('10 月 5 日起 · 11 月 5 日重置')
  })

  test('概览:从未重置、不自动重置时写「开通起 · 不自动重置」', async ({ page, api }) => {
    portalOnly(api).patch('GET /api/portal/dashboard', (d: Record<string, unknown>) => {
      d.last_reset_at = null
      d.next_reset_at = null
    })
    await page.goto('/user/dashboard')
    await expect(page.getByTestId('quota-period')).toHaveText('开通起 · 不自动重置')
  })

  test('我的节点:说明今日 / 本月 / 累计各自的时间范围', async ({ page, api }) => {
    portalOnly(api)
    await page.goto('/user/nodes')
    const note = page.getByTestId('node-traffic-note')
    await expect(note).toContainText('UTC 自然月')
    await expect(note).toContainText('额度已用')
    await expect(page.getByText('本月 UTC').first()).toBeVisible()
  })

  test('我的流量:额度卡写出周期,按节点注明天数', async ({ page, api }) => {
    portalOnly(api)
    await page.goto('/user/traffic')
    await expect(page.getByTestId('quota-period')).toContainText('额度周期')
    await expect(page.getByText(/最近 30 天 · 按 UTC 日/)).toBeVisible()
    await page.getByRole('tab', { name: '最近 7 天' }).click()
    await expect(page.getByText(/最近 7 天 · 按 UTC 日/)).toBeVisible()
  })
})

test.describe('门户探针', () => {
  test('栏目里有「探针」;卡片只给名字、标签、资源百分比与那几个检测项', async ({ page, api }) => {
    portalOnly(api)
    await page.goto('/user/dashboard')
    await page.getByRole('navigation', { name: '栏目' }).getByRole('button', { name: /探针/ }).click()
    await expect(page).toHaveURL(/\/user\/probe$/)
    const cards = page.getByTestId('portal-probe-card')
    await expect(cards).toHaveCount(2)
    const a = cards.nth(0)
    await expect(a).toContainText('上海-alpha')
    await expect(a).toContainText('CN2')
    await expect(a).toContainText('在线 13 天')
    // 资源只有百分比:没有绝对用量,悬停也没有(进度条上没有 title)。
    await expect(a.locator('.lb-usage__value')).toHaveText(['12%', '49%', '31%'])
    await expect(a.locator('.lb-usage[title]')).toHaveCount(0)
    await expect(a).not.toContainText(/\d\s?(KB|MB|GB|TB)\b/)
    // 网络:入站、出站各自那几项;口径名照旧按 metric 起名。
    const items = a.getByTestId('portal-probe-item')
    await expect(items).toHaveCount(2)
    await expect(items.nth(0)).toContainText('上海 电信')
    await expect(items.nth(0)).toContainText('168 ms')
    await expect(items.nth(0)).toContainText('1h TCPing 丢包 0%')
    await expect(items.nth(1)).toContainText('超时')
    await expect(items.nth(1)).toContainText('连续失败')
    // 没有套餐、价格、到期,也没有网速与流量。
    for (const word of ['套餐', '到期', 'USD', 'CNY', '流量', 'KB/s']) await expect(a).not.toContainText(word)
    const b = cards.nth(1)
    await expect(b).toContainText('离线')
    await expect(b).toContainText('还没有资源数据')
    await expect(b).toContainText('这台机器暂时没有展示网络检测')
    expect(api.requests('GET', '/api/portal/probe')).toHaveLength(1)
    expect(api.requests('GET', /^\/api\/(?!portal\/)/)).toHaveLength(0)
  })

  test('读失败:说明原因并可重试,不画一张空卡片', async ({ page, api }) => {
    portalOnly(api).fail('GET', '/api/portal/probe', 500, '暂时读不到探针数据')
    await page.goto('/user/probe')
    await expect(page.getByText('暂时读不到探针数据')).toBeVisible()
    await expect(page.getByTestId('portal-probe-card')).toHaveCount(0)
  })
})

test.describe('门户页面', () => {
  for (const [path, heading] of [
    ['/user/subscription', '我的订阅'],
    ['/user/nodes', '我的节点'],
    ['/user/probe', '探针'],
    ['/user/traffic', '我的流量'],
    ['/user/security', '安全设置'],
  ] as const) {
    test(`${heading} 能打开`, async ({ page, api }) => {
      portalOnly(api)
      await page.goto(path)
      await expect(page.getByRole('heading', { level: 1 })).toContainText(heading)
    })
  }

  test('概览读失败:给出原因与重试,不显示一个假的 0', async ({ page, api }) => {
    portalOnly(api).fail('GET', '/api/portal/dashboard', 500, '暂时读不到你的账号信息')
    await page.goto('/user/dashboard')
    await expect(page.getByText('暂时读不到你的账号信息')).toBeVisible()
    await expect(page.getByRole('button', { name: /重试/ })).toBeVisible()
    await expect(page.getByText('0 B')).toHaveCount(0)
  })

  test('门户里不出现管理端信息', async ({ page }) => {
    await page.goto('/user/nodes')
    // 门户 DTO 是白名单,地址不在里面;这里只确认页面上没有任何 IP 样子的东西。
    await expect(page.locator('body')).not.toContainText(/\b\d{1,3}(\.\d{1,3}){3}\b/)
  })
})
