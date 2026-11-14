import { test, expect } from './support/mock'
import { CANDS, mockCanvas, PREVIEW } from './support/topologyData'

/**
 * v23.3 链路画布与统一的下一跳选择器。
 *
 * 数据全部是手写的虚构拓扑(文档地址段 RFC 5737 / 3849、example.com),不对应任何真实机器:
 *   A(VLESS,三条地址)──► R(nginx 转发)──► B(落地,VLESS)
 *   C(Shadowsocks)──────┘  共享同一个转发入口
 *   N(realm 转发)──► 指定地址 198.51.100.77:8443(后端未知)
 *   M(Mieru,本机直连)、X(外部代理)、D(停用、待确认旧地址 → B)
 *
 * 画布只读:打开、平移、缩放、筛选、搜索都不发任何写请求。
 */

test.beforeEach(({ api }) => mockCanvas(api))

test('全图:一个入口一张卡片,共享中继只有一张,三条地址都看得见,指定地址画成辅助节点', async ({ page, api }) => {
  await page.goto('/topology')
  await expect(page.getByTestId('card-SINGBOX:1')).toBeVisible()
  for (const key of ['SINGBOX:1', 'SINGBOX:3', 'NGINX:5', 'SINGBOX:2', 'REALM:6', 'MIERU:7', 'EXTERNAL:9', 'SINGBOX:4']) {
    await expect(page.getByTestId(`card-${key}`)).toHaveCount(1)
  }
  const a = page.getByTestId('card-SINGBOX:1')
  await expect(a).toContainText('192.0.2.10:443')
  await expect(a).toContainText('198.51.100.20:2443')
  await expect(a).toContainText('[2001:db8::20]:3443')
  await expect(page.getByTestId('card-NGINX:5')).toContainText('被 2 个入口引用')
  // Mieru 的端口段完整写出,不截成首个端口。
  await expect(page.getByTestId('card-MIERU:7')).toContainText('192.0.2.30:31000-31004')
  await expect(page.locator('.tv__aux')).toContainText('后端未知')
  await expect(page.getByTestId('topo-counts')).toContainText('全图 8 个入口')
  await expect(page.getByText('配置拓扑', { exact: true })).toBeVisible()
  expect(api.mutations()).toEqual([])
})

test('平移、缩放、筛选、搜索都不发写请求,也不连节点', async ({ page, api }) => {
  await page.goto('/topology')
  const vp = page.getByTestId('topo-viewport')
  await expect(page.getByTestId('card-SINGBOX:1')).toBeVisible()
  const box = (await vp.boundingBox())!
  await page.mouse.move(box.x + 20, box.y + box.height - 20)
  await page.mouse.down()
  await page.mouse.move(box.x + 120, box.y + box.height - 60, { steps: 5 })
  await page.mouse.up()
  await vp.dispatchEvent('wheel', { deltaY: -300, clientX: box.x + 200, clientY: box.y + 200 })
  await page.getByRole('button', { name: '适应全部' }).click()
  await page.getByTestId('topo-search').fill('198.51.100.20')
  await expect(page.locator('.tv__hits')).toContainText('香港入口A')
  expect(api.mutations()).toEqual([])
  // 只读了快照本身(以及侧栏这类与画布无关的),没有任何 SSH / 部署 / 拨测接口。
  const paths = api.calls.map((c) => c.path)
  expect(paths.some((p) => /deploy|probe|recheck|services/.test(p))).toBe(false)
})

test('按地址搜索能定位到那张卡片并选中它', async ({ page }) => {
  await page.goto('/topology')
  await page.getByTestId('topo-search').fill('2001:db8::20')
  await page.locator('.tv__hit').first().click()
  await expect(page.getByTestId('card-SINGBOX:1')).toHaveClass(/is-sel/)
})

test('筛选保留必要的中间节点,并写出匹配 / 关联保留 / 全图', async ({ page }) => {
  await page.goto('/topology')
  await expect(page.getByTestId('card-SINGBOX:1')).toBeVisible()
  await page.locator('.tv__filters .ant-select').first().click()
  await page.locator('.ant-select-item-option').filter({ hasText: 'nginx 转发' }).click()
  await page.keyboard.press('Escape')
  // 只匹配 R,但它的上游 A、C 与下游 B 都要留着,不能画一条并不存在的直连边。
  await expect(page.getByTestId('topo-counts')).toContainText('匹配 1')
  await expect(page.getByTestId('topo-counts')).toContainText('全图 8')
  for (const key of ['NGINX:5', 'SINGBOX:1', 'SINGBOX:3', 'SINGBOX:2']) {
    await expect(page.getByTestId(`card-${key}`)).toBeVisible()
  }
  await expect(page.getByTestId('card-MIERU:7')).toHaveCount(0)
})

test('卡片「⋯」菜单:外部代理不能设下一跳并写明原因;键盘与触屏不必依赖右键', async ({ page }) => {
  await page.goto('/topology')
  await page.getByTestId('card-menu-EXTERNAL:9').click()
  const menu = page.getByTestId('topo-menu')
  await expect(menu).toBeVisible()
  await expect(menu.getByTestId('menu-next')).toBeDisabled()
  await expect(menu.getByTestId('menu-next')).toContainText('外部代理是别人的机器')
  await expect(menu.getByTestId('menu-copy')).toBeEnabled()
  // 右键同一个菜单。
  await page.keyboard.press('Escape')
  await page.getByTestId('card-NGINX:5').click({ button: 'right' })
  await expect(menu.getByTestId('menu-edit')).toContainText('设置转发目标')
})

test('选中边:写出连接类型与所选地址,并在目标卡片里高亮那一行', async ({ page }) => {
  await page.goto('/topology')
  await expect(page.getByTestId('card-SINGBOX:2')).toBeVisible()
  await page.getByTestId('edge-e:NGINX:5').locator('.tv__hit-path').dispatchEvent('click')
  await expect(page.getByTestId('edge-label')).toContainText('透明转发')
  await expect(page.getByTestId('edge-label')).toContainText('192.0.2.30:443')
  await expect(page.getByTestId('card-SINGBOX:2').locator('.tc__ep.is-hl')).toContainText('192.0.2.30:443')
})

test('异常列表点一条就定位到对应入口;待确认的旧地址画成单独的样式', async ({ page }) => {
  await page.goto('/topology')
  await expect(page.getByTestId('edge-e:SINGBOX:4')).toHaveClass(/st-legacy/)
  await page.getByTestId('topo-problems').click()
  await page.getByRole('button', { name: /待确认旧地址/ }).click()
  await expect(page.getByTestId('card-SINGBOX:4')).toHaveClass(/is-sel/)
})

test('快照读不到时说读不到,不画成空图', async ({ page, api }) => {
  api.fail('GET', '/api/topology', 500, '数据库忙')
  await page.goto('/topology')
  await expect(page.getByRole('alert')).toContainText('读取链路拓扑失败')
  await expect(page.getByText('还没有任何入口')).toHaveCount(0)
})

test('出口选择器:多条地址必须选一条,预览写出实际连接与认证终点,提交带地址条目与修订号', async ({ page, api }) => {
  api.on('GET', /^\/api\/topology\/targets/, { json: CANDS })
  api.on('POST', '/api/topology/preview', { json: PREVIEW })
  api.on('POST', '/api/inbounds/1/chain', { json: { result: { target_deploy: null, host_deploy: null, stage: '完成', steps: [] } } })
  await page.goto('/nodes/1/entries')
  await page.getByRole('button', { name: '出口' }).first().click()
  const dialog = page.getByRole('dialog').filter({ hasText: '的下一跳' })
  await expect(dialog.getByTestId('cand-MIERU:7')).toHaveAttribute('aria-disabled', 'true')
  await expect(dialog.getByTestId('cand-MIERU:7')).toContainText('Mieru 入口不能当任何下一跳')
  await dialog.getByTestId('cand-SINGBOX:2').click()
  await expect(dialog.getByText('这个入口有多条地址,请选一条')).toBeVisible()
  await expect(dialog.getByRole('button', { name: '下一步' })).toBeDisabled()
  await dialog.getByTestId('ep-22').click()
  await expect(dialog.getByTestId('nexthop-preview')).toContainText('198.51.100.20:2443')
  await expect(dialog.getByTestId('nexthop-preview')).toContainText('新加坡落地 / 新加坡落地B')
  const previews = api.requests('POST', '/api/topology/preview')
  expect(previews.at(-1)?.body).toMatchObject({ from: 'SINGBOX:1', target_kind: 'INBOUND', target_id: 2, target_endpoint_id: 22 })
  await dialog.getByRole('button', { name: '下一步' }).click()
  await page.getByRole('button', { name: '保存并下发' }).click()
  await expect.poll(() => api.requests('POST', '/api/inbounds/1/chain').length).toBe(1)
  expect(api.requests('POST', '/api/inbounds/1/chain')[0].body).toMatchObject({
    target_kind: 'INBOUND',
    target_inbound_id: 2,
    target_endpoint_id: 22,
    revision: 'rev-0002',
  })
})

test('预览之后数据变了:提交被 409 拒绝,提示重新确认', async ({ page, api }) => {
  api.on('GET', /^\/api\/topology\/targets/, { json: CANDS })
  api.on('POST', '/api/topology/preview', { json: PREVIEW })
  api.on('POST', '/api/inbounds/1/chain', {
    status: 409,
    json: { error: '预览之后链路数据变过了', code: 'STALE_PREVIEW', revision: 'rev-0003' },
  })
  await page.goto('/nodes/1/entries')
  await page.getByRole('button', { name: '出口' }).first().click()
  const dialog = page.getByRole('dialog').filter({ hasText: '的下一跳' })
  await dialog.getByTestId('cand-SINGBOX:2').click()
  await dialog.getByTestId('ep-21').click()
  await expect(dialog.getByTestId('nexthop-preview')).toBeVisible()
  await dialog.getByRole('button', { name: '下一步' }).click()
  await page.getByRole('button', { name: '保存并下发' }).click()
  await expect(page.getByText('预览之后链路数据变过了,请重新确认')).toBeVisible()
})
