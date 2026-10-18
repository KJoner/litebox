import { test, expect, adminOnly } from './support/mock'

/**
 * 公共探测目标库(V22.2):自定义 / Zstatic 预置 / TcpQuality 同步,第三方来源的授权确认与限额,
 * 节点详情里的目标选择器与本机覆盖,节点列表的批量应用。
 *
 * 守住的几条:第三方目标导入只是登记、不对任何节点生效;未确认授权时启用会被提醒;
 * 授权确认要先勾「已阅读」;上游的改动默认不勾、要逐条确认;握手近似叫「无应答」,不叫丢包。
 */

test.beforeEach(({ api }) => {
  adminOnly(api)
})

const previewItem = (key: string, name: string, status: string, extra: object = {}) => ({
  key, name, host: `${key.split(':')[1]}-${key.split(':')[2]}-v4.ip.zstaticcdn.com`, port: 80, family: 'V4',
  region: '华北', province: '北京', city: '', isp: 'CT', role: 'PRIMARY', level: 'PROVINCE', ip: '', status,
  local_ids: [], changes: [], suspicious: [], manual_kept: [], exists_as: '', missing_since: '', ...extra,
})

const preview = (source: string, items: object[], extra: object = {}) => ({
  source, snapshot_id: source === 'ZSTATIC' ? '' : 'snap-1', fetched_at: '2026-10-17T11:47:00Z',
  tcp_mode: 'CONNECT', host_mode: 'DOMAIN', items, missing: [], rejected: [], unsupported: {},
  counts: { NEW: items.filter((i) => (i as { status: string }).status === 'NEW').length }, bulk_warning: '', ...extra,
})

test('目标库:来源、方式、生效范围分列;第三方目标未授权时写明原因;按来源筛选', async ({ page }) => {
  await page.goto('/probe-library')
  await expect(page.getByRole('heading', { name: '探测目标库' })).toBeVisible()
  const table = page.getByTestId('probe-library-table')
  await expect(table).toContainText('公共 DNS(演示)')
  await expect(table).toContainText('北京电信')
  await expect(table).toContainText('TCP 握手(近似 SYN)')
  await expect(table).toContainText('还没有确认授权')
  await expect(page.getByTestId('probe-source-ZSTATIC')).toContainText('未确认授权')
  await expect(page.getByTestId('probe-source-TCPQUALITY')).toContainText('上次拉取失败')
  await expect(page.getByTestId('probe-source-TCPQUALITY')).toContainText('已导入的目标不受影响')
  await expect(page.getByTestId('probe-limits')).toContainText('第三方目标全站每分钟探测尝试上限')

  await page.getByLabel('来源').click()
  await page.locator('.ant-select-item-option', { hasText: 'Zstatic' }).click()
  await expect(table.locator('tbody tr.ant-table-row')).toHaveCount(1)
  await expect(table).toContainText('北京电信')
})

test('授权确认:先勾「已阅读」才能确认,请求里带说明;未授权的目标批量启用前就提醒', async ({ page, api }) => {
  api
    .on('PUT', '/api/probe/sources/ZSTATIC', { json: {} })
    .on('GET', '/api/probe/public-targets/3/nodes', { json: api.fixture('GET /api/probe/public-targets/2/nodes') })
  await page.goto('/probe-library')
  // 勾选北京电信 → 应用到节点:弹窗里先说清楚「这个来源还没确认授权」。
  const row = page.getByTestId('probe-library-table').locator('tr', { hasText: '北京电信' })
  await row.getByRole('checkbox').check()
  await page.getByRole('button', { name: '应用到节点…' }).click()
  await expect(page.getByTestId('probe-scope-modal')).toContainText('还没有确认授权')
  await page.getByRole('dialog', { name: /启用节点/ }).getByRole('button', { name: '取消' }).click()

  await page.getByTestId('probe-source-ZSTATIC').getByRole('button', { name: '阅读说明并确认' }).click()
  const dlg = page.getByRole('dialog', { name: /使用说明与授权/ })
  await expect(dlg).toContainText('仅供个人测试与研究参考')
  await expect(dlg).toContainText('不能替代来源方的实际授权')
  const confirm = dlg.getByRole('button', { name: '确认' })
  await expect(confirm).toBeDisabled()
  await dlg.getByRole('checkbox').check()
  await dlg.getByPlaceholder(/授权说明/).fill('已与来源方确认研究用途')
  await confirm.click()
  await expect.poll(() => api.requests('PUT', '/api/probe/sources/ZSTATIC').length).toBe(1)
  expect(api.requests('PUT', '/api/probe/sources/ZSTATIC')[0]!.body).toEqual({ authorize: true, authorized_note: '已与来源方确认研究用途' })
  expect(api.requests('POST', '/api/probe/public-targets/batch')).toHaveLength(0)
})

test('Zstatic 预置:默认勾常用三地,本地生成预览;导入只写目标库,不启用任何节点', async ({ page, api }) => {
  api
    .on('POST', '/api/probe/sources/zstatic/presets', {
      json: preview('ZSTATIC', [
        previewItem('prov:bj:ct:4', '北京电信', 'EXISTS', { exists_as: '「北京电信」(Zstatic CDN 节点目录)' }),
        previewItem('prov:bj:cu:4', '北京联通', 'NEW'),
        previewItem('prov:bj:cm:4', '北京移动', 'NEW'),
      ]),
    })
    .on('POST', '/api/probe/sources/apply', { json: { created: 2, updated: 0, missing: 0, restored: 0, skipped: [] } })
  await page.goto('/probe-library')
  await page.getByRole('button', { name: 'Zstatic 预置' }).click()
  const dlg = page.getByTestId('source-import-ZSTATIC')
  await expect(dlg).toContainText('导入只是登记,不会让任何节点开始探测')
  await dlg.getByRole('button', { name: '生成预览' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/sources/zstatic/presets').length).toBe(1)
  expect(api.requests('POST', '/api/probe/sources/zstatic/presets')[0]!.body).toEqual({
    provinces: ['BJ', 'SH', 'GD'], isps: ['CT', 'CU', 'CM'], families: ['V4'], tcp_mode: 'CONNECT',
  })
  await expect(dlg).toContainText('已存在')
  await page.getByRole('button', { name: '写入目标库' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/sources/apply').length).toBe(1)
  expect(api.requests('POST', '/api/probe/sources/apply')[0]!.body).toEqual({
    source: 'ZSTATIC', import: ['prov:bj:cu:4', 'prov:bj:cm:4'], update: [], mark_missing: false,
    tcp_mode: 'CONNECT', host_mode: 'DOMAIN',
  })
  expect(api.requests('POST', '/api/probe/bindings')).toHaveLength(0)
  expect(api.requests('POST', '/api/probe/public-targets/batch')).toHaveLength(0)
})

test('TcpQuality:上游不可用时说清楚目标库没动;上游改动默认不勾,可疑改动标出来', async ({ page, api }) => {
  api.fail('POST', '/api/probe/sources/tcpquality/fetch', 502, '上游返回 HTTP 503')
  await page.goto('/probe-library')
  await page.getByRole('button', { name: 'TcpQuality 同步' }).click()
  const dlg = page.getByTestId('source-import-TCPQUALITY')
  await expect(dlg).toContainText('不做定期同步')
  await dlg.getByRole('button', { name: '拉取并预览' }).click()
  await expect(dlg).toContainText('上游返回 HTTP 503')
  await expect(dlg).toContainText('目标库没有任何改动')

  api
    .on('POST', '/api/probe/sources/tcpquality/fetch', {
      json: preview(
        'TCPQUALITY',
        [
          previewItem('cdn:4:bj:ct', '北京电信', 'CHANGED', {
            changes: ['IP 198.51.100.1 → 192.0.2.9'], suspicious: ['IP 跳到了另一个网段'], manual_kept: ['name'], local_ids: [3],
          }),
          previewItem('cdn:4:bj:ct:backup', '北京电信(备用)', 'NEW', { role: 'BACKUP' }),
        ],
        { unsupported: { 教育网: 31, Telegram: 10 }, counts: { CHANGED: 1, NEW: 1 } },
      ),
    })
    .on('POST', '/api/probe/sources/apply', { json: { created: 0, updated: 1, missing: 0, restored: 0, skipped: [] } })
  await dlg.getByRole('button', { name: '拉取并预览' }).click()
  await expect(dlg).toContainText('可疑:IP 跳到了另一个网段')
  await expect(dlg).toContainText('手工改过的 name 不会覆盖')
  await expect(dlg).toContainText('首版不提供批量导入:教育网 31 条、Telegram 10 条')
  // 什么都没勾就提交:拦在页面上。
  await page.getByRole('button', { name: '写入目标库' }).click()
  await expect(dlg).toContainText('没有勾选任何要导入或接受的项')
  await dlg.locator('tr[data-key="cdn:4:bj:ct"]').getByRole('checkbox').check()
  await page.getByRole('button', { name: '写入目标库' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/sources/apply').length).toBe(1)
  const body = api.requests('POST', '/api/probe/sources/apply')[0]!.body as { import: string[]; update: string[]; snapshot_id: string }
  expect(body.update).toEqual(['cdn:4:bj:ct'])
  expect(body.import).toEqual([])
  expect(body.snapshot_id).toBe('snap-1')
})

test('网络监测 Tab · 出站:握手近似叫「无应答」;从目标库挑目标、暂停本机都只改这一台', async ({ page, api }) => {
  api
    .on('POST', '/api/probe/bindings', { json: { message: '已保存' } })
    .on('PUT', '/api/nodes/1/probe-settings', { json: { node_id: 1, paused: true, interval_sec: 0, updated_at: '' } })
  await page.goto('/nodes/1/network')
  const syn = page.getByTestId('probe-public-4')
  await expect(syn).toContainText('SYN 443')
  await expect(syn).toContainText('无应答')
  await expect(syn).not.toContainText('丢包')
  // 没有选中的第三方目标不在列表里(目标库可能有几百项)。
  await expect(page.getByTestId('probe-public-3')).toHaveCount(0)

  await page.getByTestId('probe-pick-open').click()
  const picker = page.getByTestId('node-target-picker')
  await expect(picker.getByTestId('pick-3')).toContainText('未授权')
  await picker.getByTestId('pick-3').getByRole('checkbox').check()
  await page.getByRole('button', { name: '应用到本机' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/bindings').length).toBe(1)
  expect(api.requests('POST', '/api/probe/bindings')[0]!.body).toEqual({ node_ids: [1], public_ids: [3], mode: 'ON' })

  await page.getByTestId('probe-node-settings').getByRole('switch').click()
  await expect.poll(() => api.requests('PUT', '/api/nodes/1/probe-settings').length).toBe(1)
  expect(api.requests('PUT', '/api/nodes/1/probe-settings')[0]!.body).toEqual({ paused: true, interval_sec: 0 })
  expect(api.requests('PUT', /^\/api\/probe\/public-targets/)).toHaveLength(0)
})

test('自建节点列表:勾选几台机器批量应用探测目标', async ({ page, api }) => {
  api.on('POST', '/api/probe/bindings', { json: { message: '已保存' } })
  await page.goto('/nodes')
  const rows = page.locator('.ant-table-tbody tr.ant-table-row')
  await rows.nth(0).getByRole('checkbox').check()
  await rows.nth(1).getByRole('checkbox').check()
  await page.getByTestId('nodes-probe-batch-open').click()
  const dlg = page.getByTestId('nodes-probe-batch')
  await dlg.getByTestId('pick-1').getByRole('checkbox').check()
  await page.getByRole('dialog', { name: /应用探测目标/ }).getByRole('button', { name: '应用' }).click()
  await expect.poll(() => api.requests('POST', '/api/probe/bindings').length).toBe(1)
  const body = api.requests('POST', '/api/probe/bindings')[0]!.body as { node_ids: number[]; public_ids: number[]; mode: string }
  expect(body.node_ids).toHaveLength(2)
  expect(body.public_ids).toEqual([1])
  expect(body.mode).toBe('ON')
})
