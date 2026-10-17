import { test, expect, adminOnly, CAPTURED_AT } from './support/mock'

/** 页面里的时钟固定在录制那一刻,测试里造时间也得从那一刻算。 */
const NOW = Date.parse(CAPTURED_AT)
const ago = (ms: number) => new Date(NOW - ms).toISOString()

/**
 * 管理端外壳:主题三档、侧栏导航与折叠、窄屏抽屉、侧栏底部的同步状态。
 */

test.describe('主题', () => {
  test('三档切换写到 <html data-theme>,刷新后保留', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/dashboard')
    const html = page.locator('html')
    await expect(html).toHaveAttribute('data-theme', 'light')

    await page.getByRole('button', { name: '切换主题' }).click()
    await page.getByRole('menuitem', { name: '深色' }).click()
    await expect(html).toHaveAttribute('data-theme', 'dark')

    await page.reload()
    await expect(html).toHaveAttribute('data-theme', 'dark')

    await page.getByRole('button', { name: '切换主题' }).click()
    await page.getByRole('menuitem', { name: '浅色' }).click()
    await expect(html).toHaveAttribute('data-theme', 'light')
  })

  test('「跟随系统」持续跟随系统的深浅变化', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/dashboard')
    await page.getByRole('button', { name: '切换主题' }).click()
    await page.getByRole('menuitem', { name: '跟随系统' }).click()
    const html = page.locator('html')
    await expect(html).toHaveAttribute('data-theme', 'light')
    await page.emulateMedia({ colorScheme: 'dark' })
    await expect(html).toHaveAttribute('data-theme', 'dark')
    await page.emulateMedia({ colorScheme: 'light' })
    await expect(html).toHaveAttribute('data-theme', 'light')
  })
})

test.describe('导航', () => {
  const PAGES: [string, RegExp, string][] = [
    ['用户管理', /\/users$/, '用户管理'],
    ['自建节点', /\/nodes$/, '自建节点'],
    ['入口管理', /\/inbounds$/, '入口管理'],
    ['外部代理', /\/external-proxies$/, '外部代理'],
    ['订阅配置', /\/subscription-profiles$/, '订阅配置'],
    ['部署记录', /\/deployments$/, '部署记录'],
    ['审计日志', /\/audit-logs$/, '审计日志'],
    ['系统设置', /\/settings$/, '设置'],
    ['仪表盘', /\/dashboard$/, '仪表盘'],
  ]

  test('侧栏逐项可达,当前项标 aria-current,面包屑跟着变', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/dashboard')
    const nav = page.getByRole('navigation', { name: '主导航' })
    for (const [label, url, crumb] of PAGES) {
      await nav.getByRole('button', { name: label, exact: true }).click()
      await expect(page).toHaveURL(url)
      await expect(nav.getByRole('button', { name: label, exact: true })).toHaveAttribute('aria-current', 'page')
      await expect(page.getByRole('navigation', { name: '面包屑' })).toContainText(crumb)
    }
  })

  test('侧栏折叠偏好记住,宽屏下也保持折叠', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/dashboard')
    await page.getByRole('button', { name: '收起侧栏' }).click()
    await expect(page.getByRole('button', { name: '展开侧栏' })).toBeVisible()
    await page.reload()
    await expect(page.getByRole('button', { name: '展开侧栏' })).toBeVisible()
  })

  test('窄于 1440 时默认折叠', async ({ page, api }) => {
    adminOnly(api)
    await page.setViewportSize({ width: 1280, height: 800 })
    await page.goto('/dashboard')
    await expect(page.getByRole('button', { name: '展开侧栏' })).toBeVisible()
  })

  test('手机宽度:导航收进抽屉,点一项后抽屉关闭', async ({ page, api }) => {
    adminOnly(api)
    await page.setViewportSize({ width: 390, height: 844 })
    await page.goto('/dashboard')
    await page.getByRole('button', { name: '打开导航' }).click()
    const drawer = page.locator('.ant-drawer-content')
    await expect(drawer).toBeVisible()
    // 抽屉里的项带计数徽标,名字是「自建节点 2」。
    await drawer.getByRole('button', { name: /^自建节点/ }).click()
    await expect(page).toHaveURL(/\/nodes$/)
    await expect(drawer).toBeHidden()
  })

  test('退出登录回到登录页', async ({ page, api }) => {
    adminOnly(api)
    api.on('POST', '/api/auth/logout', { status: 204 })
    await page.goto('/dashboard')
    // 退出之后 me 回 401,守卫才会放行登录页。
    api.on('GET', '/api/auth/me', { status: 401, json: { error: '未登录' } })
    await page.getByRole('button', { name: '账户菜单' }).click()
    await page.getByRole('menuitem', { name: '退出登录' }).click()
    await expect(page).toHaveURL(/\/login/)
    expect(api.requests('POST', '/api/auth/logout')).toHaveLength(1)
  })
})

test.describe('侧栏同步状态', () => {
  const status = (s: object) => ({ json: { failing_nodes: [], running: false, ...s } })

  test('首轮还在跑、已有失败:说「首轮同步仍在进行」,不说「尚未运行」', async ({ page, api }) => {
    adminOnly(api)
    api.on(
      'GET',
      '/api/traffic/status',
      status({
        running: true,
        failing_nodes: [{ node_id: 1, error: 'dial tcp: i/o timeout', failed_at: ago(30_000) }],
      }),
    )
    await page.goto('/dashboard')
    const box = page.getByTestId('sync-status')
    await expect(box).toContainText('1 个节点同步失败')
    await expect(box).toContainText('首轮同步仍在进行')
    await expect(box).not.toContainText('尚未运行')
  })

  test('从没跑完过一轮、也没有失败:不说「正常」', async ({ page, api }) => {
    adminOnly(api)
    api.on('GET', '/api/traffic/status', status({}))
    await page.goto('/dashboard')
    const box = page.getByTestId('sync-status')
    await expect(box).toContainText('等待首轮同步')
    await expect(box).not.toContainText('正常')
  })

  test('跑完过、没有失败:正常 + 上次完成时间', async ({ page, api }) => {
    adminOnly(api)
    api.on('GET', '/api/traffic/status', status({ last_run: ago(60_000) }))
    await page.goto('/dashboard')
    const box = page.getByTestId('sync-status')
    await expect(box).toContainText('流量同步正常')
    await expect(box).toContainText('上次完成')
  })

  test('超过 15 分钟没跑完一轮:不再说正常', async ({ page, api }) => {
    adminOnly(api)
    api.on('GET', '/api/traffic/status', status({ last_run: ago(40 * 60_000) }))
    await page.goto('/dashboard')
    await expect(page.getByTestId('sync-status')).toContainText('流量同步可能已停滞')
  })

  test('状态接口读失败:明说读取失败,而不是整格消失', async ({ page, api }) => {
    adminOnly(api)
    api.fail('GET', '/api/traffic/status')
    await page.goto('/dashboard')
    await expect(page.getByTestId('sync-status')).toContainText('同步状态读取失败')
  })
})
