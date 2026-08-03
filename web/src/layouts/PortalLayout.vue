<script setup lang="ts">
import { computed } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { usePortalStore } from '@/stores/portal'

// 用户门户用横向菜单而不是后台的侧边栏:栏目只有五个,
// 而且用户多半在手机上打开,侧边栏会吃掉一半宽度。
const portal = usePortalStore()
const router = useRouter()
const route = useRoute()

const selectedKeys = computed(() => [route.name as string])

const menuItems = [
  { key: 'portal-dashboard', label: '概览' },
  { key: 'portal-subscription', label: '我的订阅' },
  { key: 'portal-nodes', label: '我的节点' },
  { key: 'portal-traffic', label: '我的流量' },
  { key: 'portal-security', label: '安全设置' },
]

async function onMenuClick({ key }: { key: string }) {
  await router.push({ name: key })
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
</script>

<template>
  <a-layout class="portal-layout">
    <a-layout-header class="portal-header">
      <div class="brand">LiteBox</div>
      <a-menu
        class="portal-menu"
        theme="dark"
        mode="horizontal"
        :selected-keys="selectedKeys"
        :items="menuItems"
        @click="onMenuClick"
      />
      <a-space class="portal-user">
        <span class="name">{{ portal.identity?.display_name }}</span>
        <a-button type="text" ghost @click="onLogout">退出</a-button>
      </a-space>
    </a-layout-header>

    <a-layout-content class="portal-content">
      <RouterView />
    </a-layout-content>

    <a-layout-footer class="portal-footer">
      LiteBox · 如需调整流量或有效期,请联系管理员
    </a-layout-footer>
  </a-layout>
</template>

<style scoped>
.portal-layout {
  min-height: 100vh;
}

.portal-header {
  display: flex;
  align-items: center;
  gap: 24px;
  padding: 0 16px;
}

.brand {
  color: #fff;
  font-size: 18px;
  font-weight: 600;
  letter-spacing: 1px;
  flex: none;
}

.portal-menu {
  flex: 1;
  min-width: 0;
}

.portal-user {
  flex: none;
}

.portal-user .name {
  color: rgb(255 255 255 / 75%);
}

.portal-content {
  max-width: 1000px;
  width: 100%;
  margin: 24px auto;
  padding: 0 16px;
}

.portal-footer {
  text-align: center;
  color: rgb(0 0 0 / 45%);
}
</style>
