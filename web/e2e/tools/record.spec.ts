import { test } from '@playwright/test'
import { writeFileSync } from 'node:fs'
import { fixtureKey } from '../support/key'

/**
 * 录制固定数据:对着本机 scratch 面板把每一页打开一遍,把页面读到的 GET 响应原样存下来。
 * 只在 E2E_RECORD=1 时运行,步骤见 e2e/README.md。
 *
 * **录制只读。** 这里拦下页面发出的一切写操作(直接中止,不放行到后端),只打开页面、
 * 切 Tab、展开抽屉。scratch 面板读节点服务现状时会经 SSH 去问测试机,那是只读探测。
 * 录出来的原始数据含真实地址与凭据,必须经 sanitize.mjs 处理后才能进仓库。
 */
const OUT = process.env.E2E_RECORD_OUT ?? 'e2e/fixtures/raw.json'

test('录制固定数据', async ({ page, context, baseURL }) => {
  test.setTimeout(15 * 60_000)
  const admin = process.env.E2E_ADMIN_COOKIE
  const portal = process.env.E2E_PORTAL_COOKIE
  if (!admin || !portal) throw new Error('需要 E2E_ADMIN_COOKIE 与 E2E_PORTAL_COOKIE')
  const host = new URL(baseURL!).hostname
  await context.addCookies([
    { name: 'litebox_session', value: admin, domain: host, path: '/', httpOnly: true },
    { name: 'litebox_portal_session', value: portal, domain: host, path: '/', httpOnly: true },
  ])

  const responses: Record<string, unknown> = {}
  const failed: string[] = []
  await page.route('**/*', (route) => {
    const req = route.request()
    const url = new URL(req.url())
    if (url.hostname !== host) return route.abort('blockedbyclient')
    if (url.pathname.startsWith('/api/') && req.method() !== 'GET') return route.abort('blockedbyclient')
    return route.continue()
  })
  page.on('response', async (resp) => {
    const req = resp.request()
    const url = new URL(req.url())
    if (!url.pathname.startsWith('/api/') || req.method() !== 'GET') return
    const key = fixtureKey('GET', url)
    if (!resp.ok()) {
      failed.push(`${key} → ${resp.status()}`)
      return
    }
    try {
      responses[key] = await resp.json()
    } catch {
      failed.push(`${key} → 非 JSON`)
    }
  })

  // 流量 Tab 每 2 秒轮询一次,节点服务现状要等 SSH —— networkidle 不一定等得到,封个顶。
  const settle = async () => {
    await page.waitForLoadState('networkidle', { timeout: 20_000 }).catch(() => {})
    await page.waitForTimeout(1500)
  }
  const visit = async (path: string) => {
    await page.goto(path)
    await settle()
  }

  const capturedAt = new Date().toISOString()

  for (const p of [
    '/dashboard',
    '/users',
    '/nodes',
    '/inbounds',
    '/external-proxies',
    '/subscription-profiles',
    '/deployments',
    '/audit-logs',
    '/settings',
  ]) {
    await visit(p)
  }

  // 用户详情抽屉:点第一行的名字。
  await visit('/users')
  const firstUser = page.locator('.ant-table-row').first()
  if (await firstUser.count()) {
    await firstUser.click()
    await settle()
  }

  // 每台节点的五个 Tab。
  const nodes = (await (await page.request.get('/api/nodes')).json()).items as { id: number }[]
  for (const n of nodes) {
    for (const tab of ['overview', 'entries', 'traffic', 'metrics', 'deploys']) {
      await visit(`/nodes/${n.id}/${tab}`)
    }
  }

  // 门户。
  for (const p of ['/user/dashboard', '/user/subscription', '/user/nodes', '/user/security', '/user/traffic']) {
    await visit(p)
  }
  // 我的流量的 7 天档(上面最后一页就是它)。
  await page.getByRole('tab', { name: '最近 7 天' }).click({ timeout: 10_000 })
  await settle()

  writeFileSync(OUT, JSON.stringify({ capturedAt, responses, failed }, null, 2) + '\n')
  console.log(`录到 ${Object.keys(responses).length} 个接口,${failed.length} 个失败`)
  for (const f of failed) console.log('  失败:', f)
})
