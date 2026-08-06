<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { usePortalStore } from '@/stores/portal'

/**
 * 用户门户布局。白底顶栏 + 横向菜单 —— 栏目只有五个,
 * 而且用户多半在手机上打开,侧边栏会吃掉一半宽度。
 *
 * 门户与后台是两套界面语言:后台是高密度表格、给一个人一天看十次;
 * 门户是低密度单栏、给十个人一个月看两次。同一套 Token,不同的密度与词汇。
 * 门户里不出现技术字段(UUID、hash、rev、sha256),也不出现管理员才懂的词。
 */
const portal = usePortalStore()
const router = useRouter()
const route = useRoute()

const selected = computed(() => route.name as string)

const menuItems = [
  { key: 'portal-dashboard', label: '概览' },
  { key: 'portal-subscription', label: '我的订阅' },
  { key: 'portal-nodes', label: '我的节点' },
  { key: 'portal-traffic', label: '我的流量' },
  { key: 'portal-security', label: '安全设置' },
]

/**
 * 强制改密期间只留「安全设置」。其余页面的接口一律 403,
 * 把它们摆在那儿只会换来一句「没有权限」—— 而用户此刻该做的只有一件事。
 */
const visibleItems = computed(() =>
  portal.identity?.must_change_password
    ? menuItems.filter((m) => m.key === 'portal-security')
    : menuItems,
)

const drawerOpen = ref(false)

async function go(key: string) {
  drawerOpen.value = false
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
      <a-button class="pl__burger lb-touch-target" type="text" @click="drawerOpen = true">☰</a-button>
      <span class="pl__logo">LiteBox</span>

      <nav class="pl__nav">
        <button
          v-for="m in visibleItems"
          :key="m.key"
          class="pl__item"
          :class="{ 'pl__item--on': selected === m.key }"
          @click="go(m.key)"
        >
          {{ m.label }}
        </button>
      </nav>

      <div class="pl__user">
        <span class="pl__avatar">{{ initials }}</span>
        <span class="pl__name">{{ portal.identity?.display_name }}</span>
        <a-button type="text" size="small" @click="onLogout">退出</a-button>
      </div>
    </header>

    <main id="lb-main" class="pl__content">
      <RouterView />
    </main>

    <footer class="pl__footer">需要调整流量或有效期,请联系管理员。</footer>
  </div>

  <a-drawer v-model:open="drawerOpen" placement="left" :width="240" :body-style="{ padding: '8px' }">
    <template #title><span class="pl__logo">LiteBox</span></template>
    <button
      v-for="m in visibleItems"
      :key="m.key"
      class="pl__item pl__item--tall"
      :class="{ 'pl__item--on': selected === m.key }"
      @click="go(m.key)"
    >
      {{ m.label }}
    </button>
  </a-drawer>
</template>

<style scoped>
.pl {
  display: flex;
  flex-direction: column;
  min-height: 100vh;
  background: #f6f7f9;
}

.pl__header {
  display: flex;
  align-items: center;
  gap: 16px;
  height: 52px;
  padding: 0 20px;
  background: #fff;
  border-bottom: 1px solid #e3e6ea;
}

.pl__burger {
  display: none;
}

.pl__logo {
  font-size: 15px;
  font-weight: 600;
  letter-spacing: -0.01em;
  flex: none;
}

.pl__nav {
  display: flex;
  gap: 2px;
  flex: 1;
  min-width: 0;
  overflow-x: auto;
}

.pl__item {
  flex: none;
  height: 30px;
  padding: 0 12px;
  border: none;
  border-radius: 4px;
  background: transparent;
  color: #576070;
  font-size: 13px;
  font-family: inherit;
  cursor: pointer;
}

.pl__item:hover {
  background: #f1f3f5;
}

.pl__item--on {
  background: #eef4fc;
  color: #1d4f96;
  font-weight: 500;
}

.pl__item--tall {
  display: block;
  width: 100%;
  height: 48px;
  text-align: left;
  font-size: 14px;
}

.pl__user {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
}

.pl__avatar {
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

.pl__name {
  font-size: 12.5px;
  color: #576070;
}

.pl__content {
  flex: 1;
  width: 100%;
  max-width: 840px;
  margin: 0 auto;
  padding: 20px 16px 32px;
}

.pl__footer {
  padding: 16px;
  text-align: center;
  font-size: 11.5px;
  color: #6b7480;
}

/* 门户的主战场是手机,不是「适配一下」。 */
@media (max-width: 767px) {
  .pl__burger {
    display: inline-flex;
  }

  .pl__nav,
  .pl__name {
    display: none;
  }

  .pl__logo {
    flex: 1;
  }

  .pl__content {
    padding: 12px 12px 24px;
  }
}
</style>
