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

test.describe('门户页面', () => {
  for (const [path, heading] of [
    ['/user/subscription', '我的订阅'],
    ['/user/nodes', '我的节点'],
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
