<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const selectedKeys = computed(() => [route.name as string])
const collapsed = ref(false)

// Phase 1 只有三个页面,用户与节点管理在 Phase 3/2 接入。
const menuItems = [
  { key: 'dashboard', label: '仪表盘' },
  { key: 'audit-logs', label: '审计日志' },
  { key: 'settings', label: '设置' },
]

async function onMenuClick({ key }: { key: string }) {
  await router.push({ name: key })
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
  <a-layout class="app-layout">
    <a-layout-sider v-model:collapsed="collapsed" collapsible theme="dark">
      <div class="logo">{{ collapsed ? 'LB' : 'LiteBox' }}</div>
      <a-menu
        theme="dark"
        mode="inline"
        :selected-keys="selectedKeys"
        :items="menuItems"
        @click="onMenuClick"
      />
    </a-layout-sider>

    <a-layout>
      <a-layout-header class="app-header">
        <span class="page-title">{{ route.meta.title ?? '' }}</span>
        <a-space>
          <span class="username">{{ auth.admin?.username }}</span>
          <a-button type="text" @click="onLogout">退出登录</a-button>
        </a-space>
      </a-layout-header>

      <a-layout-content class="app-content">
        <RouterView />
      </a-layout-content>
    </a-layout>
  </a-layout>
</template>

<style scoped>
.app-layout {
  min-height: 100vh;
}

.logo {
  height: 48px;
  margin: 8px 16px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 18px;
  font-weight: 600;
  letter-spacing: 1px;
}

.app-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
  background: #fff;
  border-bottom: 1px solid #f0f0f0;
}

.page-title {
  font-size: 16px;
  font-weight: 500;
}

.username {
  color: rgb(0 0 0 / 65%);
}

.app-content {
  margin: 24px;
}
</style>
