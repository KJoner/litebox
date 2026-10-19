import { test, expect, adminOnly } from './support/mock'

/**
 * Boce 外部入站拨测(付费)与探针卡片展示设置。
 *
 * 守住的几条:页面访问只读库,不调任何会花钱的接口;手动检测要先确认、带幂等请求 ID;
 * Key 只看得到末 4 位、请求体里不回传;启用 / 提高预算先说清楚后果;IPv6 入口选不了并写明原因;
 * 拨测点异常不叫失败;改卡片展示只写偏好,不碰 Boce;批量应用每台各建一个任务。
 */

test.beforeEach(({ api }) => {
  adminOnly(api)
})

const ESTIMATE = {
  points_per_round: 2, rounds_today: 16, points_today: 32, month: '2026-10', days_in_month: 31, rounds_month: 496,
  points_month: 992, peak_day_points: 32, daily_avg_points: 32, points_30d: 960, yuan_today: 0.064, yuan_month: 1.984,
  others_month_points: 1488, others_daily_avg: 48, total_month_points: 2480, total_daily_avg: 80, total_yuan_month: 4.96,
  balance_points: 9200, days_runnable: 115, point_price: 0.002, daily_limit: 200, monthly_limit: 4000, run_limit: 10,
  over_run_limit: false, over_daily: false, over_monthly: false,
}

test('系统设置:Key 只显示末 4 位;用量与账户余额分开;启用前先确认,请求体不带 Key', async ({ page, api }) => {
  api.patch('GET /api/boce', (o) => {
    o.settings.enabled = false
    o.blocked = 'Boce 外部拨测没有启用(系统设置 → Boce 外部拨测)'
  })
  api.on('PUT', '/api/boce/settings', (req) => ({ json: { ...(req.body as object), key_configured: true, key_hint: '…a1b2' } }))
  await page.goto('/settings#set-boce')
  const box = page.getByTestId('boce-settings')
  await expect(box).toContainText('未启用')
  await expect(box).toContainText('Boce 外部拨测没有启用')
  await expect(box.getByLabel('Boce API Key')).toHaveAttribute('placeholder', /…a1b2/)
  // 面板估算与 Boce 账户余额分开写。
  await expect(box).toContainText('本月估算消耗')
  await expect(box).toContainText('612 波点 / 4000 波点')
  await expect(box).toContainText('Boce 账户余额(真实数字)')
  await expect(box).toContainText('¥18.40 · 9200 波点')
  await expect(box).toContainText('已消失 1')
  // 全部任务总览。
  await expect(box).toContainText('上海-alpha 三网拨测')

  await box.getByRole('switch', { name: '启用 Boce 外部拨测' }).click()
  await box.getByTestId('boce-save').click()
  const confirm = page.getByRole('dialog').filter({ hasText: '启用 Boce 外部拨测?' })
  await expect(confirm).toContainText('1488 波点')
  await confirm.getByRole('button', { name: '确认保存' }).click()
  await expect.poll(() => api.requests('PUT', '/api/boce/settings').length).toBe(1)
  const body = api.requests('PUT', '/api/boce/settings')[0]!.body as Record<string, unknown>
  expect(body.enabled).toBe(true)
  expect(body.daily_points).toBe(200)
  expect('api_key' in body).toBe(false)
})

test('系统设置:校验 Key、同步拨测点走免费接口;只读打开不发任何写请求', async ({ page, api }) => {
  api
    .on('POST', '/api/boce/verify', { json: { balance: '18.40', point: 9200 } })
    .on('POST', '/api/boce/vantages/sync', {
      json: { results: [{ area: 'CN', total: 4, added: 0, missing: 1, error: '' }, { area: 'OVERSEA', total: 1, added: 0, missing: 0, error: '' }] },
    })
  await page.goto('/settings#set-boce')
  const box = page.getByTestId('boce-settings')
  await expect(box).toContainText('校验通过')
  expect(api.mutations()).toHaveLength(0)
  await box.getByTestId('boce-verify').click()
  await expect.poll(() => api.requests('POST', '/api/boce/verify').length).toBe(1)
  await box.getByTestId('boce-sync').click()
  await expect.poll(() => api.requests('POST', '/api/boce/vantages/sync').length).toBe(1)
  expect(api.requests('POST', /\/run$/)).toHaveLength(0)
})

test('网络监测 · Boce:任务与拨测点分行;拨测点异常不算失败;手动检测先确认、带请求 ID', async ({ page, api }) => {
  api.on('POST', '/api/boce/tasks/1/run', (req) => ({
    json: { id: 42, task_id: 1, node_id: 1, trigger: 'MANUAL', state: 'POLLING', state_text: '等待结果', points: 3,
      error: '', request_id: (req.body as { request_id: string }).request_id },
  }))
  await page.goto('/nodes/1/network')
  const sec = page.getByTestId('probe-boce')
  const task = sec.getByTestId('boce-task-1')
  await expect(task).toContainText('上海-alpha 三网拨测')
  await expect(task).toContainText('3 个拨测点')
  await expect(task).toContainText('每天 08:00–24:00 每 60 分钟')
  await expect(task).toContainText('本月已用 612 波点')
  await expect(sec.getByTestId('boce-series-21')).toContainText('上海 电信')
  await expect(sec.getByTestId('boce-series-21')).toContainText('24h TCPing 丢包 0%')
  const v = sec.getByTestId('boce-series-23')
  await expect(v).toContainText('拨测点异常')
  await expect(v).toContainText('不代表入口不可达')
  await expect(v.locator('[data-state="FAIL"]')).toHaveCount(0)
  await expect(v.locator('[data-state="VANTAGE"]')).toHaveCount(1)
  // 手动关着的海外任务:写明为什么不自动执行。
  await expect(sec.getByTestId('boce-task-2')).toContainText('自动检测没有开启')

  // 取消:一个写请求都不发。
  await task.getByRole('button', { name: '手动检测' }).click()
  const dlg = page.getByRole('dialog').filter({ hasText: '立即检测一次' })
  await expect(dlg).toContainText('3 个拨测点')
  await expect(dlg).toContainText('重复点击')
  await dlg.getByRole('button', { name: '取消' }).click()
  expect(api.mutations()).toHaveLength(0)

  await task.getByRole('button', { name: '手动检测' }).click()
  await page.getByRole('dialog').filter({ hasText: '立即检测一次' }).getByRole('button', { name: '检测', exact: true }).click()
  await expect.poll(() => api.requests('POST', '/api/boce/tasks/1/run').length).toBe(1)
  const rid = (api.requests('POST', '/api/boce/tasks/1/run')[0]!.body as { request_id: string }).request_id
  expect(rid).toMatch(/^[A-Za-z0-9_-]{8,64}$/)
})

test('网络监测 · 按来源筛选:只看 Boce 时出站与主控两块都收起来', async ({ page }) => {
  await page.goto('/nodes/1/network')
  await expect(page.getByTestId('probe-outbound')).toBeVisible()
  await page.getByTestId('probe-filter').getByLabel('来源').click()
  await page.locator('.ant-select-item-option', { hasText: 'Boce(入站)' }).click()
  await expect(page.getByTestId('probe-outbound')).toHaveCount(0)
  await expect(page.getByTestId('probe-ingress')).toHaveCount(0)
  await expect(page.getByTestId('probe-boce')).toBeVisible()
  await page.getByLabel('搜索测量目标').fill('联通')
  await expect(page.getByTestId('boce-series-22')).toBeVisible()
  await expect(page.getByTestId('boce-series-21')).toHaveCount(0)
})

test('新建任务:IPv6 入口选不了并写明原因;默认不开自动检测;估算按 POST 算', async ({ page, api }) => {
  api
    .on('POST', '/api/boce/estimate', { json: ESTIMATE })
    .on('POST', '/api/nodes/1/boce/tasks', (req) => ({ json: { id: 3, ...(req.body as object) } }))
  await page.goto('/nodes/1/network')
  await page.getByTestId('boce-task-new').click()
  const form = page.getByTestId('boce-task-form')
  await form.getByLabel('被测入口').click()
  const v6 = page.locator('.ant-select-item-option', { hasText: '2001:db8::11' })
  await expect(v6).toHaveClass(/ant-select-item-option-disabled/)
  await expect(v6).toContainText('没有说明支持 IPv6')
  await page.keyboard.press('Escape')

  const picker = form.getByTestId('boce-vantage-picker')
  // 已从列表消失的拨测点新任务不能选。
  await expect(picker.locator('label', { hasText: '浙江' }).getByRole('checkbox')).toBeDisabled()
  await picker.locator('label', { hasText: '上海' }).getByRole('checkbox').check()
  await picker.locator('label', { hasText: '广东' }).getByRole('checkbox').check()
  await expect(picker).toContainText('已选 2 个 · 每轮约 2 波点')
  await expect.poll(() => api.requests('POST', '/api/boce/estimate').length).toBeGreaterThan(0)
  await expect(form.getByTestId('boce-estimate')).toContainText('992 波点')
  await expect(form.getByTestId('boce-estimate')).toContainText('约可运行 115 天')

  await page.getByRole('dialog', { name: '新建 Boce 拨测任务' }).getByRole('button', { name: '保存' }).click()
  await expect.poll(() => api.requests('POST', '/api/nodes/1/boce/tasks').length).toBe(1)
  const body = api.requests('POST', '/api/nodes/1/boce/tasks')[0]!.body as Record<string, unknown>
  expect(body.ref).toBe('EP:1')
  expect(body.test_type).toBe('TCPING')
  expect(body.vantage_ids).toEqual([31, 32])
  expect(body.enabled).toBe(false)
  expect(body.window_start).toBe('08:00')
  expect(body.window_end).toBe('24:00')
  expect(api.requests('POST', /\/run$/)).toHaveLength(0)
})

test('新建 Ping 任务:入口只显示地址、请求带测法;编辑已有任务时测法锁定', async ({ page, api }) => {
  api
    .on('POST', '/api/boce/estimate', { json: ESTIMATE })
    .on('POST', '/api/nodes/1/boce/tasks', (req) => ({ json: { id: 3, ...(req.body as object) } }))
  await page.goto('/nodes/1/network')
  await page.getByTestId('boce-task-new').click()
  const form = page.getByTestId('boce-task-form')
  const type = form.getByTestId('boce-test-type')
  await type.getByText('Ping', { exact: true }).click()
  await expect(form).toContainText('ping 不通不等于入口不可用')
  // Ping 只测地址:候选里不再带端口。
  await form.getByLabel('被测入口').click()
  const opt = page.locator('.ant-select-item-option', { hasText: '203.0.113.11' }).first()
  await expect(opt).not.toContainText('203.0.113.11:')
  await page.keyboard.press('Escape')
  await form.getByTestId('boce-vantage-picker').locator('label', { hasText: '上海' }).getByRole('checkbox').check()
  await page.getByRole('dialog', { name: '新建 Boce 拨测任务' }).getByRole('button', { name: '保存' }).click()
  await expect.poll(() => api.requests('POST', '/api/nodes/1/boce/tasks').length).toBe(1)
  const body = api.requests('POST', '/api/nodes/1/boce/tasks')[0]!.body as Record<string, unknown>
  expect(body.test_type).toBe('PING')
  expect(api.requests('POST', /\/run$/)).toHaveLength(0)

  // 编辑已有任务:测法那一栏是锁着的(换测法要复制任务)。
  await expect(page.getByTestId('boce-task-form')).toBeHidden()
  const task = page.getByTestId('probe-boce').locator('.nbp__task').first()
  await expect(task.getByTestId('boce-task-type')).toHaveText('TCPing')
  await task.getByRole('button', { name: '编辑' }).click()
  await expect(page.getByTestId('boce-task-form').getByTestId('boce-test-type').getByRole('radio').first()).toBeDisabled()
  await expect(page.getByTestId('boce-task-form')).toContainText('创建后不能更换检测方式')
})

test('执行记录:未执行的一轮写明是预算拦下的,不当成失败', async ({ page }) => {
  await page.goto('/nodes/1/network')
  await page.getByTestId('boce-task-1').getByRole('button', { name: '执行记录' }).click()
  const runs = page.getByTestId('boce-runs')
  await expect(runs).toContainText('已完成')
  await expect(runs).toContainText('估算 3 波点')
  await expect(runs).toContainText('未执行')
  await expect(runs).toContainText('超过每日上限 200')
  await expect(runs).toContainText('拨测点异常')
})

test('卡片展示设置:入站 / 出站各最多 3 项,只写偏好,不碰 Boce', async ({ page, api }) => {
  api.on('PUT', '/api/nodes/1/probe-card', (req) => ({ json: { node_id: 1, updated_at: '', ...(req.body as object) } }))
  await page.goto('/nodes/1/network')
  await page.getByTestId('card-display-open').click()
  const dlg = page.getByTestId('card-display')
  const ingress = dlg.getByTestId('card-display-ingress')
  await expect(ingress).toContainText('(3/3)')
  await expect(ingress).toContainText('Boce TCPing · 上海 电信')
  await expect(ingress.getByLabel('添加入站(主控 / Boce)展示项')).toHaveClass(/ant-select-disabled/)
  await ingress.getByRole('button', { name: '不显示第 3 项' }).click()
  await ingress.getByRole('button', { name: '上移第 2 项' }).click()
  await expect(dlg.getByTestId('card-display-outbound')).toContainText('自动推荐')
  await page.getByTestId('card-display-save').click()
  await expect.poll(() => api.requests('PUT', '/api/nodes/1/probe-card').length).toBe(1)
  expect(api.requests('PUT', '/api/nodes/1/probe-card')[0]!.body).toEqual({
    ingress_mode: 'MANUAL', ingress_ids: [22, 21], outbound_mode: 'AUTO', outbound_ids: [],
  })
  // 改展示不调任何 Boce 接口。
  expect(api.mutations().map((m) => m.path)).toEqual(['/api/nodes/1/probe-card'])
})

test('节点列表:批量应用 Boce 任务模板,每台各建一个', async ({ page, api }) => {
  api
    .on('POST', '/api/boce/estimate', { json: { ...ESTIMATE, points_per_round: 1 } })
    .on('POST', '/api/boce/tasks/batch', {
      json: { results: [{ node_id: 1, task_id: 5, error: '' }, { node_id: 2, task_id: 6, error: '' }] },
    })
  await page.goto('/nodes')
  const rows = page.locator('.ant-table-tbody tr.ant-table-row')
  await rows.nth(0).getByRole('checkbox').check()
  await rows.nth(1).getByRole('checkbox').check()
  await page.getByTestId('nodes-boce-batch-open').click()
  const dlg = page.getByTestId('boce-batch')
  await dlg.getByTestId('boce-vantage-picker').locator('label', { hasText: '北京' }).getByRole('checkbox').check()
  await page.getByRole('dialog', { name: '批量应用 Boce 拨测任务' }).getByRole('button', { name: '应用' }).click()
  await expect.poll(() => api.requests('POST', '/api/boce/tasks/batch').length).toBe(1)
  const body = api.requests('POST', '/api/boce/tasks/batch')[0]!.body as {
    template: { vantage_ids: number[]; enabled: boolean }
    nodes: { node_id: number; ref: string }[]
  }
  expect(body.template.vantage_ids).toEqual([33])
  expect(body.template.enabled).toBe(false)
  expect(body.nodes).toEqual([{ node_id: 1, ref: 'EP:1' }, { node_id: 2, ref: 'EP:2' }])
  await expect(dlg).toContainText('已建任务 #5')
})
