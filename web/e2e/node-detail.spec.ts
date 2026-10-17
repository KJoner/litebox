import { test, expect, adminOnly, CAPTURED_AT } from './support/mock'

/**
 * 节点详情:五个 Tab、入口与服务卡片的操作菜单、需要确认的操作(取消什么都不发,
 * 确认只打到模拟层)、长操作的进度与失败结果。
 */

const NODE = 1
const TABS: [string, string][] = [
  ['概览', 'overview'],
  ['入口与转发', 'entries'],
  ['流量', 'traffic'],
  ['资源', 'metrics'],
  ['部署与记录', 'deploys'],
]

test.beforeEach(({ api }) => {
  adminOnly(api)
})

function deployResult(over: Record<string, unknown> = {}) {
  return {
    node_id: NODE,
    revision: 3,
    config_sha256: 'f'.repeat(64),
    status: 'SUCCESS',
    steps: [
      { name: '前置检查:SSH 连接与主机身份', status: 'SUCCESS', duration_ms: 120 },
      { name: '同步流量', status: 'SUCCESS', duration_ms: 340 },
      { name: '原子替换配置', status: 'SUCCESS', duration_ms: 80 },
      { name: '健康检查:VLESS 拨测(in-1)', status: 'SUCCESS', duration_ms: 1900, detail: 'HTTP 204' },
    ],
    started_at: CAPTURED_AT,
    finished_at: CAPTURED_AT,
    ...over,
  }
}

async function openEntries(page: import('@playwright/test').Page) {
  await page.goto(`/nodes/${NODE}/entries`)
  await expect(page.locator('[data-service="singbox"]')).toBeVisible()
}

test('五个 Tab 逐个可达,地址栏跟着变', async ({ page }) => {
  await page.goto(`/nodes/${NODE}`)
  await expect(page.getByRole('tab', { name: '概览' })).toHaveAttribute('aria-selected', 'true')
  // 从第二个起点,最后回到概览 —— 点已经选中的那个不会改地址。
  for (const [label, key] of [...TABS.slice(1), TABS[0]]) {
    await page.getByRole('tab', { name: label }).click()
    await expect(page).toHaveURL(new RegExp(`/nodes/${NODE}/${key}$`))
    await expect(page.getByRole('tab', { name: label })).toHaveAttribute('aria-selected', 'true')
  }
})

test('直接打开 /nodes/1/traffic 落在流量 Tab', async ({ page }) => {
  await page.goto(`/nodes/${NODE}/traffic`)
  await expect(page.getByRole('tab', { name: '流量' })).toHaveAttribute('aria-selected', 'true')
})

test('入口 Tab 的「新增入口」菜单列出四类入口', async ({ page }) => {
  await openEntries(page)
  // a-dropdown-button 的右半边是菜单触发器,左半边「新增入口」直接新建 sing-box 入口。
  await page.locator('.ant-dropdown-button').first().getByRole('button').nth(1).click()
  const menu = page.getByRole('menu')
  for (const t of ['sing-box 入口', 'Mieru 入口', 'nginx 转发入口', 'realm 转发入口']) {
    await expect(menu.getByRole('menuitem', { name: t })).toBeVisible()
  }
  await page.keyboard.press('Escape')
})

test('下发配置:取消时一个写请求都不发', async ({ page, api }) => {
  await openEntries(page)
  await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: /部署 rev \d+ 到/ })
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('健康检查不通过时自动回滚')
  await dialog.getByRole('button', { name: '取消' }).click()
  await expect(dialog).toBeHidden()
  expect(api.mutations()).toEqual([])
})

test('下发配置:确认后只打到模拟层,进度弹窗给出逐步结果,跑完才能关', async ({ page, api }) => {
  let release!: () => void
  const gate = new Promise<void>((r) => (release = r))
  api.on('POST', `/api/nodes/${NODE}/deploy`, async () => {
    await gate
    return { json: deployResult() }
  })
  await openEntries(page)
  await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
  await page.getByRole('dialog').filter({ hasText: /部署 rev/ }).getByRole('button', { name: '部署' }).click()

  const progress = page.getByRole('dialog').filter({ hasText: '下发 sing-box 配置' })
  await expect(progress).toContainText('正在这台机器上执行')
  // 跑的时候关不掉:没有 ×,ESC 也不管用。
  await expect(progress.getByRole('button', { name: 'Close' })).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(progress).toBeVisible()

  release()
  await expect(progress).toContainText('健康检查:VLESS 拨测(in-1)')
  await expect(progress).toContainText('HTTP 204')
  const posts = api.requests('POST', `/api/nodes/${NODE}/deploy`)
  expect(posts).toHaveLength(1)
  expect(posts[0].body).toEqual({ force: false })
  expect(api.mutations()).toHaveLength(1)

  await progress.getByRole('button', { name: 'Close' }).click()
  await expect(progress).toBeHidden()
})

test('下发失败并回滚:红色结论、已做完的步骤、错误原因都在', async ({ page, api }) => {
  const log = Array.from({ length: 120 }, (_, i) => `sing-box[${1000 + i}]: ERROR inbound/vless[in-1]: REALITY: failed to dial dest`).join('\n')
  api.on('POST', `/api/nodes/${NODE}/deploy`, {
    json: deployResult({
      status: 'ROLLED_BACK',
      error_message: '健康检查:VLESS 拨测(in-1)失败:经代理未取到 HTTP 响应: EOF',
      rollback_result: '已回滚到 rev 2,服务正常',
      steps: [
        { name: '前置检查:SSH 连接与主机身份', status: 'SUCCESS', duration_ms: 120 },
        { name: '原子替换配置', status: 'SUCCESS', duration_ms: 80 },
        { name: '健康检查:VLESS 拨测(in-1)', status: 'FAILED', duration_ms: 9000, detail: log },
        { name: '回滚', status: 'SUCCESS', duration_ms: 2100 },
      ],
    }),
  })
  await openEntries(page)
  await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
  await page.getByRole('dialog').filter({ hasText: /部署 rev/ }).getByRole('button', { name: '部署' }).click()

  const progress = page.getByRole('dialog').filter({ hasText: '下发 sing-box 配置' })
  await expect(progress).toContainText('经代理未取到 HTTP 响应')
  await expect(progress).toContainText('回滚')
  // 长日志在自己的框里滚,不把后面的「回滚」那一步推出屏幕。
  const detail = progress.locator('.nop__detail').first()
  const scrolls = await detail.evaluate((el) => el.scrollHeight > el.clientHeight + 4)
  expect(scrolls, '长日志应在自己的框里滚动').toBe(true)
  await expect(progress.getByText('回滚', { exact: true })).toBeInViewport()
  await expect(progress.getByRole('button', { name: 'Close' })).toBeInViewport()
})

test('连接中断(拿不到响应):提示操作很可能已在后台跑完,不催人重试', async ({ page, api }) => {
  await page.route(`**/api/nodes/${NODE}/deploy`, (route) => route.abort('connectionreset'))
  await openEntries(page)
  await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
  await page.getByRole('dialog').filter({ hasText: /部署 rev/ }).getByRole('button', { name: '部署' }).click()
  const progress = page.getByRole('dialog').filter({ hasText: '下发 sing-box 配置' })
  await expect(progress).toContainText('很可能已经跑完')
  // 这条请求被浏览器层中止,没有到模拟层;记账里也就没有它。
  void api
})

test('部署与记录 Tab:部署记录读失败给出重试,不显示「暂无数据」', async ({ page, api }) => {
  api.fail('GET', `/api/nodes/${NODE}/deployments`)
  await page.goto(`/nodes/${NODE}/deploys`)
  await expect(page.getByRole('button', { name: /重试/ }).first()).toBeVisible()
  await expect(page.getByText('暂无数据', { exact: true })).toHaveCount(0)
})
