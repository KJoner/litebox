import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
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
        meta: { title: '节点管理' },
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
