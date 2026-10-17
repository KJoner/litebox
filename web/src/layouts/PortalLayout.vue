<script setup lang="ts">
import { computed } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { usePortalStore } from '@/stores/portal'
import { useThemeStore, type ThemePreference } from '@/stores/theme'
import { LbIcon, type LbIconName } from '@/components/lb'

/**
 * 用户门户布局(UI 2.0)。实底顶栏 + 横向栏目导航,内容区居中 960 宽 ——
 * 栏目只有五个,而且用户多半在手机上打开,侧边栏会吃掉一半宽度。
 *
 * 门户与后台是两套界面语言:后台是高密度表格、给一个人一天看十次;
 * 门户是低密度单栏、给十个人一个月看两次。同一套令牌,不同的密度与词汇。
 * 门户里不出现技术字段(UUID、hash、rev、sha256),也不出现管理员才懂的词。
 *
 * 栏目是下划线导航(与后台的页面级 Tabs 同一种形态);「安全设置」并进栏目,
 * 主题与退出收进右上角的用户菜单。窄屏下栏目换到第二行、可横向滚动。
 */
const portal = usePortalStore()
const themeStore = useThemeStore()
const router = useRouter()
const route = useRoute()

const selected = computed(() => route.name as string)

const menuItems: { key: string; label: string; icon: LbIconName }[] = [
  { key: 'portal-dashboard', label: '概览', icon: 'layout-grid' },
  { key: 'portal-subscription', label: '我的订阅', icon: 'link' },
  { key: 'portal-nodes', label: '我的节点', icon: 'server' },
  { key: 'portal-traffic', label: '我的流量', icon: 'activity' },
  { key: 'portal-security', label: '安全设置', icon: 'shield' },
]

/**
 * 强制改密期间只留「安全设置」。其余页面的接口一律 403,
 * 把它们摆在那儿只会换来一句「没有权限」—— 而用户此刻该做的只有一件事。
 */
const mustChange = computed(() => !!portal.identity?.must_change_password)
const visibleItems = computed(() =>
  mustChange.value ? menuItems.filter((m) => m.key === 'portal-security') : menuItems,
)

async function go(key: string) {
  if (key !== route.name) await router.push({ name: key })
}

const themeOptions: { key: ThemePreference; label: string; icon: LbIconName }[] = [
  { key: 'light', label: '浅色', icon: 'sun' },
  { key: 'dark', label: '深色', icon: 'moon' },
  { key: 'system', label: '跟随系统', icon: 'monitor' },
]

async function onUserMenu({ key }: { key: string | number }) {
  if (key === 'logout') {
    try {
      await portal.logout()
      message.success('已退出登录')
    } catch {
      message.warning('退出登录时出错,已在本地清除状态')
    }
    await router.replace({ name: 'portal-login' })
    return
  }
  if (key === 'security') {
    await go('portal-security')
    return
  }
  const t = themeOptions.find((o) => `theme-${o.key}` === key)
  if (t) themeStore.setPreference(t.key)
}

/**
 * 头像缩写。中文名取一个字、拉丁名取两个字母 ——
 * 「陈明」取两字就是整个名字,和旁边的姓名完全重复。
 */
const initials = computed(() => {
  const s = portal.identity?.display_name || portal.identity?.username || '?'
  return /[一-龥]/.test(s[0]) ? s[0] : s.slice(0, 2).toUpperCase()
})
</script>

<template>
  <div class="pl">
    <header class="pl__header">
      <div class="pl__bar">
        <div class="pl__brand" @click="go(mustChange ? 'portal-security' : 'portal-dashboard')">
          <span class="pl__logo">LB</span>
          <span class="pl__brand-name">用户中心</span>
        </div>

        <nav class="pl__nav" aria-label="栏目">
          <button
            v-for="m in visibleItems"
            :key="m.key"
            type="button"
            class="pl__item"
            :class="{ 'pl__item--on': selected === m.key }"
            :aria-current="selected === m.key ? 'page' : undefined"
            @click="go(m.key)"
          >
            <LbIcon :name="m.icon" :size="15" />
            {{ m.label }}
          </button>
        </nav>

        <a-dropdown :trigger="['click']" placement="bottomRight">
          <button type="button" class="pl__user" aria-label="账户菜单">
            <span class="pl__avatar">{{ initials }}</span>
            <span class="pl__name">{{ portal.identity?.display_name }}</span>
            <LbIcon name="chevron-down" :size="14" class="pl__caret" />
          </button>
          <template #overlay>
            <a-menu :selected-keys="[`theme-${themeStore.preference}`]" @click="onUserMenu">
              <a-menu-item-group>
                <template #title>
                  <div class="pl__who">
                    <span class="pl__who-name">{{ portal.identity?.display_name }}</span>
                    <span class="pl__who-sub">{{ portal.identity?.username }}</span>
                  </div>
                </template>
              </a-menu-item-group>
              <a-menu-divider />
              <a-menu-item key="security">
                <span class="pl__menu-item"><LbIcon name="shield" :size="15" />安全设置</span>
              </a-menu-item>
              <a-menu-divider />
              <a-menu-item-group title="外观">
                <a-menu-item v-for="o in themeOptions" :key="`theme-${o.key}`">
                  <span class="pl__menu-item">
                    <LbIcon :name="o.icon" :size="15" />
                    {{ o.label }}
                    <LbIcon v-if="themeStore.preference === o.key" name="check" :size="14" class="pl__menu-check" />
                  </span>
                </a-menu-item>
              </a-menu-item-group>
              <a-menu-divider />
              <a-menu-item key="logout">
                <span class="pl__menu-item"><LbIcon name="log-out" :size="15" />退出登录</span>
              </a-menu-item>
            </a-menu>
          </template>
        </a-dropdown>
      </div>
    </header>

    <main id="lb-main" class="pl__content">
      <RouterView />
    </main>

    <footer class="pl__footer">需要调整流量或有效期,请联系管理员。</footer>
  </div>
</template>

<style scoped>
.pl {
  display: flex;
  flex-direction: column;
  min-height: 100vh;
  background: var(--bg);
}

/* 顶栏吸附在视口上,不跟着内容滚 —— 门户多在手机上用,
   往下翻两屏之后要能直接换栏目,而不是先滚回顶部。 */
.pl__header {
  position: sticky;
  top: 0;
  z-index: 20;
  background: var(--surface);
  border-bottom: 1px solid var(--sep);
}

.pl__bar {
  display: flex;
  align-items: center;
  gap: var(--sp-6);
  max-width: 1008px;
  height: var(--header-h);
  margin: 0 auto;
  padding: 0 var(--sp-6);
  box-sizing: border-box;
}

.pl__brand {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: none;
  cursor: pointer;
}

.pl__logo {
  width: 30px;
  height: 30px;
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

.pl__brand-name {
  font-size: var(--fs-md);
  font-weight: 600;
  letter-spacing: -0.01em;
}

/* 栏目:下划线导航,高度与顶栏一致,选中项底部 2px 品牌色。 */
.pl__nav {
  display: flex;
  align-self: stretch;
  gap: var(--sp-1);
  flex: 1;
  min-width: 0;
  overflow-x: auto;
  scrollbar-width: none;
}
.pl__nav::-webkit-scrollbar {
  display: none;
}

.pl__item {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  flex: none;
  padding: 0 10px;
  border: none;
  background: transparent;
  color: var(--text3);
  font-size: var(--fs-base);
  font-weight: 500;
  font-family: inherit;
  cursor: pointer;
  transition: color var(--dur-fast);
}
.pl__item:hover {
  color: var(--text);
}
.pl__item--on {
  color: var(--text);
  font-weight: 600;
}
.pl__item--on::after {
  content: '';
  position: absolute;
  left: 10px;
  right: 10px;
  bottom: -1px;
  height: 2px;
  border-radius: 2px 2px 0 0;
  background: var(--brand);
}
.pl__item :deep(.lb-icon) {
  color: var(--text3);
}
.pl__item--on :deep(.lb-icon) {
  color: var(--brand);
}

.pl__user {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  flex: none;
  height: 36px;
  padding: 0 8px 0 4px;
  border: none;
  border-radius: var(--r-btn);
  background: transparent;
  color: var(--text);
  font-family: inherit;
  cursor: pointer;
  transition: background var(--dur-fast);
}
.pl__user:hover {
  background: var(--fill);
}

.pl__avatar {
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

.pl__name {
  max-width: 140px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--fs-sm);
  font-weight: 500;
}
.pl__caret {
  color: var(--text3);
}

.pl__who {
  display: flex;
  flex-direction: column;
  padding: 2px 0;
}
.pl__who-name {
  font-size: var(--fs-base);
  font-weight: 600;
  color: var(--text);
}
.pl__who-sub {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.pl__menu-item {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 140px;
}
.pl__menu-check {
  margin-left: auto;
  color: var(--brand);
}

.pl__content {
  flex: 1;
  width: 100%;
  max-width: 1008px;
  margin: 0 auto;
  padding: var(--sp-6) var(--sp-6) var(--sp-12);
  box-sizing: border-box;
  animation: lb-fadeup var(--dur) var(--ease);
}

.pl__footer {
  padding: var(--sp-4);
  text-align: center;
  font-size: var(--fs-xs);
  color: var(--text3);
}

/* 门户的主战场是手机,不是「适配一下」。 */
@media (max-width: 767px) {
  .pl__bar {
    flex-wrap: wrap;
    height: auto;
    gap: 0 var(--sp-3);
    padding: 0 var(--sp-3);
  }
  .pl__brand {
    height: 52px;
  }
  .pl__user {
    margin-left: auto;
  }
  .pl__name {
    display: none;
  }
  .pl__nav {
    order: 3;
    flex: 1 1 100%;
    height: 44px;
    margin: 0 calc(-1 * var(--sp-3));
    padding: 0 var(--sp-1);
    border-top: 1px solid var(--sep2);
  }
  .pl__item {
    padding: 0 10px;
  }
  .pl__content {
    padding: var(--sp-4) var(--sp-4) var(--sp-8);
  }
}
</style>
