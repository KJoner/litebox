<script setup lang="ts">
import { computed } from 'vue'
import type { NodeStatus, UserStatus } from '@/api/client'

const props = defineProps<{
  status: UserStatus | NodeStatus | string
  kind: 'user' | 'node' | 'deploy'
}>()

// 状态永远同时带文字与颜色 —— 颜色不单独承载含义。
const userStatuses: Record<string, { text: string; color: string }> = {
  ACTIVE: { text: '正常', color: 'green' },
  DISABLED: { text: '已停用', color: 'default' },
  EXPIRED: { text: '已过期', color: 'orange' },
  QUOTA_EXCEEDED: { text: '流量用尽', color: 'red' },
  DEPLOY_PENDING: { text: '待部署', color: 'blue' },
  DEPLOY_FAILED: { text: '部署失败', color: 'red' },
}

const nodeStatuses: Record<string, { text: string; color: string }> = {
  PENDING: { text: '待初始化', color: 'default' },
  ONLINE: { text: '在线', color: 'green' },
  OFFLINE: { text: '离线', color: 'orange' },
  DISABLED: { text: '已禁用', color: 'default' },
  DEPLOY_FAILED: { text: '部署失败', color: 'red' },
}

const deployStatuses: Record<string, { text: string; color: string }> = {
  SUCCESS: { text: '成功', color: 'green' },
  FAILED: { text: '失败', color: 'red' },
  ROLLED_BACK: { text: '已回滚', color: 'orange' },
  RUNNING: { text: '进行中', color: 'blue' },
  SKIPPED: { text: '已跳过', color: 'default' },
}

const meta = computed(() => {
  const table =
    props.kind === 'user' ? userStatuses : props.kind === 'node' ? nodeStatuses : deployStatuses
  return table[props.status] ?? { text: props.status, color: 'default' }
})
</script>

<template>
  <a-tag :color="meta.color">{{ meta.text }}</a-tag>
</template>
