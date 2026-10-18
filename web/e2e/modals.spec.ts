import { test, expect, adminOnly, type ApiMock } from './support/mock'
import type { Page } from '@playwright/test'

/**
 * 操作弹窗(UI 2.0.1 P1-4):探测、配置比对、TCP 调优、重新引导、确认框。
 * 塞进去的都是长内容 —— 要看的是长内容下弹窗还能不能关、按钮会不会被挤出屏幕、
 * 正文是不是在弹窗里滚而不是把整页撑开。两个宽度各走一遍。
 */

const NODE = 1
const WIDTHS = [
  ['桌面', 1440, 900],
  ['手机', 390, 844],
] as const

test.beforeEach(({ api }) => {
  adminOnly(api)
})

const many = <T>(n: number, f: (i: number) => T): T[] => Array.from({ length: n }, (_, i) => f(i))

function mockProbe(api: ApiMock) {
  api.on('POST', `/api/nodes/${NODE}/probe`, {
    json: {
      arch: 'amd64',
      kernel: '6.1.0-26-amd64',
      os_name: 'Debian GNU/Linux 12 (bookworm)',
      mem_total_mb: 122,
      singbox_path: '/opt/litebox/bin/sing-box',
      resolved_ip: '203.0.113.11',
      singbox_version: 'v1.14.0-litebox',
      build_tags: ['with_utls', 'with_v2ray_api', 'badlinkname', 'tfogo_checklinkname0'],
      has_v2ray_api: true,
      mita_version: '',
      has_unshare: true,
      init_system: 'systemd',
      init_version: '252',
      tcp_forwarding: 'no',
      problems: [],
      warnings: many(
        30,
        (i) =>
          `sshd 禁止了 TCP 转发(/etc/ssh/sshd_config.d/99-hardening-${i}.conf:${i + 3} AllowTcpForwarding no),流量同步、握手目标实测与部署拨测都会失败`,
      ),
    },
  })
}

function mockDiff(api: ApiMock) {
  api.on('GET', `/api/nodes/${NODE}/config-diff`, {
    json: {
      node_id: NODE,
      revision: 3,
      desired_sha256: 'a'.repeat(64),
      remote_sha256: 'b'.repeat(64),
      in_sync: false,
      desired_users: many(80, (i) => `user_${String(i + 1).padStart(6, '0')}`),
      has_singbox: true,
      mieru: [],
      diff: {
        changed: true,
        users: {
          added: many(40, (i) => `user_${String(i + 100).padStart(6, '0')}`),
          removed: many(25, (i) => `user_${String(i + 200).padStart(6, '0')}`),
          uuid_reset: [],
        },
        node_attr: many(12, (i) => `入口 in-${i + 1}:监听端口 ${20000 + i} → ${30000 + i}`),
        summary: '新增 40 个用户,移除 25 个用户,12 项入口属性变化',
      },
    },
  })
}

function mockTuning(api: ApiMock) {
  api.on('POST', `/api/nodes/${NODE}/tcp-tuning/preview`, {
    json: {
      node_id: NODE,
      mode: 'PREVIEW',
      facts: {
        os_name: 'Debian GNU/Linux 12',
        kernel: '6.1.0-26-amd64',
        virt: 'kvm',
        init_system: 'systemd',
        mem_total_kb: 124928,
        cpu_count: 1,
        disk_total_kb: 10_000_000,
        disk_free_kb: 6_000_000,
        cc_available: ['reno', 'cubic'],
        cc_current: 'cubic',
        qdisc_now: 'fq_codel',
        reserved_now: '',
        nofile_limit: 65536,
        has_sysctl: true,
      },
      profile: '128MB 档',
      items: many(24, (i) => ({
        group: i < 8 ? '拥塞控制' : i < 16 ? '缓冲区' : '连接表',
        key: `net.ipv4.tcp_example_key_number_${i}`,
        desired: String(4194304 + i),
        current: String(212992 + i),
        reason: `内存 122MB 的 1/16,夹在 [4MB, 64MB] 之间 —— 第 ${i} 项的说明写得很长,用来看换行与截断`,
        state: 'PENDING',
        detail: '',
      })),
      warnings: many(6, (i) => `第 ${i + 1} 条提醒:/etc/sysctl.d/99-other-${i}.conf 里也设置了同名键,排在面板文件之后,重启后会覆盖这次调优`),
      notes: many(5, (i) => `刻意没做的第 ${i + 1} 件事:不设置 net.ipv4.tcp_mem,它是全系统 TCP 内存的硬上限`),
      conf_path: '/etc/sysctl.d/90-litebox-tcp.conf',
      tuned_at: '',
      baseline_present: false,
      changed: true,
      summary: '24 项待写入',
      generated_at: '2026-10-17T11:46:21Z',
    },
  })
}

async function openOverview(page: Page) {
  await page.goto(`/nodes/${NODE}/overview`)
  await expect(page.getByRole('button', { name: '探测', exact: true })).toBeVisible()
}

/** 弹窗没有超出视口、关闭按钮在视口里、正文在弹窗里面滚。 */
async function expectContained(page: Page, dialog: ReturnType<Page['getByRole']>, w: number, h: number) {
  // 弹窗有一段淡入缩放,探测完还会重读一次节点档案 —— 量到稳定为止。
  const content = dialog.locator('.ant-modal-content')
  await expect.poll(async () => (await content.boundingBox())?.x ?? -1).toBeGreaterThanOrEqual(0)
  await expect.poll(async () => {
    const b = await content.boundingBox()
    return b ? b.x + b.width : Infinity
  }).toBeLessThanOrEqual(w + 1)
  await expect.poll(async () => {
    const b = await content.boundingBox()
    return b ? b.y + b.height : Infinity
  }).toBeLessThanOrEqual(h + 1)
  await expect(dialog.getByRole('button', { name: 'Close' })).toBeInViewport()
  const scrolls = await dialog
    .locator('.ant-modal-body')
    .evaluate((el) => el.scrollHeight > el.clientHeight)
  expect(scrolls, '长内容应在弹窗正文里滚').toBe(true)
  const pageScroll = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)
  expect(pageScroll, '页面不该被弹窗撑出横向滚动').toBe(false)
}

for (const [label, w, h] of WIDTHS) {
  test.describe(`${label} ${w}px`, () => {
    test.use({ viewport: { width: w, height: h } })

    test('探测结果:30 条警告', async ({ page, api }) => {
      mockProbe(api)
      // 探测完会重读一次节点档案,那是 GET,走固定数据。
      await openOverview(page)
      await page.getByRole('button', { name: '探测', exact: true }).click()
      const dialog = page.getByRole('dialog').filter({ hasText: '探测完成' })
      await expect(dialog).toBeVisible()
      await expectContained(page, dialog, w, h)
      // 检查还在跑(探测完要重读一次节点档案)时 ESC 按设计无效;跑完才能关。
      await expect(page.getByText(/探测中…/)).toHaveCount(0)
      // 焦点要进到弹窗里(打开动画结束后才移进去),键盘用户的 ESC / Tab 才落在弹窗上。
      await expect
        .poll(() => page.evaluate(() => !!document.activeElement?.closest('.ant-modal-wrap')))
        .toBe(true)
      await page.keyboard.press('Escape')
      await expect(dialog).toBeHidden()
    })

    test('TCP 调优检查:24 项', async ({ page, api }) => {
      mockTuning(api)
      await openOverview(page)
      await page.getByRole('button', { name: 'TCP 调优' }).click()
      const dialog = page.getByRole('dialog').filter({ hasText: 'net.ipv4.tcp_example_key_number_0' })
      await expect(dialog).toBeVisible()
      await expectContained(page, dialog, w, h)
    })

    test('配置比对:几十个用户变化', async ({ page, api }) => {
      mockDiff(api)
      await page.goto(`/nodes/${NODE}/deploys`)
      await page.getByRole('button', { name: '比对配置' }).first().click()
      const dialog = page.getByRole('dialog').filter({ hasText: '新增 40 个用户' })
      await expect(dialog).toBeVisible()
      await expectContained(page, dialog, w, h)
    })

    test('部署确认框:按钮不插空格、都在视口里', async ({ page }) => {
      await page.goto(`/nodes/${NODE}/entries`)
      await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
      const dialog = page.getByRole('dialog').filter({ hasText: /部署 rev/ })
      await expect(dialog.getByRole('button', { name: '部署', exact: true })).toBeInViewport()
      await expect(dialog.getByRole('button', { name: '取消', exact: true })).toBeInViewport()
      await dialog.getByRole('button', { name: '取消', exact: true }).click()
    })
  })
}

test('重新引导:多段说明保留换行,口令输入框不回显', async ({ page, api }) => {
  api.on('POST', `/api/nodes/${NODE}/bootstrap`, (req) => {
    expect((req.body as { root_password?: string }).root_password).toBe('s3cret')
    return {
      json: {
        node_id: NODE,
        method: 'password',
        already_present: false,
        authorized_keys_path: '/root/.ssh/authorized_keys',
        pubkey_auth_fixed: true,
        password_auth_fixed: true,
        password_auth_error: '',
        detail: '备份在 /etc/ssh/sshd_config.d/00-litebox.conf.bak-20261017;root 账户未锁定',
      },
    }
  })
  await page.goto(`/nodes/${NODE}/overview`)
  await page.getByRole('button', { name: /的更多操作$/ }).click()
  await page.getByRole('menuitem', { name: '重新引导' }).click()
  const form = page.getByRole('dialog').filter({ hasText: '重新引导节点' })
  await form.locator('input[type="password"]').fill('s3cret')
  await form.getByRole('button', { name: /引导/ }).last().click()

  const result = page.getByRole('dialog').filter({ hasText: '面板公钥已装入并验证通过' })
  await expect(result).toBeVisible()
  const content = result.locator('.ant-modal-confirm-content')
  const ws = await content.evaluate((el) => getComputedStyle(el).whiteSpace)
  expect(ws).toBe('pre-line')
  // 三段说明 + 详情,各占一段,而不是挤成一行。
  const lines = await content.evaluate((el) => (el as HTMLElement).innerText.split('\n').filter(Boolean).length)
  expect(lines).toBeGreaterThanOrEqual(3)
  await result.getByRole('button', { name: '知道了' }).click()
})

test('深色模式下的确认框也是深色的', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('litebox.theme', 'dark'))
  await page.goto(`/nodes/${NODE}/entries`)
  await page.locator('[data-service="singbox"]').getByRole('button', { name: '下发配置' }).click()
  const dialog = page.getByRole('dialog').filter({ hasText: /部署 rev/ })
  await expect(dialog).toBeVisible()
  const bg = await dialog.locator('.ant-modal-content').evaluate((el) => getComputedStyle(el).backgroundColor)
  // --surface 深色是 #121923;浅色卡是白的。
  expect(bg).not.toBe('rgb(255, 255, 255)')
  await dialog.getByRole('button', { name: '取消', exact: true }).click()
})
