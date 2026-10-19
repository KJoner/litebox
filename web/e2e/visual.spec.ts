import { test, expect, adminOnly, portalOnly } from './support/mock'
import type { Page } from '@playwright/test'

/**
 * 截图基线(@visual)。代表性页面 × 深浅主题 × 三个宽度。
 *
 * 基线按平台分开存(见 playwright.config.ts 的 snapshotPathTemplate):字体渲染在
 * Windows 与 Linux 上不一样。CI 只跑功能用例(--grep-invert @visual);改了界面之后在
 * 本机跑 `npm run test:e2e:update` 更新基线,并在提交里把新旧截图一起看一遍。
 */

const VIEWPORTS = [
  ['1440', 1440, 900],
  ['768', 768, 1024],
  ['390', 390, 844],
] as const

const ADMIN_PAGES: [string, string][] = [
  ['dashboard', '/dashboard'],
  ['dashboard-cards', '/dashboard'],
  ['nodes', '/nodes'],
  ['node-overview', '/nodes/1/overview'],
  ['node-entries', '/nodes/1/entries'],
  ['node-network', '/nodes/1/network'],
  ['probe-library', '/probe-library'],
]

const PORTAL_PAGES: [string, string][] = [
  ['portal-dashboard', '/user/dashboard'],
  ['portal-nodes', '/user/nodes'],
]

async function settle(page: Page) {
  await page.waitForLoadState('networkidle')
  await page.evaluate(() => document.fonts.ready)
  // 骨架、淡入动画都结束;截图本身再关掉 CSS 动画。
  await expect(page.locator('.ant-skeleton')).toHaveCount(0)
  await page.waitForTimeout(300)
}

for (const theme of ['light', 'dark'] as const) {
  for (const [vw, w, h] of VIEWPORTS) {
    // 深色只截桌面:深浅两套的差别在颜色,不在布局。
    if (theme === 'dark' && vw !== '1440') continue
    test.describe(`@visual ${theme} ${vw}`, () => {
      test.use({ viewport: { width: w, height: h }, colorScheme: theme })

      for (const [name, path] of ADMIN_PAGES) {
        test(`${name}`, async ({ page, api }) => {
          adminOnly(api)
          // 「dashboard」沿用原有视图的基线;卡片视图单独一组。
          const v = name === 'dashboard-cards' ? 'cards' : 'classic'
          await page.addInitScript((view) => localStorage.setItem('lb.dashboard.view', view), v)
          await page.goto(path)
          await settle(page)
          // 390 宽下节点详情的六个 Tab 放不下,AntD 有时把选中的那个滚进视野、有时不滚(取决于
          // 布局完成的先后),同一页每次截到的位置不一样。这一条只遮住 Tab 栏;Tab 栏本身由 768 /
          // 1440 两组基线与功能用例盯着。
          const mask = vw === '390' && name.startsWith('node-') ? [page.locator('.ant-tabs-nav')] : []
          await expect(page).toHaveScreenshot(`${name}-${theme}-${vw}.png`, { fullPage: true, mask })
        })
      }

      for (const [name, path] of PORTAL_PAGES) {
        test(`${name}`, async ({ page, api }) => {
          portalOnly(api)
          await page.goto(path)
          await settle(page)
          await expect(page).toHaveScreenshot(`${name}-${theme}-${vw}.png`, { fullPage: true })
        })
      }
    })
  }
}

test.describe('@visual 状态', () => {
  test('仪表盘部分失败', async ({ page, api }) => {
    adminOnly(api).fail('GET', '/api/deployments').fail('GET', '/api/proxy-sources')
    await page.addInitScript(() => localStorage.setItem('lb.dashboard.view', 'classic'))
    await page.goto('/dashboard')
    await settle(page)
    await expect(page).toHaveScreenshot('dashboard-partial-failure-light-1440.png')
  })

  test('部署失败的进度弹窗', async ({ page, api }) => {
    adminOnly(api).on('POST', '/api/nodes/1/deploy', {
      json: {
        node_id: 1,
        revision: 3,
        config_sha256: 'f'.repeat(64),
        status: 'ROLLED_BACK',
        error_message: '健康检查:VLESS 拨测(in-1)失败:经代理未取到 HTTP 响应: EOF',
        rollback_result: '已回滚到 rev 2,服务正常',
        steps: [
          { name: '前置检查:SSH 连接与主机身份', status: 'SUCCESS', duration_ms: 120 },
          { name: '原子替换配置', status: 'SUCCESS', duration_ms: 80 },
          {
            name: '健康检查:VLESS 拨测(in-1)',
            status: 'FAILED',
            duration_ms: 9000,
            detail: Array.from({ length: 40 }, (_, i) => `sing-box[${1000 + i}]: ERROR inbound/vless[in-1]: REALITY: failed to dial dest`).join('\n'),
          },
          { name: '回滚', status: 'SUCCESS', duration_ms: 2100 },
        ],
        started_at: '2026-10-17T11:46:21Z',
        finished_at: '2026-10-17T11:46:35Z',
      },
    })
    await page.goto('/nodes/1/entries')
    await settle(page)
    await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
    await page.getByRole('dialog').filter({ hasText: /部署 rev/ }).getByRole('button', { name: '部署', exact: true }).click()
    const progress = page.getByRole('dialog').filter({ hasText: '下发 sing-box 配置' })
    await expect(progress).toContainText('回滚')
    await page.waitForTimeout(400)
    await expect(page).toHaveScreenshot('deploy-failed-modal-light-1440.png')
  })

  // Boce 外部拨测:设置页那一节(浅 / 深)、新建任务弹窗(含费用估算)、卡片展示设置弹窗。
  for (const theme of ['light', 'dark'] as const) {
    test(`Boce 设置 ${theme}`, async ({ page, api }) => {
      await page.emulateMedia({ colorScheme: theme })
      adminOnly(api)
      await page.goto('/settings#set-boce')
      await settle(page)
      // 这一节比视口高:吸顶的顶栏与侧栏会盖进元素截图里。截图前把它们放回文档流。
      await page.addStyleTag({ content: '.ml__header, .ml__sider, .st__anchors { position: static !important; }' })
      await page.waitForTimeout(200)
      await expect(page.locator('#set-boce')).toHaveScreenshot(`boce-settings-${theme}-1440.png`)
    })
  }

  test('Boce 新建任务弹窗', async ({ page, api }) => {
    adminOnly(api).on('POST', '/api/boce/estimate', {
      json: {
        points_per_round: 2, rounds_today: 16, points_today: 32, month: '2026-10', days_in_month: 31, rounds_month: 496,
        points_month: 992, peak_day_points: 32, daily_avg_points: 32, points_30d: 960, yuan_today: 0.064,
        yuan_month: 1.984, others_month_points: 1488, others_daily_avg: 48, total_month_points: 2480,
        total_daily_avg: 80, total_yuan_month: 4.96, balance_points: 9200, days_runnable: 115, point_price: 0.002,
        daily_limit: 200, monthly_limit: 4000, run_limit: 10, over_run_limit: false, over_daily: false,
        over_monthly: false,
      },
    })
    // 弹窗比 900 高:把视口拉高,整个表单(名称、入口、拨测点、时间表、估算)一屏截下来。
    await page.setViewportSize({ width: 1440, height: 1500 })
    await page.goto('/nodes/1/network')
    await settle(page)
    await page.getByTestId('boce-task-new').click()
    const picker = page.getByTestId('boce-vantage-picker')
    await picker.locator('label', { hasText: '上海' }).getByRole('checkbox').check()
    await picker.locator('label', { hasText: '广东' }).getByRole('checkbox').check()
    await expect(page.getByTestId('boce-estimate')).toContainText('992 波点')
    await page.waitForTimeout(400)
    await expect(page.getByRole('dialog').filter({ hasText: '新建 Boce 拨测任务' })).toHaveScreenshot('boce-task-form-light-1440.png')
  })

  test('卡片展示设置弹窗', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/nodes/1/network')
    await settle(page)
    await page.getByTestId('card-display-open').click()
    await expect(page.getByTestId('card-display')).toBeVisible()
    await page.waitForTimeout(400)
    await expect(page.getByRole('dialog').filter({ hasText: '探针卡片展示设置' })).toHaveScreenshot('card-display-light-1440.png')
  })
})
