<script setup lang="ts">
import { computed } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { usePortalStore } from '@/stores/portal'
import { useThemeStore } from '@/stores/theme'
import { LbIcon } from '@/components/lb'

/**
 * 用户门户布局。毛玻璃顶栏 + 分段控件式导航 —— 栏目只有五个,
 * 而且用户多半在手机上打开,侧边栏会吃掉一半宽度。
 *
 * 门户与后台是两套界面语言:后台是高密度表格、给一个人一天看十次;
 * 门户是低密度单栏、给十个人一个月看两次。同一套 Token,不同的密度与词汇。
 * 门户里不出现技术字段(UUID、hash、rev、sha256),也不出现管理员才懂的词。
 *
 * V18:顶栏 52px 毛玻璃,左 Logo + 「用户中心」,中间四个导航项是分段控件,
 * 右侧用户名 + 安全设置 + 退出;内容区 max-width 960。窄屏下分段导航横向可滚。
 */
const portal = usePortalStore()
const themeStore = useThemeStore()
const router = useRouter()
const route = useRoute()

const selected = computed(() => route.name as string)

const menuItems = [
  { key: 'portal-dashboard', label: '概览' },
  { key: 'portal-subscription', label: '我的订阅' },
  { key: 'portal-nodes', label: '我的节点' },
  { key: 'portal-traffic', label: '我的流量' },
]

/**
 * 强制改密期间只留「安全设置」。其余页面的接口一律 403,
 * 把它们摆在那儿只会换来一句「没有权限」—— 而用户此刻该做的只有一件事。
 */
const mustChange = computed(() => !!portal.identity?.must_change_password)
const visibleItems = computed(() => (mustChange.value ? [] : menuItems))

async function go(key: string) {
  if (key !== route.name) await router.push({ name: key })
}

async function onLogout() {
  try {
    await portal.logout()
    message.success('已退出登录')
  } catch {
    message.warning('退出登录时出错,已在本地清除状态')
  }
  await router.replace({ name: 'portal-login' })
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
      <div class="pl__brand" @click="go(mustChange ? 'portal-security' : 'portal-dashboard')">
        <span class="pl__logo">LB</span>
        <span class="pl__brand-name">用户中心</span>
      </div>

      <nav v-if="visibleItems.length" class="pl__nav lb-seg lb-seg--sm" aria-label="栏目">
        <button
          v-for="m in visibleItems"
          :key="m.key"
          type="button"
          class="lb-seg__item pl__item"
          :class="{ 'lb-seg__item--on': selected === m.key }"
          :aria-current="selected === m.key ? 'page' : undefined"
          @click="go(m.key)"
        >
          {{ m.label }}
        </button>
      </nav>
      <span v-else class="pl__nav-spacer" />

      <div class="pl__user">
        <button
          type="button"
          class="pl__iconbtn"
          :aria-label="themeStore.mode === 'dark' ? '切换到浅色' : '切换到深色'"
          :title="themeStore.mode === 'dark' ? '切换到浅色' : '切换到深色'"
          @click="themeStore.toggle()"
        >
          <LbIcon :name="themeStore.mode === 'dark' ? 'sun' : 'moon'" :size="16" />
        </button>
        <span class="pl__avatar">{{ initials }}</span>
        <span class="pl__name">{{ portal.identity?.display_name }}</span>
        <button
          type="button"
          class="pl__ghost"
          :class="{ 'pl__ghost--on': selected === 'portal-security' }"
          @click="go('portal-security')"
        >
          安全设置
        </button>
        <button type="button" class="pl__ghost pl__ghost--brand" @click="onLogout">退出</button>
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
  display: flex;
  align-items: center;
  gap: 16px;
  height: 52px;
  padding: 0 24px;
  background: var(--glass);
  backdrop-filter: saturate(180%) blur(20px);
  -webkit-backdrop-filter: saturate(180%) blur(20px);
  border-bottom: 1px solid var(--sep);
}

.pl__brand {
  display: flex;
  align-items: center;
  gap: 10px;
  flex: none;
  cursor: pointer;
}

.pl__logo {
  width: 28px;
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 9px;
  background: linear-gradient(145deg, var(--brand), var(--brand-hover));
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  letter-spacing: -0.02em;
  box-shadow: 0 2px 6px rgba(37, 99, 184, 0.3);
}

.pl__brand-name {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: -0.02em;
}

.pl__nav {
  margin: 0 auto;
  max-width: 100%;
  overflow-x: auto;
  scrollbar-width: none;
}
.pl__nav::-webkit-scrollbar {
  display: none;
}
.pl__nav-spacer {
  flex: 1;
}

.pl__item {
  padding: 0 14px;
}

.pl__user {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
}

.pl__iconbtn {
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
.pl__iconbtn:hover {
  background: var(--fill);
  color: var(--text);
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
  font-size: 11px;
  font-weight: 600;
}

.pl__name {
  font-size: 13px;
  color: var(--text2);
}

.pl__ghost {
  height: 28px;
  padding: 0 12px;
  border: none;
  border-radius: var(--r-pill);
  background: transparent;
  color: var(--text2);
  font-size: 13px;
  font-family: inherit;
  cursor: pointer;
  transition: background 0.2s, color 0.2s;
}
.pl__ghost:hover {
  background: var(--fill);
  color: var(--text);
}
.pl__ghost--on {
  background: var(--fill);
  color: var(--text);
  font-weight: 600;
}
.pl__ghost--brand {
  color: var(--brand);
}
.pl__ghost--brand:hover {
  color: var(--brand);
}

.pl__content {
  flex: 1;
  width: 100%;
  max-width: 960px;
  margin: 0 auto;
  padding: 28px 24px 48px;
  box-sizing: border-box;
  animation: lb-fadeup 0.4s var(--ease);
}

.pl__footer {
  padding: 16px;
  text-align: center;
  font-size: 12px;
  color: var(--text3);
}

/* 门户的主战场是手机,不是「适配一下」。 */
@media (max-width: 767px) {
  .pl__header {
    height: auto;
    min-height: 52px;
    flex-wrap: wrap;
    gap: 8px 12px;
    padding: 8px 12px;
  }

  .pl__brand-name,
  .pl__name {
    display: none;
  }

  .pl__nav {
    order: 3;
    width: 100%;
    margin: 0;
  }

  .pl__user {
    margin-left: auto;
  }

  .pl__item {
    line-height: 32px;
  }

  .pl__content {
    padding: 16px 16px 32px;
  }
}
</style>
