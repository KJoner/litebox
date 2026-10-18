import { test, expect, adminOnly, type ApiMock } from './support/mock'

/**
 * 仪表盘的部分失败(UI 2.0.1 P0-1):任何一个来源读失败,结论都不能是「一切正常」;
 * 已经读到的严重项照常显示;「重试失败项」只重跑失败的那几项。
 */

/** 一个「什么都不用处理」的基线:没有预警、没有部署、没有到期。 */
function calm(api: ApiMock) {
  adminOnly(api)
  api
    .on('GET', '/api/dashboard/alerts', { json: { items: [] } })
    .on('GET', '/api/deployments', { json: { items: [] } })
    .on('GET', '/api/external-proxies', { json: { items: [] } })
    .on('GET', '/api/proxy-sources', { json: { items: [] } })
    .on('GET', '/api/traffic/status', { json: { failing_nodes: [], running: false } })
    .patch('GET /api/nodes', (d: { items: { expiry: unknown }[] }) => {
      for (const n of d.items) n.expiry = null
    })
    .patch('GET /api/nodes/health', (d: { items: Record<string, unknown>[] }) => {
      for (const h of d.items) Object.assign(h, { singbox: 'RUNNING', singbox_detail: 'started', recover_error: undefined })
    })
  return api
}

/** 指标卡、趋势图、节点健康表只在「原有视图」里;卡片视图是默认。 */
async function classicView(page: import('@playwright/test').Page) {
  await page.addInitScript(() => localStorage.setItem('lb.dashboard.view', 'classic'))
}

/** 固定数据里节点 1 的巡检结果,改一处状态。 */
function healthOf(api: ApiMock, patch: Record<string, unknown>) {
  api.patch('GET /api/nodes/health', (d: { items: Record<string, unknown>[] }) => {
    Object.assign(d.items.find((h) => h.node_id === 1)!, patch)
  })
}

const severe = {
  level: 'error',
  category: 'user',
  target: '测试用户A',
  target_id: 1,
  message: '流量已用完,账号已自动停用',
}

test('全部读到、没有待办:一切正常', async ({ page, api }) => {
  calm(api)
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).toContainText('一切正常')
  await expect(page.getByTestId('dashboard-partial-failure')).toHaveCount(0)
  await expect(page.getByText('没有需要处理的事', { exact: true })).toBeVisible()
})

for (const [label, path] of [
  ['部署记录', '/api/deployments'],
  ['代理源到期', '/api/proxy-sources'],
  ['外部代理到期', '/api/external-proxies'],
  ['预警', '/api/dashboard/alerts'],
  ['节点列表', '/api/nodes'],
] as const) {
  test(`${label}读失败:结论不说一切正常,提示里点名`, async ({ page, api }) => {
    calm(api).fail('GET', path)
    await page.goto('/dashboard')
    const headline = page.getByTestId('dashboard-headline')
    await expect(headline).toContainText('部分数据读取失败')
    await expect(headline).not.toContainText('一切正常')
    const notice = page.getByTestId('dashboard-partial-failure')
    await expect(notice).toContainText(label)
    await expect(notice).toContainText('清单与上方结论可能不完整')
    // 「需要处理」卡不能再画那个绿色的「没有需要处理的事」。
    await expect(page.getByText('没有需要处理的事', { exact: true })).toHaveCount(0)
  })
}

test('流量同步状态读失败也要说出来', async ({ page, api }) => {
  calm(api).fail('GET', '/api/traffic/status')
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).toContainText('无法确认一切正常')
  await expect(page.getByTestId('dashboard-partial-failure')).toContainText('流量同步状态')
})

test('已读到的严重项优先:别的来源失败时结论仍是「有 N 件事需要处理」', async ({ page, api }) => {
  calm(api).on('GET', '/api/dashboard/alerts', { json: { items: [severe] } }).fail('GET', '/api/proxy-sources')
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).toContainText('有 1 件事需要处理')
  await expect(page.getByText('流量已用完,账号已自动停用')).toBeVisible()
  await expect(page.getByTestId('dashboard-partial-failure')).toContainText('代理源到期')
})

test('重试只重跑失败的那几项,成功后提示消失', async ({ page, api }) => {
  calm(api).fail('GET', '/api/deployments')
  await page.goto('/dashboard')
  const notice = page.getByTestId('dashboard-partial-failure')
  await expect(notice).toBeVisible()

  const alertsBefore = api.requests('GET', '/api/dashboard/alerts').length
  const deploysBefore = api.requests('GET', '/api/deployments').length
  api.reset('GET', '/api/deployments').on('GET', '/api/deployments', { json: { items: [] } })
  await notice.getByRole('button', { name: '重试失败项' }).click()

  await expect(notice).toHaveCount(0)
  await expect(page.getByTestId('dashboard-headline')).toContainText('一切正常')
  expect(api.requests('GET', '/api/deployments').length).toBe(deploysBefore + 1)
  expect(api.requests('GET', '/api/dashboard/alerts').length).toBe(alertsBefore)
})

test('刷新时读失败保留上一次读到的严重项', async ({ page, api }) => {
  calm(api).on('GET', '/api/dashboard/alerts', { json: { items: [severe] } })
  await page.goto('/dashboard')
  await expect(page.getByText('流量已用完,账号已自动停用')).toBeVisible()

  api.fail('GET', '/api/dashboard/alerts')
  await page.getByRole('button', { name: '刷新' }).click()
  await expect(page.getByTestId('dashboard-partial-failure')).toContainText('预警')
  await expect(page.getByText('流量已用完,账号已自动停用')).toBeVisible()
  await expect(page.getByTestId('dashboard-headline')).toContainText('有 1 件事需要处理')
})

test('概览指标读失败显示「—」与读取失败,不是 0', async ({ page, api }) => {
  await classicView(page)
  calm(api).fail('GET', '/api/dashboard/summary')
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-partial-failure')).toContainText('概览指标')
  await expect(page.getByText('本月流量').locator('xpath=ancestor::*[contains(@class,"lb-metric")][1]')).not.toContainText('0 B')
})

test('巡检连不上:预警,不说一切正常', async ({ page, api }) => {
  calm(api)
  healthOf(api, { singbox: 'UNREACHABLE', singbox_detail: 'SSH 握手失败' })
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).toContainText('1 条预警')
  await expect(page.getByText(/巡检连不上这台机器/)).toBeVisible()
})

test('服务没在运行:严重,结论是「有 1 件事需要处理」', async ({ page, api }) => {
  calm(api)
  healthOf(api, { singbox: 'STOPPED', singbox_detail: 'inactive' })
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).toContainText('有 1 件事需要处理')
  await expect(page.getByText('sing-box 没在运行')).toBeVisible()
})

test('流量同步失败:进清单,不说一切正常', async ({ page, api }) => {
  calm(api).on('GET', '/api/traffic/status', {
    json: { failing_nodes: [{ node_id: 2, error: 'dial tcp: i/o timeout' }], running: false },
  })
  await page.goto('/dashboard')
  await expect(page.getByTestId('dashboard-headline')).not.toContainText('一切正常')
  await expect(page.getByText(/流量同步失败.*i\/o timeout/)).toBeVisible()
})
