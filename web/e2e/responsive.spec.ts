import { test, expect, adminOnly, portalOnly } from './support/mock'

/**
 * 响应式(UI 2.0.1 P1-5):768 宽下表格的操作够得着;手机宽度下页面不横向溢出;
 * 键盘能走到原来只有鼠标点得到的地方。
 */

test.describe('768 宽', () => {
  test.use({ viewport: { width: 768, height: 1024 } })

  for (const path of ['/nodes', '/users', '/inbounds', '/external-proxies']) {
    test(`${path}:每一行的操作都在可视范围里,不用横向滚`, async ({ page, api }) => {
      adminOnly(api)
      await page.goto(path)
      const rows = page.locator('.ant-table-tbody > tr.ant-table-row')
      await expect(rows.first()).toBeVisible()
      const n = await rows.count()
      for (let i = 0; i < n; i++) {
        const ops = rows.nth(i).locator('td').last()
        const box = await ops.boundingBox()
        expect(box, `${path} 第 ${i + 1} 行的操作格`).not.toBeNull()
        expect(box!.x + box!.width).toBeLessThanOrEqual(768)
        expect(box!.x).toBeGreaterThanOrEqual(64)
      }
    })
  }

  test('仪表盘的节点健康换成卡片,不再横向滚', async ({ page, api }) => {
    adminOnly(api)
    // 这一条说的是「原有视图」里的节点健康表;卡片视图本来就是卡片。
    await page.addInitScript(() => localStorage.setItem('lb.dashboard.view', 'classic'))
    await page.goto('/dashboard')
    await expect(page.locator('.dv__ncard').first()).toBeVisible()
    await expect(page.locator('.dv__nodes')).toHaveCount(0)
  })
})

test.describe('390 宽', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  const ADMIN = ['/dashboard', '/users', '/nodes', '/inbounds', '/external-proxies', '/deployments', '/audit-logs', '/settings', '/nodes/1/overview', '/nodes/1/entries', '/nodes/1/traffic', '/nodes/1/metrics', '/nodes/1/network', '/nodes/1/deploys']
  for (const path of ADMIN) {
    test(`${path} 页面不横向溢出`, async ({ page, api }) => {
      adminOnly(api)
      await page.goto(path)
      await page.waitForLoadState('networkidle')
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
      expect(overflow).toBeLessThanOrEqual(0)
    })
  }

  for (const path of ['/user/dashboard', '/user/subscription', '/user/nodes', '/user/traffic', '/user/security']) {
    test(`门户 ${path} 页面不横向溢出`, async ({ page, api }) => {
      portalOnly(api)
      await page.goto(path)
      await page.waitForLoadState('networkidle')
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
      expect(overflow).toBeLessThanOrEqual(0)
    })
  }
})

test.describe('键盘可达', () => {
  test('仪表盘「需要处理」清单的每一项都是能 Tab 到的链接', async ({ page, api }) => {
    adminOnly(api).on('GET', '/api/dashboard/alerts', {
      json: {
        items: [{ level: 'error', category: 'node', target: '上海-alpha', target_id: 1, message: '节点额度已用完' }],
      },
    })
    await page.goto('/dashboard')
    const item = page.getByRole('link', { name: /节点额度已用完/ })
    await expect(item).toHaveAttribute('href', '/nodes/1')
    await item.focus()
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/nodes\/1$/)
  })

  test('面包屑的上一级是真链接', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/nodes/1/overview')
    const crumb = page.getByRole('navigation', { name: '面包屑' }).getByRole('link', { name: '自建节点' })
    await expect(crumb).toHaveAttribute('href', '/nodes')
    await crumb.focus()
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/nodes$/)
  })

  test('页内动作是按钮:用户名打开详情抽屉', async ({ page, api }) => {
    adminOnly(api)
    const user = api.fixture<{ items: { display_name: string }[] }>('GET /api/users').items[0]
    // 抽屉要读的几个接口不在固定数据里,这里只确认它是能用键盘按的按钮。
    await page.goto('/users')
    const btn = page.getByRole('button', { name: user.display_name, exact: true }).first()
    await expect(btn).toBeVisible()
    await expect(btn).toHaveAttribute('type', 'button')
  })

  test('门户顶栏的品牌区是链接,不是只能用鼠标点的 div', async ({ page, api }) => {
    portalOnly(api)
    await page.goto('/user/nodes')
    await expect(page.getByRole('link', { name: /用户中心/ })).toHaveAttribute('href', '/user/dashboard')
  })
})
