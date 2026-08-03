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
  {
    path: '/',
    component: () => import('@/layouts/MainLayout.vue'),
    children: [
      { path: '', redirect: '/dashboard' },
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/views/DashboardView.vue'),
        meta: { title: '仪表盘' },
      },
      {
        path: 'users',
        name: 'users',
        component: () => import('@/views/UsersView.vue'),
        meta: { title: '用户管理' },
      },
      {
        path: 'nodes',
        name: 'nodes',
        component: () => import('@/views/NodesView.vue'),
        meta: { title: '节点管理' },
      },
      {
        path: 'deployments',
        name: 'deployments',
        component: () => import('@/views/DeploymentsView.vue'),
        meta: { title: '部署记录' },
      },
      {
        path: 'audit-logs',
        name: 'audit-logs',
        component: () => import('@/views/AuditLogView.vue'),
        meta: { title: '审计日志' },
      },
      {
        path: 'settings',
        name: 'settings',
        component: () => import('@/views/SettingsView.vue'),
        meta: { title: '设置' },
      },
    ],
  },

  // 用户门户。与管理后台是两套独立的路由、布局与守卫,
  // 只共用基础 UI 组件 —— 权限数据源不同,不能靠一个守卫加判断来区分。
  {
    path: '/user/login',
    name: 'portal-login',
    component: () => import('@/views/portal/PortalLoginView.vue'),
    meta: { portal: true, public: true },
  },
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
  {
    path: '/:pathMatch(.*)*',
    redirect: '/dashboard',
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
      return portal.identity ? { name: 'portal-dashboard' } : true
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
