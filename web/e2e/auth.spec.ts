import { test, expect, signedOut, adminOnly, portalOnly } from './support/mock'

/**
 * 登录与路由守卫。管理端与门户是两套完全独立的认证(CLAUDE.md V2):
 * 表、Cookie、中间件都不共享,守卫也各管各的。
 */

const ADMIN = { id: 1, username: 'admin' }

test.describe('管理端', () => {
  test('未登录访问后台页 → 登录页,并带上 redirect', async ({ page, api }) => {
    signedOut(api)
    await page.goto('/nodes')
    await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)nodes$/)
    await expect(page.getByRole('button', { name: '登录' })).toBeDisabled()
  })

  test('口令错误:显示后端给的原因,停在登录页', async ({ page, api }) => {
    signedOut(api).on('POST', '/api/auth/login', { status: 401, json: { error: '用户名或密码错误' } })
    await page.goto('/login')
    await page.getByPlaceholder('admin').fill('admin')
    await page.getByPlaceholder('请输入密码').fill('wrong')
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByText('用户名或密码错误')).toBeVisible()
    await expect(page).toHaveURL(/\/login/)
  })

  test('登录成功后回到原来要去的页面', async ({ page, api }) => {
    let signedIn = false
    api
      .on('GET', '/api/auth/me', () => (signedIn ? { json: ADMIN } : { status: 401, json: { error: '未登录' } }))
      .on('POST', '/api/auth/login', (req) => {
        expect(req.body).toEqual({ username: 'admin', password: 'correct horse' })
        signedIn = true
        return { json: ADMIN }
      })
    api.on('GET', '/api/portal/auth/me', { status: 401, json: { error: '未登录' } })
    await page.goto('/nodes')
    await page.getByPlaceholder('admin').fill('admin')
    await page.getByPlaceholder('请输入密码').fill('correct horse')
    await page.keyboard.press('Enter')
    await expect(page).toHaveURL(/\/nodes$/)
    await expect(page.getByRole('heading', { name: /自建节点/ })).toBeVisible()
  })

  test('已登录访问登录页直接进仪表盘', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/login')
    await expect(page).toHaveURL(/\/dashboard$/)
  })

  test('只有门户会话时进不了后台', async ({ page, api }) => {
    portalOnly(api)
    await page.goto('/dashboard')
    await expect(page).toHaveURL(/\/login\?redirect=/)
  })
})

test.describe('门户', () => {
  test('未登录访问门户页 → 用户登录页(首页)', async ({ page, api }) => {
    signedOut(api)
    await page.goto('/user/nodes')
    await expect(page).toHaveURL(/\/\?redirect=(%2F|\/)user(%2F|\/)nodes$/)
    await expect(page.getByPlaceholder('管理员分配的账号')).toBeVisible()
  })

  test('已登录的管理员打开首页被送回后台', async ({ page, api }) => {
    adminOnly(api)
    await page.goto('/')
    await expect(page).toHaveURL(/\/dashboard$/)
  })

  test('强制改密期间只能停在安全设置,导航只剩这一项', async ({ page, api }) => {
    adminOnly(api).on('GET', '/api/portal/auth/me', {
      json: { username: 'demo', display_name: '测试用户A', must_change_password: true },
    })
    await page.goto('/user/dashboard')
    await expect(page).toHaveURL(/\/user\/security$/)
    const nav = page.getByRole('navigation', { name: '栏目' })
    await expect(nav.getByRole('button')).toHaveText([/安全设置/])
    await page.goto('/user/subscription')
    await expect(page).toHaveURL(/\/user\/security$/)
  })
})
