import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { Modal, message } from 'ant-design-vue'
import { api, isSessionExpired, portalApi, setSessionExpiredHandler, type SessionScope } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { usePortalStore } from '@/stores/portal'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { public: true },
  },
  // 管理后台。父路由只是布局容器,刻意不占用 `/` —— 根路径要留给用户登录页。
  // 子路由一律用绝对路径,所以各管理页的地址(/dashboard、/users…)一个没变,
  // 已有的书签继续有效。`/_admin` 本身没有空子路由,不会被访问到。
  {
    path: '/_admin',
    component: () => import('@/layouts/MainLayout.vue'),
    children: [
      {
        path: '/dashboard',
        name: 'dashboard',
        component: () => import('@/views/DashboardView.vue'),
        meta: { title: '仪表盘' },
      },
      {
        path: '/users',
        name: 'users',
        component: () => import('@/views/UsersView.vue'),
        meta: { title: '用户管理' },
      },
      {
        path: '/nodes',
        name: 'nodes',
        component: () => import('@/views/NodesView.vue'),
        // 界面上叫「自建节点」,但路由与 API 一个字没改 ——
        // 重命名一个被十几处引用的标识,收益只是名字好看一点,
        // 而审计日志里已经写死的历史记录改不回来。
        meta: { title: '自建节点' },
      },
      {
        // 节点详情整页。tab 进地址栏 —— 整页之后地址是可以分享的,
        // 而「打开某台机器的入口列表」正是最常被转述的一件事;
        // 只记住 /nodes/3 的话,对方点开落在概览上,还得再找一次。
        path: '/nodes/:id/:tab?',
        name: 'node-detail',
        component: () => import('@/views/NodeDetailView.vue'),
        meta: { title: '节点详情' },
      },
      {
        // 入口管理与自建节点并列而不是塞进节点详情:机器多起来之后,
        // 「这套系统一共对外开了哪些口子」只能一台台点进去拼,
        // 而那是个横着看的问题,节点详情是竖着切的。
        path: '/inbounds',
        name: 'inbounds',
        component: () => import('@/views/InboundsView.vue'),
        meta: { title: '入口管理' },
      },
      {
        path: '/external-proxies',
        name: 'external-proxies',
        component: () => import('@/views/ExternalProxiesView.vue'),
        meta: { title: '外部代理' },
      },
      {
        path: '/subscription-profiles',
        name: 'subscription-profiles',
        component: () => import('@/views/SubscriptionProfilesView.vue'),
        meta: { title: '订阅配置' },
      },
      {
        // 公共探测目标库(出站网络质量的目标:自定义、Zstatic 预置、TcpQuality 同步)。
        // 原来在系统设置里的一小节,目标多起来之后要筛选、批量与来源管理,单独成页。
        path: '/probe-library',
        name: 'probe-library',
        component: () => import('@/views/ProbeLibraryView.vue'),
        meta: { title: '探测目标库' },
      },
      {
        path: '/deployments',
        name: 'deployments',
        component: () => import('@/views/DeploymentsView.vue'),
        meta: { title: '部署记录' },
      },
      {
        path: '/audit-logs',
        name: 'audit-logs',
        component: () => import('@/views/AuditLogView.vue'),
        meta: { title: '审计日志' },
      },
      {
        path: '/settings',
        name: 'settings',
        component: () => import('@/views/SettingsView.vue'),
        meta: { title: '设置' },
      },
    ],
  },

  // 用户门户。与管理后台是两套独立的路由、布局与守卫,
  // 只共用基础 UI 组件 —— 权限数据源不同,不能靠一个守卫加判断来区分。
  // 首页就是用户登录页。
  //
  // 用户有 10 个、管理员只有 1 个,谁更多谁就该占默认路径。管理员开通账号后
  // 手边只有面板地址,发首页是最自然的动作 —— 首页要是管理员登录页,
  // 用户在那里输账号只会得到「用户名或密码错误」,看起来完全就是密码发错了。
  //
  // name 保持 portal-login,所有 { name: 'portal-login' } 的跳转自动指向这里。
  {
    path: '/',
    name: 'portal-login',
    component: () => import('@/views/portal/PortalLoginView.vue'),
    meta: { portal: true, public: true },
  },
  // 已经发出去的旧地址继续可用。
  { path: '/user/login', redirect: '/' },
  {
    path: '/user',
    component: () => import('@/layouts/PortalLayout.vue'),
    meta: { portal: true },
    children: [
      { path: '', redirect: '/user/dashboard' },
      {
        path: 'dashboard',
        name: 'portal-dashboard',
        component: () => import('@/views/portal/PortalDashboardView.vue'),
        meta: { portal: true, title: '概览' },
      },
      {
        path: 'subscription',
        name: 'portal-subscription',
        component: () => import('@/views/portal/PortalSubscriptionView.vue'),
        meta: { portal: true, title: '我的订阅' },
      },
      {
        path: 'nodes',
        name: 'portal-nodes',
        component: () => import('@/views/portal/PortalNodesView.vue'),
        meta: { portal: true, title: '我的节点' },
      },
      {
        path: 'traffic',
        name: 'portal-traffic',
        component: () => import('@/views/portal/PortalTrafficView.vue'),
        meta: { portal: true, title: '我的流量' },
      },
      {
        // 探针:用户能用的那几台机器的资源百分比与网络探测(只读库里已有的数据)。
        path: 'probe',
        name: 'portal-probe',
        component: () => import('@/views/portal/PortalProbeView.vue'),
        meta: { portal: true, title: '探针' },
      },
      {
        path: 'security',
        name: 'portal-security',
        component: () => import('@/views/portal/PortalSecurityView.vue'),
        meta: { portal: true, title: '安全设置' },
      },
    ],
  },

  // 门户下的未知路径回到门户首页,不能落到全局兜底 ——
  // 那会把普通用户甩到管理后台的登录页,看起来像"我的账号没了"。
  {
    path: '/user/:pathMatch(.*)*',
    redirect: '/user/dashboard',
  },
  // 陌生路径回首页。已登录的管理员会被守卫送到 /dashboard,
  // 未登录的人看到的是用户登录页 —— 后者才是绝大多数访问者。
  {
    path: '/:pathMatch(.*)*',
    redirect: '/',
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach(async (to) => {
  if (to.meta.portal) {
    const portal = usePortalStore()
    await portal.resolve()

    if (to.meta.public) {
      if (portal.identity) return { name: 'portal-dashboard' }
      // 首页同时是管理员的入口。已登录的管理员访问它不该被晾在用户登录页,
      // 直接送回后台 —— 他只有一个人,但每天都要进来。
      if (to.name === 'portal-login') {
        const auth = useAuthStore()
        await auth.resolve()
        if (auth.admin) return { name: 'dashboard' }
      }
      return true
    }
    if (!portal.identity) {
      return { name: 'portal-login', query: { redirect: to.fullPath } }
    }
    // 强制改密的用户只能待在安全设置页。后端也会挡住其他接口,
    // 这里只是让他直接看到该做什么,而不是一路 403。
    if (portal.identity.must_change_password && to.name !== 'portal-security') {
      return { name: 'portal-security' }
    }
    return true
  }

  const auth = useAuthStore()
  // 首次进入任意页面时先向后端确认一次登录状态,
  // 避免刷新页面后误判为未登录。
  await auth.resolve()

  if (to.meta.public) {
    // 已登录时不再停留在登录页。
    return auth.admin ? { name: 'dashboard' } : true
  }
  if (!auth.admin) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  return true
})

/**
 * 登录页上的 redirect 只收站内路径:以 / 开头、不是 //(那是另一个主机)。
 * 会话失效时会带着它跳转,登录完照着它回去 —— 收外部地址就成了一个开放跳转。
 */
export function safeRedirect(raw: unknown, fallback: string): string {
  return typeof raw === 'string' && raw.startsWith('/') && !raw.startsWith('//') ? raw : fallback
}

// ---------- 会话失效 ----------
//
// 会话在后端失效之后(过期、改密码踢掉、管理员撤销门户会话),原来页面上只会一个接口接一个接口地
// 报错。现在任何一个接口拿到「会话没了」的 401,就清掉这一边的登录状态、送回对应的登录页,
// 地址里带着原来的页面,登录完回到那里。判据只认 code = UNAUTHENTICATED(见 api/client.ts)。

let expiring = false

function sessionExpired(scope: SessionScope): void {
  const current = router.currentRoute.value
  // 已经在登录页(或别的公开页)上:那里的请求本来就可能 401,不再跳一次。
  if (expiring || current.meta.public) return
  // 只管当前所在的那一边:后台页面上不会有门户请求,反过来也一样;对不上就不动。
  const portal = !!current.meta.portal
  if ((scope === 'portal') !== portal) return
  expiring = true
  if (portal) usePortalStore().clear()
  else useAuthStore().clear()
  void router
    .replace({ name: portal ? 'portal-login' : 'login', query: { redirect: current.fullPath, expired: '1' } })
    .finally(() => {
      expiring = false
      // 跳走之前那几秒里,页面上各处可能已经弹了「登录已失效」的吐司或错误框:登录页自己会说明,收掉它们。
      Modal.destroyAll()
      message.destroy()
    })
}

setSessionExpiredHandler(sessionExpired)

// 标签页藏起来一段时间再切回来时问一次「还登着吗」:不等用户点了什么才发现会话早就没了。
// 只问 /auth/me(不在会话检查的名单里自动处理,这里显式走同一条路)。
let hiddenAt = 0
if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      hiddenAt = Date.now()
      return
    }
    if (!hiddenAt || Date.now() - hiddenAt < 60_000) return
    hiddenAt = 0
    const current = router.currentRoute.value
    if (current.meta.public) return
    const scope: SessionScope = current.meta.portal ? 'portal' : 'admin'
    const check = scope === 'portal' ? portalApi.me() : api.me()
    check.catch((err) => {
      if (isSessionExpired(err)) sessionExpired(scope)
    })
  })
}
