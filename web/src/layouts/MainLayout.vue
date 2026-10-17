<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api, type TrafficStatus } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { useThemeStore, type ThemePreference } from '@/stores/theme'
import { LbIcon, LbTimeText, type LbIconName } from '@/components/lb'
import { summarizeSync, SYNC_DOT, SYNC_STALE_MS } from './syncStatus'

/**
 * 后台布局(UI 2.0)。白侧栏 240px + 1px 右边线,顶栏 56px 实底 + 下边线,
 * 内容区 --bg 底 —— 「L 形外框 + 浅灰内容区」是云控制台最常见的骨架,
 * 卡片在内容区里靠细边框分层。
 *
 * 侧栏分三组(总览 / 资源 / 运维):九个平铺的菜单项没有层级,
 * 每次都要从头读一遍才能找到目标。分组之后「用户」「节点」永远在中间那块。
 *
 * 折叠:顶栏左侧的按钮手动切换,偏好存 localStorage;没设过偏好时按宽度自动
 * (≥1440 展开、768–1439 折叠成图标条,不自动隐藏 —— 隐藏之后管理员会找不到导航)。
 * 阈值取 1440 而不是 1280:节点、用户、入口几张表连勾选列在内要 1050~1110 宽,
 * 1280 / 1366 的笔记本上侧栏展开时内容区只有 990~1078,表格只能横向滚动。
 * <768 顶栏汉堡 + 抽屉。
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

/** 侧栏计数。取不到就不显示 —— 显示 0 会被读成「一个都没有」。 */
const counts = ref<{
  users?: number
  nodes?: number
  external?: number
  failedDeploys?: number
}>({})
/**
 * 侧栏底部的流量同步状态。它是全站唯一一处能看出后台任务还活着的地方,
 * 所以每分钟重拉一次 —— 只在挂载时读一次的话,开着页面一小时后它还在说
 * 「上次完成 刚刚」。读失败时保留上一次的值并标出来,不让这一格消失。
 */
const syncStatus = ref<TrafficStatus | null>(null)
const syncLoadFailed = ref(false)
const sync = computed(() => summarizeSync(syncStatus.value, syncLoadFailed.value))
const syncTooltip = computed(() => (sync.value ? `${sync.value.title} · ${sync.value.detail}` : ''))

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

/**
 * 面包屑:分组 / 列表页 / 详情页。分组名不可点 —— 它不是页面;
 * 详情页时中间那一级是列表页,可以点回去。
 */
const crumb = computed(() => {
  for (const g of groups.value) {
    const hit = g.items.find((i) => i.key === navKey.value)
    if (!hit) continue
    const isDetail = navKey.value !== route.name
    return {
      group: g.title,
      parent: isDetail ? { key: hit.key, label: hit.label } : null,
      page: isDetail ? ((route.meta.title as string) ?? hit.label) : hit.label,
    }
  }
  return { group: '', parent: null, page: (route.meta.title as string) ?? '' }
})

// ---------- 折叠 ----------

const SIDER_KEY = 'litebox.sider'
function readSiderPref(): 'expanded' | 'collapsed' | null {
  try {
    const v = localStorage.getItem(SIDER_KEY)
    return v === 'expanded' || v === 'collapsed' ? v : null
  } catch {
    return null
  }
}
const siderPref = ref(readSiderPref())
const width = ref(window.innerWidth)
const narrow = computed(() => width.value < 768)
const collapsed = computed(() => {
  if (narrow.value) return false
  if (siderPref.value) return siderPref.value === 'collapsed'
  return width.value < 1440
})
const drawerOpen = ref(false)

function toggleSider() {
  if (narrow.value) {
    drawerOpen.value = true
    return
  }
  siderPref.value = collapsed.value ? 'expanded' : 'collapsed'
  try {
    localStorage.setItem(SIDER_KEY, siderPref.value)
  } catch {
    /* 存不下就只在这次会话里生效 */
  }
}

function onResize() {
  width.value = window.innerWidth
}

// ---------- 主题 ----------

const themeOptions: { key: ThemePreference; label: string; icon: LbIconName }[] = [
  { key: 'light', label: '浅色', icon: 'sun' },
  { key: 'dark', label: '深色', icon: 'moon' },
  { key: 'system', label: '跟随系统', icon: 'monitor' },
]
const themeIcon = computed<LbIconName>(
  () => themeOptions.find((o) => o.key === themeStore.preference)?.icon ?? 'monitor',
)
function onTheme({ key }: { key: string | number }) {
  themeStore.setPreference(key as ThemePreference)
}

// ---------- 数据 ----------

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
  applySync(s)
}

function applySync(r: PromiseSettledResult<TrafficStatus>) {
  if (r.status === 'fulfilled') {
    syncStatus.value = r.value
    syncLoadFailed.value = false
  } else {
    syncLoadFailed.value = true
  }
}

async function refreshSync() {
  if (document.hidden) return
  const [r] = await Promise.allSettled([api.trafficStatus()])
  applySync(r)
}

let syncTimer: number | undefined
onMounted(() => {
  window.addEventListener('resize', onResize)
  loadMeta()
  syncTimer = window.setInterval(refreshSync, 60_000)
})
onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  window.clearInterval(syncTimer)
})

async function go(key: string) {
  drawerOpen.value = false
  if (key !== route.name) await router.push({ name: key })
}

async function onUserMenu({ key }: { key: string | number }) {
  if (key !== 'logout') return
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
  <div class="ml" :class="{ 'ml--mini': collapsed }">
    <!-- 窄屏走抽屉,桌面走常驻侧栏。两者共用同一份菜单结构。 -->
    <aside v-if="!narrow" class="ml__sider">
      <div class="ml__brand">
        <span class="ml__logo">LB</span>
        <span v-if="!collapsed" class="ml__brand-text">
          <span class="ml__brand-name">LiteBox</span>
          <span class="ml__brand-sub">节点管理</span>
        </span>
      </div>

      <nav class="ml__nav" aria-label="主导航">
        <template v-for="g in groups" :key="g.title">
          <div v-if="!collapsed" class="ml__group">{{ g.title }}</div>
          <div v-else class="ml__group-gap" />
          <a-tooltip v-for="it in g.items" :key="it.key" :title="collapsed ? it.label : undefined" placement="right">
            <button
              type="button"
              class="ml__item"
              :class="{ 'ml__item--on': navKey === it.key }"
              :aria-current="navKey === it.key ? 'page' : undefined"
              :aria-label="it.label"
              @click="go(it.key)"
            >
              <LbIcon :name="it.icon" :size="18" />
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
          </a-tooltip>
        </template>
      </nav>

      <!-- 后台任务还活不活着,只有这里看得出来。 -->
      <a-tooltip v-if="sync" :title="collapsed ? syncTooltip : undefined" placement="right">
        <div
          class="ml__sync"
          :class="{ 'ml__sync--mini': collapsed }"
          role="status"
          :aria-label="syncTooltip"
          data-testid="sync-status"
        >
          <span class="lb-dot" :style="{ '--dot': SYNC_DOT[sync.tone] }" />
          <div v-if="!collapsed" class="ml__sync-text">
            <div class="ml__sync-title">{{ sync.title }}</div>
            <div class="ml__sync-time">
              {{ sync.detail }}
              <LbTimeText v-if="sync.at" :value="sync.at" :warn-after-ms="SYNC_STALE_MS" />
            </div>
          </div>
        </div>
      </a-tooltip>
    </aside>

    <div class="ml__main">
      <header class="ml__header">
        <button
          type="button"
          class="ml__iconbtn lb-touch-target"
          :aria-label="narrow ? '打开导航' : collapsed ? '展开侧栏' : '收起侧栏'"
          :title="narrow ? '打开导航' : collapsed ? '展开侧栏' : '收起侧栏'"
          @click="toggleSider"
        >
          <LbIcon :name="narrow ? 'menu' : 'panel-left'" :size="18" />
        </button>
        <span v-if="!narrow" class="ml__vsep" />
        <nav class="ml__crumb" aria-label="面包屑">
          <template v-if="crumb.group && !narrow">
            <span class="ml__crumb-group">{{ crumb.group }}</span>
            <LbIcon name="chevron-right" :size="14" class="ml__crumb-sep" />
          </template>
          <template v-if="crumb.parent">
            <a class="ml__crumb-link" @click="go(crumb.parent.key)">{{ crumb.parent.label }}</a>
            <LbIcon name="chevron-right" :size="14" class="ml__crumb-sep" />
          </template>
          <span class="ml__crumb-page" aria-current="page">{{ crumb.page }}</span>
        </nav>

        <div class="ml__tools">
          <a-dropdown :trigger="['click']" placement="bottomRight">
            <button type="button" class="ml__iconbtn" aria-label="切换主题" title="主题">
              <LbIcon :name="themeIcon" :size="17" />
            </button>
            <template #overlay>
              <a-menu :selected-keys="[themeStore.preference]" class="ml__menu" @click="onTheme">
                <a-menu-item v-for="o in themeOptions" :key="o.key">
                  <span class="ml__menu-item">
                    <LbIcon :name="o.icon" :size="15" />
                    <span>{{ o.label }}</span>
                    <LbIcon v-if="themeStore.preference === o.key" name="check" :size="14" class="ml__menu-check" />
                  </span>
                </a-menu-item>
              </a-menu>
            </template>
          </a-dropdown>

          <a-dropdown :trigger="['click']" placement="bottomRight">
            <button type="button" class="ml__user" aria-label="账户菜单">
              <span class="ml__avatar">{{ initials }}</span>
              <span v-if="!narrow" class="ml__username">{{ auth.admin?.username }}</span>
              <LbIcon v-if="!narrow" name="chevron-down" :size="14" class="ml__user-caret" />
            </button>
            <template #overlay>
              <a-menu class="ml__menu" @click="onUserMenu">
                <a-menu-item-group>
                  <template #title>
                    <div class="ml__menu-who">
                      <span class="ml__menu-name">{{ auth.admin?.username }}</span>
                      <span class="ml__menu-role">管理员</span>
                    </div>
                  </template>
                </a-menu-item-group>
                <a-menu-divider />
                <a-menu-item key="logout">
                  <span class="ml__menu-item">
                    <LbIcon name="log-out" :size="15" />
                    <span>退出登录</span>
                  </span>
                </a-menu-item>
              </a-menu>
            </template>
          </a-dropdown>
        </div>
      </header>

      <main id="lb-main" class="ml__content">
        <RouterView />
      </main>
    </div>
  </div>

  <!-- 窄屏导航。菜单项 44px,点完自动收起。 -->
  <a-drawer v-model:open="drawerOpen" placement="left" :width="264" :body-style="{ padding: '8px 12px' }" class="ml__drawer">
    <template #title>
      <span class="ml__brand ml__brand--drawer">
        <span class="ml__logo">LB</span>
        <span class="ml__brand-name">LiteBox</span>
      </span>
    </template>
    <nav class="ml__nav" aria-label="主导航">
      <template v-for="g in groups" :key="g.title">
        <div class="ml__group">{{ g.title }}</div>
        <button
          v-for="it in g.items"
          :key="it.key"
          type="button"
          class="ml__item ml__item--tall"
          :class="{ 'ml__item--on': navKey === it.key }"
          @click="go(it.key)"
        >
          <LbIcon :name="it.icon" :size="18" />
          <span class="ml__item-text">{{ it.label }}</span>
          <span v-if="it.badge" class="ml__badge lb-tabular" :class="{ 'ml__badge--danger': it.danger }">
            {{ it.badge }}
          </span>
        </button>
      </template>
    </nav>
    <div v-if="sync" class="ml__sync ml__sync--drawer" role="status" :aria-label="syncTooltip">
      <span class="lb-dot" :style="{ '--dot': SYNC_DOT[sync.tone] }" />
      <div class="ml__sync-text">
        <div class="ml__sync-title">{{ sync.title }}</div>
        <div class="ml__sync-time">
          {{ sync.detail }}
          <LbTimeText v-if="sync.at" :value="sync.at" :warn-after-ms="SYNC_STALE_MS" />
        </div>
      </div>
    </div>
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
 * margin-left,而侧栏有展开 / 折叠两种宽度,补错一次就是内容被压在
 * 侧栏底下。sticky 仍然占位,宽度变化自动跟着走。
 */
.ml__sider {
  position: sticky;
  top: 0;
  flex: none;
  width: var(--sider-w);
  height: 100vh;
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border-right: 1px solid var(--sep);
  transition: width var(--dur-slow) var(--ease);
}
.ml--mini .ml__sider {
  width: var(--sider-w-mini);
}

.ml__brand {
  height: var(--header-h);
  flex: none;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 0 var(--sp-4);
  border-bottom: 1px solid var(--sep2);
}
.ml--mini .ml__brand {
  justify-content: center;
  padding: 0;
}
.ml__brand--drawer {
  height: auto;
  padding: 0;
  border: none;
}

.ml__logo {
  width: 30px;
  height: 30px;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--r-btn);
  background: var(--brand);
  color: var(--on-brand);
  font-size: var(--fs-xs);
  font-weight: 700;
  letter-spacing: -0.02em;
}

.ml__brand-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
  line-height: 1.2;
}
.ml__brand-name {
  font-size: var(--fs-md);
  font-weight: 600;
  letter-spacing: -0.01em;
  color: var(--text);
}
.ml__brand-sub {
  font-size: var(--fs-2xs);
  color: var(--text3);
}

.ml__nav {
  flex: 1;
  padding: var(--sp-2) var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: 2px;
  overflow-y: auto;
  min-height: 0;
}
.ml--mini .ml__nav {
  padding: var(--sp-2) 10px;
  align-items: center;
}

.ml__group {
  padding: var(--sp-3) 10px 6px;
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text3);
}
.ml__group-gap {
  height: 1px;
  width: 24px;
  margin: 10px auto;
  background: var(--sep);
}

.ml__item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  height: 36px;
  padding: 0 10px;
  border: none;
  border-radius: var(--r-btn);
  background: transparent;
  color: var(--text2);
  font-size: var(--fs-base);
  font-weight: 500;
  font-family: inherit;
  text-align: left;
  cursor: pointer;
  transition: background var(--dur-fast), color var(--dur-fast);
}
.ml__item :deep(.lb-icon) {
  color: var(--text3);
  transition: color var(--dur-fast);
}
.ml__item:hover {
  background: var(--fill);
  color: var(--text);
}
.ml__item:hover :deep(.lb-icon) {
  color: var(--text2);
}

/* 选中 = 浅蓝底 + 蓝字 + 600。不用实底蓝:那会成为一屏里最重的元素,抢走内容的注意力。 */
.ml__item--on,
.ml__item--on:hover {
  background: var(--brand-bg);
  color: var(--brand);
  font-weight: 600;
}
.ml__item--on :deep(.lb-icon),
.ml__item--on:hover :deep(.lb-icon) {
  color: var(--brand);
}

.ml--mini .ml__item {
  justify-content: center;
  width: 40px;
  height: 40px;
  padding: 0;
}

.ml__item--tall {
  height: 44px;
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
  border-radius: var(--r-pill);
  background: var(--fill);
  color: var(--text3);
  font-size: var(--fs-2xs);
  font-weight: 600;
  line-height: 18px;
  text-align: center;
}
.ml__item--on .ml__badge {
  background: var(--surface);
  color: var(--brand);
}

.ml__badge--danger,
.ml__item--on .ml__badge--danger {
  background: var(--bad-bg);
  color: var(--bad);
}

.ml__dot {
  position: absolute;
  top: 8px;
  right: 8px;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--bad);
  box-shadow: 0 0 0 2px var(--surface);
}

.ml__sync {
  flex: none;
  margin: var(--sp-3);
  padding: 10px var(--sp-3);
  border-radius: var(--r-group);
  border: 1px solid var(--sep);
  background: var(--surface2);
  display: flex;
  align-items: center;
  gap: 10px;
}
.ml__sync--mini {
  justify-content: center;
  padding: 10px 0;
  margin: var(--sp-3) 10px;
}
.ml__sync--drawer {
  margin: var(--sp-4) 2px 0;
}

.ml__sync-text {
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--text);
}
.ml__sync-title {
  font-weight: 500;
}

.ml__sync-time {
  font-size: var(--fs-2xs);
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
  height: var(--header-h);
  flex: none;
  padding: 0 var(--sp-6) 0 var(--sp-4);
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  background: var(--surface);
  border-bottom: 1px solid var(--sep);
}

.ml__vsep {
  width: 1px;
  height: 18px;
  background: var(--sep);
  margin: 0 4px;
}

.ml__crumb {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  font-size: var(--fs-base);
}

.ml__crumb-group {
  color: var(--text3);
}
.ml__crumb-link {
  color: var(--text3);
  cursor: pointer;
}
.ml__crumb-link:hover {
  color: var(--text);
}

.ml__crumb-sep {
  color: var(--text3);
  opacity: 0.7;
}

.ml__crumb-page {
  font-weight: 600;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ml__tools {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin-left: auto;
}

.ml__iconbtn {
  width: 34px;
  height: 34px;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: var(--r-btn);
  background: transparent;
  color: var(--text2);
  cursor: pointer;
  transition: background var(--dur-fast), color var(--dur-fast);
}
.ml__iconbtn:hover {
  background: var(--fill);
  color: var(--text);
}

.ml__user {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  height: 36px;
  padding: 0 8px 0 4px;
  border: 1px solid transparent;
  border-radius: var(--r-btn);
  background: transparent;
  color: var(--text);
  font-family: inherit;
  cursor: pointer;
  transition: background var(--dur-fast), border-color var(--dur-fast);
}
.ml__user:hover {
  background: var(--fill);
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
  font-size: var(--fs-2xs);
  font-weight: 600;
}

.ml__username {
  font-size: var(--fs-sm);
  font-weight: 500;
}
.ml__user-caret {
  color: var(--text3);
}

.ml__menu-item {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 132px;
}
.ml__menu-check {
  margin-left: auto;
  color: var(--brand);
}
.ml__menu-who {
  display: flex;
  flex-direction: column;
  padding: 2px 0;
}
.ml__menu-name {
  font-size: var(--fs-base);
  font-weight: 600;
  color: var(--text);
}
.ml__menu-role {
  font-size: var(--fs-xs);
  color: var(--text3);
}

.ml__content {
  width: 100%;
  max-width: var(--content-max);
  padding: var(--page-pad) var(--page-pad) var(--sp-12);
  box-sizing: border-box;
  min-width: 0;
}

@media (max-width: 767px) {
  .ml__header {
    padding: 0 var(--sp-3) 0 var(--sp-2);
  }
  .ml__content {
    padding: var(--sp-4) var(--sp-4) var(--sp-8);
  }
}
</style>
