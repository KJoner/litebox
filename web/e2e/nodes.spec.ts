import { test, expect, adminOnly } from './support/mock'

/**
 * 节点列表:渲染、搜索、行内操作菜单、键盘可达、失败态、768 宽下操作列可达。
 */

type NodeLite = { id: number; name: string; display_name: string; host: string }

test.beforeEach(({ api }) => {
  adminOnly(api)
})

test('列出全部节点,名字是能用键盘打开的真链接', async ({ page, api }) => {
  const nodes = api.fixture<{ items: NodeLite[] }>('GET /api/nodes').items
  await page.goto('/nodes')
  for (const n of nodes) {
    await expect(page.getByRole('link', { name: n.name, exact: true }).first()).toBeVisible()
  }
  const first = page.getByRole('link', { name: nodes[0].name, exact: true }).first()
  await expect(first).toHaveAttribute('href', `/nodes/${nodes[0].id}`)
  await first.focus()
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(new RegExp(`/nodes/${nodes[0].id}`))
})

test('搜索按内部名称、订阅名称、地址过滤', async ({ page, api }) => {
  const nodes = api.fixture<{ items: NodeLite[] }>('GET /api/nodes').items
  test.skip(nodes.length < 2, '固定数据里至少要两台节点')
  await page.goto('/nodes')
  await page.getByPlaceholder('内部名称 / 订阅名称 / 地址').fill(nodes[1].host)
  await expect(page.getByRole('link', { name: nodes[1].name, exact: true }).first()).toBeVisible()
  await expect(page.getByRole('link', { name: nodes[0].name, exact: true })).toHaveCount(0)
})

test('行内「更多操作」菜单能打开,菜单项可见', async ({ page, api }) => {
  const n = api.fixture<{ items: NodeLite[] }>('GET /api/nodes').items[0]
  await page.goto('/nodes')
  await page.getByRole('button', { name: `${n.name} 的更多操作` }).first().click()
  await expect(page.getByRole('menuitem', { name: '更多操作(详情页)' })).toBeVisible()
  await page.keyboard.press('Escape')
})

test('列表读失败:错误态 + 重试,不显示「暂无数据」', async ({ page, api }) => {
  api.fail('GET', '/api/nodes')
  await page.goto('/nodes')
  await expect(page.getByRole('button', { name: /重试/ }).first()).toBeVisible()
  await expect(page.getByText('暂无数据', { exact: true })).toHaveCount(0)
})

test('768 宽:每行的操作按钮都在可视范围里', async ({ page, api }) => {
  const nodes = api.fixture<{ items: NodeLite[] }>('GET /api/nodes').items
  await page.setViewportSize({ width: 768, height: 900 })
  await page.goto('/nodes')
  for (const n of nodes) {
    const btn = page.getByRole('button', { name: `${n.name} 的更多操作` }).first()
    await expect(btn).toBeVisible()
    const box = await btn.boundingBox()
    expect(box, `${n.name} 的操作按钮`).not.toBeNull()
    expect(box!.x + box!.width).toBeLessThanOrEqual(768)
  }
})
