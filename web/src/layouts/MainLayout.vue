<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { LbTimeText } from '@/components/lb'
import { color } from '@/theme/tokens'

/**
 * 后台布局。白侧栏 + 1px 右边线,不用深色 Sider ——
 * 深色块与「浅灰底 + 白内容区」的方向直接冲突。
 *
 * 侧栏分三组(总览 / 资源 / 运维):六个平铺的菜单项没有层级,
 * 每次都要从头读一遍才能找到目标。分组之后「用户」「节点」永远在中间那块。
 *
 * 断点三档,与表格的断点一致:
 *   >=1280 展开 216px;768–1279 折叠成图标条(不自动隐藏);<768 顶栏汉堡 + 抽屉。
 */
const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

/**
 * 详情页归属它的列表页。
 *
 * 侧栏按 route.name 高亮,而 /nodes/3 的 name 是 node-detail —— 不映射的话
 * 一进详情页整个菜单就没有选中项了,「我在哪」这件事当场丢失。
 * 面包屑同理:它靠同一个 key 找分组名,不映射会退化成一个没有分组的孤零零标题。
 */
const NAV_PARENT: Record<string, string> = {
  'node-detail': 'nodes',
}

const navKey = computed(() => NAV_PARENT[route.name as string] ?? (route.name as string))
const selectedKeys = computed(() => [navKey.value])

/** 侧栏计数。取不到就不显示 —— 显示 0 会被读成「一个都没有」。 */
const counts = ref<{
  users?: number
  nodes?: number
  external?: number
  failedDeploys?: number
}>({})
/** 侧栏底部的流量同步状态。它是全站唯一一处能看出后台任务还活着的地方。 */
const sync = ref<{ lastRun?: string; failing: number } | null>(null)

interface NavItem {
  key: string
  label: string
  /** 计数徽标。undefined 表示不显示 —— 显示 0 会被读成「一个都没有」。 */
  badge?: number
  /** 徽标标红。只给「近 7 天失败部署」这种真需要处理的计数。 */
  danger?: boolean
}

const groups = computed<{ title: string; items: NavItem[] }[]>(() => [
  {
    title: '总览',
    items: [{ key: 'dashboard', label: '仪表盘' }],
  },
  {
    title: '资源',
    items: [
      { key: 'users', label: '用户管理', badge: counts.value.users },
      // 「自建节点」与「外部代理」并列:两者只有「能被用户连」这一点相同 ——
      // 一个是我们有 root 的机器,一个是别人的。都叫「节点」的话,
      // 管理员读预警、读审计时每一次都要先判断说的是哪一类。
      { key: 'nodes', label: '自建节点', badge: counts.value.nodes },
      { key: 'external-proxies', label: '外部代理', badge: counts.value.external },
      // 排在两类线路之后:管理员的工作流是配节点 → 配外部代理 →
      // 配这些东西怎么发出去。
      { key: 'subscription-profiles', label: '订阅配置' },
    ],
  },
  {
    title: '运维',
    items: [
      { key: 'deployments', label: '部署记录', badge: counts.value.failedDeploys, danger: true },
      { key: 'audit-logs', label: '审计日志' },
      { key: 'settings', label: '系统设置' },
    ],
  },
])

/** 面包屑:一级是分组名,二级是页面名。分组名不可点 —— 它不是页面。 */
const crumb = computed(() => {
  for (const g of groups.value) {
    const hit = g.items.find((i) => i.key === navKey.value)
    // 详情页借列表页的分组,但页面名取自己的 —— 顶栏写着「自建节点」
    // 而地址在 /nodes/3 上,会让人以为点错了。
    if (hit) {
      return {
        group: g.title,
        page: navKey.value === route.name ? hit.label : ((route.meta.title as string) ?? hit.label),
      }
    }
  }
  return { group: '', page: (route.meta.title as string) ?? '' }
})

const collapsed = ref(false)
const drawerOpen = ref(false)
const narrow = ref(false)

function onResize() {
  const w = window.innerWidth
  narrow.value = w < 768
  // 768–1279 折叠成图标条。不自动隐藏 —— 隐藏之后管理员会找不到导航在哪。
  collapsed.value = w >= 768 && w < 1280
}

async function loadMeta() {
  // 侧栏计数是装饰性的,任何一个取不到都不该影响页面本身。
  const [u, n, e, d, s] = await Promise.allSettled([
    api.users(),
    api.nodes(),
    api.externalProxies(),
    api.deployments(50),
    api.trafficStatus(),
  ])
  if (u.status === 'fulfilled') counts.value.users = u.value.items.length
  if (n.status === 'fulfilled') counts.value.nodes = n.value.items.length
  if (e.status === 'fulfilled') counts.value.external = e.value.items.length || undefined
  if (d.status === 'fulfilled') {
    const week = Date.now() - 7 * 86400000
    const failed = d.value.items.filter(
      (x) =>
        (x.status === 'FAILED' || x.status === 'ROLLED_BACK') &&
        new Date(x.started_at).getTime() >= week,
    ).length
    counts.value.failedDeploys = failed || undefined
  }
  if (s.status === 'fulfilled') {
    sync.value = { lastRun: s.value.last_run, failing: s.value.failing_nodes.length }
  }
}

onMounted(() => {
  onResize()
  window.addEventListener('resize', onResize)
  loadMeta()
})
onUnmounted(() => window.removeEventListener('resize', onResize))

async function go(key: string) {
  drawerOpen.value = false
  if (key !== route.name) await router.push({ name: key })
}

async function onLogout() {
  try {
    await auth.logout()
    message.success('已退出登录')
  } catch {
    message.warning('退出登录时出错,已在本地清除状态')
  }
  await router.replace({ name: 'login' })
}
</script>

<template>
  <a-layout class="ml">
    <!-- 窄屏走抽屉,桌面走常驻侧栏。两者共用同一份菜单模板。 -->
    <a-layout-sider
      v-if="!narrow"
      :collapsed="collapsed"
      :width="216"
      :collapsed-width="56"
      theme="light"
      class="ml__sider"
    >
      <div class="ml__brand" :class="{ 'ml__brand--mini': collapsed }">
        <span class="ml__logo">{{ collapsed ? 'LB' : 'LiteBox' }}</span>
      </div>

      <nav class="ml__nav">
        <template v-for="g in groups" :key="g.title">
          <div v-if="!collapsed" class="ml__group">{{ g.title }}</div>
          <button
            v-for="it in g.items"
            :key="it.key"
            class="ml__item"
            :class="{ 'ml__item--on': selectedKeys[0] === it.key, 'ml__item--mini': collapsed }"
            :title="collapsed ? it.label : undefined"
            @click="go(it.key)"
          >
            <span class="ml__item-text">{{ collapsed ? it.label.slice(0, 2) : it.label }}</span>
            <span
              v-if="it.badge && !collapsed"
              class="ml__badge"
              :class="{ 'ml__badge--danger': it.danger }"
            >
              {{ it.badge }}
            </span>
          </button>
        </template>
      </nav>

      <!-- 后台任务还活不活着,只有这里看得出来。 -->
      <div v-if="sync && !collapsed" class="ml__sync">
        <span
          class="ml__sync-dot"
          :style="{ background: sync.failing ? color.danger : color.success }"
        />
        <div class="ml__sync-text">
          <div>{{ sync.failing ? `${sync.failing} 个节点同步失败` : '流量同步正常' }}</div>
          <div class="ml__sync-time">
            上次 <LbTimeText :value="sync.lastRun ?? null" empty="尚未运行" />
          </div>
        </div>
      </div>
    </a-layout-sider>

    <a-layout>
      <a-layout-header class="ml__header">
        <a-button v-if="narrow" type="text" class="lb-touch-target" @click="drawerOpen = true">
          ☰
        </a-button>
        <div class="ml__crumb">
          <template v-if="crumb.group && !narrow">
            <span class="ml__crumb-group">{{ crumb.group }}</span>
            <span class="ml__crumb-sep">/</span>
          </template>
          <span class="ml__crumb-page">{{ crumb.page }}</span>
        </div>
        <div class="ml__user">
          <span class="ml__avatar">{{ (auth.admin?.username ?? '?').slice(0, 2).toUpperCase() }}</span>
          <span v-if="!narrow" class="ml__username">{{ auth.admin?.username }}</span>
          <a-button type="text" size="small" @click="onLogout">退出登录</a-button>
        </div>
      </a-layout-header>

      <a-layout-content id="lb-main" class="ml__content">
        <RouterView />
      </a-layout-content>
    </a-layout>
  </a-layout>

  <!-- 窄屏导航。菜单项 48px,点完自动收起。 -->
  <a-drawer v-model:open="drawerOpen" placement="left" :width="240" :body-style="{ padding: '8px 0' }">
    <template #title><span class="ml__logo">LiteBox</span></template>
    <nav class="ml__nav">
      <template v-for="g in groups" :key="g.title">
        <div class="ml__group">{{ g.title }}</div>
        <button
          v-for="it in g.items"
          :key="it.key"
          class="ml__item ml__item--tall"
          :class="{ 'ml__item--on': selectedKeys[0] === it.key }"
          @click="go(it.key)"
        >
          <span class="ml__item-text">{{ it.label }}</span>
          <span v-if="it.badge" class="ml__badge" :class="{ 'ml__badge--danger': it.danger }">
            {{ it.badge }}
          </span>
        </button>
      </template>
    </nav>
  </a-drawer>
</template>

<style scoped>
.ml {
  min-height: 100vh;
}

/*
 * 侧栏与顶栏都吸附在视口上,不跟着内容滚。
 *
 * 用 sticky 而不是 fixed:fixed 会脱离文档流,得再给内容区补一个等宽的
 * margin-left,而侧栏有展开 216 / 折叠 56 两种宽度,补错一次就是内容被压在
 * 侧栏底下。sticky 仍然占位,宽度变化自动跟着走。
 */
.ml__sider {
  position: sticky;
  top: 0;
  height: 100vh;
  /* 白侧栏靠 1px 边线与内容区分开,不靠投影。 */
  border-right: 1px solid #e3e6ea;
  display: flex;
  flex-direction: column;
}

/* 菜单项多到装不下时由侧栏自己滚,不把整页拉长。 */
.ml__sider :deep(.ant-layout-sider-children) {
  display: flex;
  flex-direction: column;
  height: 100%;
  overflow-y: auto;
}

.ml__brand {
  height: 48px;
  display: flex;
  align-items: center;
  padding: 0 16px;
  border-bottom: 1px solid #edeff2;
}

.ml__brand--mini {
  justify-content: center;
  padding: 0;
}

.ml__logo {
  font-size: 15px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.ml__nav {
  flex: 1;
  padding: 8px;
  display: flex;
  flex-direction: column;
  gap: 1px;
  overflow-y: auto;
}

.ml__group {
  padding: 10px 10px 4px;
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  color: #6b7480;
}

.ml__item {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  padding: 0 10px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: #576070;
  font-size: 13px;
  font-family: inherit;
  text-align: left;
  cursor: pointer;
}

.ml__item:hover {
  background: #f1f3f5;
}

.ml__item--on {
  background: #eef4fc;
  color: #1d4f96;
  font-weight: 500;
}

.ml__item--mini {
  justify-content: center;
  padding: 0;
}

.ml__item--tall {
  height: 48px;
  font-size: 14px;
}

.ml__item-text {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ml__item--mini .ml__item-text {
  flex: none;
}

.ml__badge {
  flex: none;
  min-width: 18px;
  padding: 0 5px;
  border-radius: 9px;
  background: #f1f3f5;
  color: #6b7480;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 10.5px;
  line-height: 16px;
  text-align: center;
}

.ml__badge--danger {
  background: #fdecea;
  color: #b4291d;
}

.ml__sync {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 10px 12px;
  border-top: 1px solid #edeff2;
}

.ml__sync-dot {
  width: 7px;
  height: 7px;
  margin-top: 5px;
  border-radius: 50%;
  flex: none;
}

.ml__sync-text {
  min-width: 0;
  font-size: 11.5px;
  color: #576070;
}

.ml__sync-time {
  font-size: 10.5px;
  color: #6b7480;
}

.ml__header {
  position: sticky;
  top: 0;
  /* 盖住滚上来的内容,但要低于抽屉(1000)与弹窗(1000)。 */
  z-index: 20;
  display: flex;
  align-items: center;
  gap: 10px;
  border-bottom: 1px solid #e3e6ea;
}

.ml__crumb {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}

.ml__crumb-group {
  font-size: 13px;
  color: #6b7480;
}

.ml__crumb-sep {
  color: #a9b1bb;
}

.ml__crumb-page {
  font-size: 13px;
  font-weight: 500;
}

.ml__user {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-left: auto;
}

.ml__avatar {
  width: 24px;
  height: 24px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 4px;
  background: #eef4fc;
  color: #1d4f96;
  font-size: 10.5px;
  font-weight: 600;
}

.ml__username {
  font-size: 12.5px;
  color: #576070;
}

.ml__content {
  padding: 20px;
}

@media (max-width: 767px) {
  .ml__content {
    padding: 12px;
  }
}
</style>
