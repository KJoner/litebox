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
