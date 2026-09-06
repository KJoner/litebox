<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore } from '@/stores/theme'
import { LbIcon, LbTimeText, type LbIconName } from '@/components/lb'

/**
 * 后台布局(V18 iOS 风格壳)。白侧栏 232px + 1px 右边线,不用深色 Sider ——
 * 深色块与「浅灰底 + 白内容区」的方向直接冲突。顶栏 52px 毛玻璃。
 *
 * 侧栏分三组(总览 / 资源 / 运维):九个平铺的菜单项没有层级,
 * 每次都要从头读一遍才能找到目标。分组之后「用户」「节点」永远在中间那块。
 *
 * 断点三档,与表格的断点一致:
 *   >=1280 展开 232px;768–1279 折叠成图标条(不自动隐藏);<768 顶栏汉堡 + 抽屉。
 */
const auth = useAuthStore()
const themeStore = useThemeStore()
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
  icon: LbIconName
  /** 计数徽标。undefined 表示不显示 —— 显示 0 会被读成「一个都没有」。 */
  badge?: number
  /** 徽标标红。只给「近 7 天失败部署」这种真需要处理的计数。 */
  danger?: boolean
}

const groups = computed<{ title: string; items: NavItem[] }[]>(() => [
  {
    title: '总览',
    items: [{ key: 'dashboard', label: '仪表盘', icon: 'layout-grid' }],
  },
  {
    title: '资源',
    items: [
      { key: 'users', label: '用户管理', icon: 'users', badge: counts.value.users },
      // 「自建节点」与「外部代理」并列:两者只有「能被用户连」这一点相同 ——
      // 一个是我们有 root 的机器,一个是别人的。都叫「节点」的话,
      // 管理员读预警、读审计时每一次都要先判断说的是哪一类。
      { key: 'nodes', label: '自建节点', icon: 'server', badge: counts.value.nodes },
      // 紧跟自建节点:入口是机器的一部分,而这一页只是换个方向去看它们。
      { key: 'inbounds', label: '入口管理', icon: 'log-in' },
      { key: 'external-proxies', label: '外部代理', icon: 'external-link', badge: counts.value.external },
      // 排在两类线路之后:管理员的工作流是配节点 → 配外部代理 →
      // 配这些东西怎么发出去。
      { key: 'subscription-profiles', label: '订阅配置', icon: 'file-text' },
    ],
  },
  {
    title: '运维',
    items: [
      { key: 'deployments', label: '部署记录', icon: 'clock', badge: counts.value.failedDeploys, danger: true },
      { key: 'audit-logs', label: '审计日志', icon: 'list' },
      { key: 'settings', label: '系统设置', icon: 'settings' },
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

const initials = computed(() => (auth.admin?.username ?? '?').slice(0, 2).toUpperCase())
</script>

<template>
  <div class="ml">
    <!-- 窄屏走抽屉,桌面走常驻侧栏。两者共用同一份菜单模板。 -->
    <aside v-if="!narrow" class="ml__sider" :class="{ 'ml__sider--mini': collapsed }">
      <div class="ml__brand">
        <span class="ml__logo">LB</span>
        <span v-if="!collapsed" class="ml__brand-name">LiteBox</span>
      </div>

      <nav class="ml__nav">
        <template v-for="g in groups" :key="g.title">
          <div v-if="!collapsed" class="ml__group">{{ g.title }}</div>
          <div v-else class="ml__group-gap" />
          <button
            v-for="it in g.items"
            :key="it.key"
            class="ml__item"
            :class="{ 'ml__item--on': selectedKeys[0] === it.key, 'ml__item--mini': collapsed }"
            :title="collapsed ? it.label : undefined"
            @click="go(it.key)"
          >
            <LbIcon :name="it.icon" />
            <span v-if="!collapsed" class="ml__item-text">{{ it.label }}</span>
            <span
              v-if="it.badge && !collapsed"
              class="ml__badge lb-tabular"
              :class="{ 'ml__badge--danger': it.danger }"
            >
              {{ it.badge }}
            </span>
            <span v-else-if="it.badge && collapsed && it.danger" class="ml__dot" />
          </button>
        </template>
      </nav>

      <!-- 后台任务还活不活着,只有这里看得出来。 -->
      <div v-if="sync && !collapsed" class="ml__sync">
        <span class="ml__sync-dot" :class="{ 'ml__sync-dot--bad': sync.failing }" />
        <div class="ml__sync-text">
          <div class="ml__sync-title">
            {{ sync.failing ? `${sync.failing} 个节点同步失败` : '流量同步正常' }}
          </div>
          <div class="ml__sync-time">
            上次 <LbTimeText :value="sync.lastRun ?? null" empty="尚未运行" />
          </div>
        </div>
      </div>
      <div v-else-if="sync && collapsed" class="ml__sync ml__sync--mini" :title="sync.failing ? `${sync.failing} 个节点同步失败` : '流量同步正常'">
        <span class="ml__sync-dot" :class="{ 'ml__sync-dot--bad': sync.failing }" />
      </div>
    </aside>

    <div class="ml__main">
      <header class="ml__header">
        <button v-if="narrow" type="button" class="ml__iconbtn lb-touch-target" aria-label="打开导航" @click="drawerOpen = true">
          <LbIcon name="menu" :size="18" />
        </button>
        <div class="ml__crumb">
          <template v-if="crumb.group && !narrow">
            <span class="ml__crumb-group">{{ crumb.group }}</span>
            <span class="ml__crumb-sep">›</span>
          </template>
          <span class="ml__crumb-page">{{ crumb.page }}</span>
        </div>
        <div class="ml__user">
          <button
            type="button"
            class="ml__iconbtn"
            :aria-label="themeStore.mode === 'dark' ? '切换到浅色' : '切换到深色'"
            :title="themeStore.mode === 'dark' ? '切换到浅色' : '切换到深色'"
            @click="themeStore.toggle()"
          >
            <LbIcon :name="themeStore.mode === 'dark' ? 'sun' : 'moon'" :size="16" />
          </button>
          <span class="ml__avatar">{{ initials }}</span>
          <span v-if="!narrow" class="ml__username">{{ auth.admin?.username }}</span>
          <button type="button" class="ml__logout" @click="onLogout">退出登录</button>
        </div>
      </header>

      <main id="lb-main" class="ml__content">
        <RouterView />
      </main>
    </div>
  </div>

  <!-- 窄屏导航。菜单项 48px,点完自动收起。 -->
  <a-drawer v-model:open="drawerOpen" placement="left" :width="260" :body-style="{ padding: '8px 12px' }" class="ml__drawer">
    <template #title>
      <span class="ml__brand ml__brand--drawer">
        <span class="ml__logo">LB</span>
        <span class="ml__brand-name">LiteBox</span>
      </span>
    </template>
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
          <LbIcon :name="it.icon" />
          <span class="ml__item-text">{{ it.label }}</span>
          <span v-if="it.badge" class="ml__badge lb-tabular" :class="{ 'ml__badge--danger': it.danger }">
            {{ it.badge }}
          </span>
        </button>
      </template>
    </nav>
  </a-drawer>
</template>

<style scoped>
.ml {
  display: flex;
  min-height: 100vh;
  background: var(--bg);
}

/*
 * 侧栏与顶栏都吸附在视口上,不跟着内容滚。
 *
 * 用 sticky 而不是 fixed:fixed 会脱离文档流,得再给内容区补一个等宽的
 * margin-left,而侧栏有展开 232 / 折叠 64 两种宽度,补错一次就是内容被压在
 * 侧栏底下。sticky 仍然占位,宽度变化自动跟着走。
 */
.ml__sider {
  position: sticky;
  top: 0;
  flex: none;
  width: 232px;
  height: 100vh;
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-right: 1px solid var(--sep);
  transition: width 0.25s var(--ease), background 0.35s;
}

.ml__sider--mini {
  width: 64px;
}

.ml__brand {
  height: 60px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 20px;
}
.ml__sider--mini .ml__brand {
  justify-content: center;
  padding: 0;
}
.ml__brand--drawer {
  height: auto;
  padding: 0;
}

.ml__logo {
  width: 28px;
  height: 28px;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 9px;
  background: linear-gradient(145deg, var(--brand), var(--brand-hover));
  color: var(--surface);
  font-size: 12px;
  font-weight: 700;
  letter-spacing: -0.02em;
  box-shadow: 0 2px 6px rgba(37, 99, 184, 0.3);
}

.ml__brand-name {
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.02em;
  color: var(--text);
}

.ml__nav {
  flex: 1;
  padding: 6px 12px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  overflow-y: auto;
  min-height: 0;
}
.ml__sider--mini .ml__nav {
  padding: 6px 10px;
}

.ml__group {
  padding: 14px 12px 6px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--text3);
}
.ml__group-gap {
  height: 12px;
}

.ml__item {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 36px;
  padding: 0 12px;
  border: none;
  border-radius: var(--r-pill);
  background: transparent;
  color: var(--text2);
  font-size: 13.5px;
  font-weight: 500;
  font-family: inherit;
  text-align: left;
  cursor: pointer;
  transition: background 0.2s, color 0.2s, transform 0.15s;
}

.ml__item:hover {
  background: var(--fill);
  color: var(--text);
}
.ml__item:active {
  transform: scale(0.98);
}

.ml__item--on,
.ml__item--on:hover {
  background: var(--brand);
  color: var(--surface);
  font-weight: 600;
}

.ml__item--mini {
  justify-content: center;
  width: 44px;
  padding: 0;
  margin: 0 auto;
  position: relative;
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

.ml__badge {
  flex: none;
  min-width: 20px;
  padding: 0 6px;
  border-radius: 10px;
  background: var(--fill);
  color: var(--text2);
  font-size: 11px;
  font-weight: 600;
  line-height: 18px;
  text-align: center;
}
.ml__item--on .ml__badge {
  background: rgba(255, 255, 255, 0.25);
  color: var(--surface);
}

.ml__badge--danger {
  background: var(--bad-bg);
  color: var(--bad);
}

.ml__dot {
  position: absolute;
  top: 8px;
  right: 9px;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--bad);
}

.ml__sync {
  flex: none;
  margin: 12px;
  padding: 12px 14px;
  border-radius: var(--r-group);
  background: var(--surface2);
  display: flex;
  align-items: flex-start;
  gap: 10px;
}
.ml__sync--mini {
  justify-content: center;
  padding: 10px;
  margin: 12px 10px;
}

.ml__sync-dot {
  width: 8px;
  height: 8px;
  margin-top: 5px;
  border-radius: 50%;
  flex: none;
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(27, 122, 75, 0.15);
}
.ml__sync--mini .ml__sync-dot {
  margin-top: 0;
}
.ml__sync-dot--bad {
  background: var(--bad);
  box-shadow: 0 0 0 3px rgba(180, 41, 29, 0.15);
}

.ml__sync-text {
  min-width: 0;
  font-size: 12.5px;
  color: var(--text);
}
.ml__sync-title {
  font-weight: 500;
}

.ml__sync-time {
  font-size: 11.5px;
  color: var(--text3);
  margin-top: 1px;
}

.ml__main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.ml__header {
  position: sticky;
  top: 0;
  /* 盖住滚上来的内容,但要低于抽屉(1000)与弹窗(1000)。 */
  z-index: 20;
  height: 52px;
  flex: none;
  padding: 0 32px;
  display: flex;
  align-items: center;
  gap: 10px;
  background: var(--glass);
  backdrop-filter: saturate(180%) blur(20px);
  -webkit-backdrop-filter: saturate(180%) blur(20px);
  border-bottom: 1px solid var(--sep);
}

.ml__crumb {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  font-size: 13px;
}

.ml__crumb-group {
  color: var(--text3);
}

.ml__crumb-sep {
  color: var(--text3);
  font-size: 11px;
}

.ml__crumb-page {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ml__user {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-left: auto;
}

.ml__iconbtn {
  width: 28px;
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: var(--text2);
  cursor: pointer;
  transition: background 0.2s, color 0.2s;
}
.ml__iconbtn:hover {
  background: var(--fill);
  color: var(--text);
}

.ml__avatar {
  width: 28px;
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: var(--brand-bg);
  color: var(--brand);
  font-size: 11px;
  font-weight: 600;
}

.ml__username {
  font-size: 13px;
  color: var(--text2);
}

.ml__logout {
  height: 28px;
  padding: 0 12px;
  border: none;
  border-radius: var(--r-pill);
  background: transparent;
  color: var(--brand);
  font-size: 13px;
  font-family: inherit;
  cursor: pointer;
  transition: background 0.2s;
}
.ml__logout:hover {
  background: var(--fill);
}

.ml__content {
  width: 100%;
  max-width: 1440px;
  padding: 28px 32px 48px;
  box-sizing: border-box;
}

@media (max-width: 767px) {
  .ml__header {
    padding: 0 12px;
  }
  .ml__content {
    padding: 16px 12px 32px;
  }
}
</style>
