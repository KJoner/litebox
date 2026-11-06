import { test, expect, adminOnly } from './support/mock'

/**
 * VNext S4:新增节点先分流(自建 / 外部)、可选项折叠;设置页搜索、外观(主题与动效)、未保存离开提示。
 */

test('新增节点:先选类型,外部节点去「外部代理」登记;自建主机的可选项默认折叠', async ({ page, api }) => {
  adminOnly(api)
  await page.goto('/nodes')
  await page.getByRole('button', { name: '添加节点' }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: '新增节点' })
  await expect(dialog.getByText('基础连接')).toBeVisible()
  await expect(dialog.getByText('同时创建的默认入口')).toBeVisible()
  // 展示名称、额度这些在折叠区里,默认看不到。
  await expect(dialog.getByText('展示名称', { exact: true })).toBeHidden()
  await dialog.getByRole('button', { name: /更多设置/ }).click()
  await expect(dialog.getByText('展示名称', { exact: true })).toBeVisible()

  await dialog.getByText('已有外部节点(分享链接 / 机场)').click()
  await expect(dialog.getByText('基础连接')).toBeHidden()
  await expect(dialog.getByRole('button', { name: '创建节点' })).toBeDisabled()
  await dialog.getByRole('button', { name: '去登记外部节点' }).click()
  await expect(page).toHaveURL(/\/external-proxies/)
  // 跳过来直接打开登记表单。
  await expect(page.getByRole('dialog')).toBeVisible()
  expect(api.mutations()).toHaveLength(0)
})

test('设置页:搜索只留相关的分组;外观里的动效档位写到 <html data-motion>', async ({ page, api }) => {
  adminOnly(api)
  await page.goto('/settings')
  await page.getByPlaceholder('搜索设置').fill('telegram')
  await expect(page.locator('#set-notify')).toBeVisible()
  await expect(page.locator('#set-sub')).toBeHidden()
  await page.getByPlaceholder('搜索设置').fill('不存在的设置项')
  await expect(page.getByText('没有和「不存在的设置项」相关的设置。')).toBeVisible()
  await page.getByPlaceholder('搜索设置').fill('')

  await page.locator('#set-appearance').getByText('关闭', { exact: true }).click()
  await expect(page.locator('html')).toHaveAttribute('data-motion', 'off')
  await page.locator('#set-appearance').getByText('浅色', { exact: true }).click()
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')
  expect(api.mutations()).toHaveLength(0)
})

test('设置页:有分组改了没保存时,离开页面要确认', async ({ page, api }) => {
  adminOnly(api)
  await page.goto('/settings')
  const input = page.locator('#set-sub input').first()
  await input.fill('https://changed.example.com')
  await expect(page.getByText('还没保存:订阅地址')).toBeVisible()
  await page.getByRole('button', { name: '审计日志' }).click()
  const confirm = page.getByRole('dialog').filter({ hasText: '有改动还没保存' })
  await expect(confirm).toBeVisible()
  await confirm.getByRole('button', { name: '留在这里' }).click()
  await expect(page).toHaveURL(/\/settings/)
  await page.getByRole('button', { name: '审计日志' }).click()
  await page.getByRole('dialog').filter({ hasText: '有改动还没保存' }).getByRole('button', { name: '放弃改动并离开' }).click()
  await expect(page).toHaveURL(/\/audit-logs/)
})

test('入口授权:入口管理的行菜单打开「仅指定用户」,保存前列出后果、发出的是同一张表的整体替换', async ({ page, api }) => {
  adminOnly(api)
  api.on('GET', '/api/inbound-access/singbox/1', { json: { restricted: false, users: [] } })
  api.on('PUT', '/api/inbound-access/singbox/1', (req) => ({
    json: { access: { restricted: true, users: [{ id: 1, user_code: 'user_000001', display_name: '测试用户A' }] }, node_id: 1, auto_deploy: true, echo: req.body },
  }))
  await page.goto('/inbounds')
  const row = page.getByRole('row').filter({ hasText: '上海-alpha' }).first()
  await row.getByRole('button', { name: '更多操作' }).click()
  await page.getByRole('menuitem', { name: '入口授权(仅指定用户)' }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: '入口授权' })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('switch').click()
  await dialog.locator('.ant-select').click()
  await page.getByText('测试用户A(user_000001)').click()
  await page.keyboard.press('Escape')
  await dialog.getByRole('button', { name: '保存' }).click()
  const confirm = page.getByRole('dialog').filter({ hasText: '保存「上海-alpha」的入口授权' })
  await expect(confirm.getByText('这个入口只对下面 1 个用户开放')).toBeVisible()
  await expect(confirm.getByText(/会自动重新下发/)).toBeVisible()
  await confirm.getByRole('button', { name: '保存' }).click()
  await expect.poll(() => api.requests('PUT', '/api/inbound-access/singbox/1').length).toBe(1)
  expect(api.requests('PUT', '/api/inbound-access/singbox/1')[0].body).toEqual({ restricted: true, user_ids: [1] })
})

test('排序模式:勾选后置顶,提交带旧值的整组;409 时提示并回退,不覆盖别人的改动', async ({ page, api }) => {
  adminOnly(api)
  let calls = 0
  api.on('POST', '/api/sort-order/batch', () => {
    calls++
    return calls === 1
      ? { json: { updated: 2 } }
      : { status: 409, json: { error: '有 1 项在你调整期间被改过或删掉了,这次一项都没保存;请刷新后重新调整', conflicts: [] } }
  })
  await page.goto('/nodes')
  await page.getByRole('button', { name: '调整顺序' }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: '调整机器顺序' })
  const rows = dialog.getByRole('list', { name: '排序列表' }).getByRole('listitem')
  await expect(rows).toHaveCount(2)
  await expect(rows.first()).toContainText('上海-alpha')
  await rows.nth(1).getByRole('checkbox').check()
  await dialog.getByRole('button', { name: '置顶' }).click()
  await expect(rows.first()).toContainText('东京-beta')
  await dialog.getByRole('button', { name: '保存顺序' }).click()
  await expect.poll(() => api.requests('POST', '/api/sort-order/batch').length).toBe(1)
  // 沿用原来占着的值 1、2,按新顺序重新分:东京 1、上海 2;旧值一并带上。
  expect(api.requests('POST', '/api/sort-order/batch')[0].body).toEqual({
    items: [
      { kind: 'NODE', id: 2, sort_order: 1, expected: 2 },
      { kind: 'NODE', id: 1, sort_order: 2, expected: 1 },
    ],
  })

  // 第二次:后端说冲突 —— 列表回到打开时的顺序,弹窗不关。
  await page.getByRole('button', { name: '调整顺序' }).click()
  await rows.nth(1).focus()
  await page.keyboard.press('Alt+ArrowUp')
  await expect(rows.first()).toContainText('东京-beta')
  await dialog.getByRole('button', { name: '保存顺序' }).click()
  await expect(page.getByText(/调整期间被改过/)).toBeVisible()
  await expect(rows.first()).toContainText('上海-alpha')
})

test('快速组链:预览按跳列出路径与改动,有问题时不能保存', async ({ page, api }) => {
  adminOnly(api)
  let problems: string[] = []
  api.on('POST', '/api/chains/quick-preview', () => ({
    json: {
      path: {
        hops: [
          { kind: 'NEW', id: 0, name: '经东京出', node_name: 'sh-node', protocol: 'SHADOWSOCKS', runs: '服务端:sh-node 上的 sing-box(将新建)', udp: '未实测', enabled: true, deployed: false },
          { kind: 'SINGBOX', id: 2, name: '东京-beta', node_name: 'tk-node', protocol: 'VLESS_REALITY', runs: '服务端:tk-node 上的 sing-box', udp: '未实测', enabled: true, deployed: true },
        ],
        egress: '从 tk-node 直连出网',
        dns: '目标域名由最终出口那一跳解析',
        loop: false,
        problems: [],
      },
      creates: ['在 sh-node 上新建入口「经东京出」(SHADOWSOCKS,监听 31000)'],
      modifies: ['落地 tk-node 的入口「东京-beta」写入一份链路凭据'],
      problems,
    },
  }))
  await page.goto('/inbounds')
  const row = page.getByRole('row').filter({ hasText: '上海-alpha' }).first()
  await row.getByRole('button', { name: '更多操作' }).click()
  await page.getByRole('menuitem', { name: /作为入口组链/ }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: '快速组链' })
  await dialog.getByPlaceholder('出现在订阅里的名字').fill('经东京出')
  await dialog.getByPlaceholder('如 31000').fill('31000')
  // 时钟固定时 AntD 的空值下拉框点一下不会展开,用方向键(见 e2e/README)。
  await dialog.locator('.ant-select').filter({ hasText: '选择落地入口' }).locator('input').focus()
  await page.keyboard.press('ArrowDown')
  await page.locator('.ant-select-item-option').filter({ hasText: '东京-beta / 东京-beta' }).click()
  const path = dialog.getByLabel('链路路径')
  await expect(path.getByText('第 2 跳')).toBeVisible()
  await expect(dialog.getByText('从 tk-node 直连出网')).toBeVisible()
  await expect(dialog.getByRole('button', { name: '只保存(不下发)' })).toBeEnabled()

  problems = ['链路成环:去向的下游又回到了这个入口']
  await dialog.getByPlaceholder('如 31000').fill('31001')
  await expect(dialog.getByText('这条链现在建不起来')).toBeVisible()
  await expect(dialog.getByRole('button', { name: '只保存(不下发)' })).toBeDisabled()
  expect(api.requests('POST', '/api/chains/quick')).toHaveLength(0)
})

test('能力矩阵:点开才读,✓ / ○ / ✕ 三种标记,不支持的悬停有原因', async ({ page, api }) => {
  adminOnly(api)
  api.on('GET', '/api/capabilities', {
    json: {
      columns: [{ key: 'selfhost', label: '自建入站' }, { key: 'surge', label: 'Surge' }, { key: 'stash', label: 'Stash' }],
      rows: [
        { key: 'TROJAN', label: 'Trojan + REALITY', kind: 'SELF', columns: {
          selfhost: { ok: true, verified: true }, surge: { ok: false, verified: false, note: 'Surge 不支持 REALITY' }, stash: { ok: false, verified: false, note: 'REALITY 在 Stash 上没有验证过' } } },
        { key: 'SS_LEGACY', label: 'Shadowsocks 传统 AEAD', kind: 'SELF', columns: {
          selfhost: { ok: true, verified: true }, surge: { ok: true, verified: false }, stash: { ok: true, verified: false } } },
      ],
      versions: { panel: 'dev', probe_protocol: 3, agent_protocol: 1 },
      components: [],
      manifest_loaded: false,
    },
  })
  await page.goto('/settings')
  await page.getByPlaceholder('搜索设置').fill('trojan')
  const sec = page.locator('#set-capability')
  await expect(sec).toBeVisible()
  expect(api.requests('GET', '/api/capabilities')).toHaveLength(0)
  await sec.getByRole('button', { name: '读取能力矩阵' }).click()
  const row = sec.getByRole('row').filter({ hasText: 'Trojan + REALITY' })
  await expect(row.getByRole('cell').nth(1)).toHaveAttribute('title', 'Surge 不支持 REALITY')
  await expect(row).toContainText('✓')
  await expect(sec.getByRole('row').filter({ hasText: '传统 AEAD' })).toContainText('○')
  await expect(sec.getByText('没有发布清单')).toBeVisible()
})
