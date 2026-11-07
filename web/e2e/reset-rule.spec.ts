import { test, expect, adminOnly } from './support/mock'

/**
 * 复核 F01:改重置规则默认下一周期生效、保存之前给出后果预览;对齐日发当地日期;
 * 只改备注时不发重置规则(原样发回去会被当成「改回当前规则」,把排好的下一周期规则撤销掉)。
 */

async function openEdit(page: import('@playwright/test').Page, name: string) {
  await page.getByRole('button', { name: `${name} 的更多操作` }).first().click()
  await page.getByRole('menuitem', { name: '编辑', exact: true }).click()
  return page.getByRole('dialog').filter({ hasText: `编辑用户 · ${name}` })
}

test('只改备注:更新请求里没有重置规则', async ({ page, api }) => {
  adminOnly(api)
  const user = api.fixture<{ items: { id: number; display_name: string }[] }>('GET /api/users').items[0]
  api.on('PATCH', `/api/users/${user.id}`, { json: user })
  await page.goto('/users')
  const dialog = await openEdit(page, user.display_name)
  await dialog.getByPlaceholder('例如:2026 年 8 月起').fill('改了备注')
  await dialog.getByRole('button', { name: '保存' }).click()
  await expect.poll(() => api.mutations().length).toBe(1)
  const body = api.mutations()[0].body as Record<string, unknown>
  expect(body.remark).toBe('改了备注')
  expect(body).not.toHaveProperty('reset_cycle')
  expect(body).not.toHaveProperty('reset_anchor_at')
})

test('改成固定天数:预览后果,对齐日按当地日期发、默认下一周期生效', async ({ page, api }) => {
  adminOnly(api)
  const list = api.fixture<{ items: { id: number; display_name: string }[] }>('GET /api/users').items
  const user = { ...list[0], reset_cycle: 'MONTHLY', reset_day: 1 }
  api.on('GET', '/api/users', { json: { items: [user, ...list.slice(1)] } })
  const previews: Record<string, unknown>[] = []
  api.on('POST', `/api/users/${user.id}/reset-rule/preview`, (call) => {
    previews.push(call.body as Record<string, unknown>)
    return {
      json: {
        apply: 'NEXT',
        effective_at: '2026-12-01T00:00:00Z',
        next_resets: ['2026-12-10T05:00:00Z', '2027-01-09T05:00:00Z'],
        keeps_usage: true,
        note: '当前周期按原规则走到 2026-12-01T00:00:00Z,那一刻照常重置并换成新规则;之前的用量不受影响',
      },
    }
  })
  api.on('PATCH', `/api/users/${user.id}`, { json: user })
  await page.goto('/users')
  const dialog = await openEdit(page, user.display_name)
  await dialog.getByText('固定天数', { exact: true }).click()
  await dialog.getByPlaceholder('YYYY-MM-DD,留空 = 创建那天').fill('2026-11-10')
  await expect(dialog.getByText('当前周期按原规则走到')).toBeVisible()
  await expect(dialog.getByText('改规则本身不清零当前已用的流量')).toBeVisible()
  await expect(dialog.getByText('下一周期生效', { exact: true })).toBeVisible()
  const last = previews[previews.length - 1]
  expect(last.reset_anchor_at).toBe('2026-11-10')
  expect(last.reset_apply).toBe('NEXT')

  await dialog.getByRole('button', { name: '保存' }).click()
  await expect.poll(() => api.mutations().filter((m) => m.method === 'PATCH').length).toBe(1)
  const body = api.mutations().find((m) => m.method === 'PATCH')!.body as Record<string, unknown>
  expect(body.reset_cycle).toBe('DAYS')
  expect(body.reset_anchor_at).toBe('2026-11-10')
  expect(body.reset_apply).toBe('NEXT')
})
