<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type AuditLog } from '@/api/client'
import { formatTime } from '@/utils/format'

const logs = ref<AuditLog[]>([])
const loading = ref(true)
const keyword = ref('')
const onlyFailed = ref(false)

// 动作名从英文常量映射成中文,列表才读得懂。
const actionNames: Record<string, string> = {
  'admin.login': '管理员登录',
  'admin.login_failed': '登录失败',
  'admin.logout': '管理员注销',
  'admin.change_password': '修改密码',
  'node.create': '新增节点',
  'node.update': '修改节点',
  'node.delete': '删除节点',
  'node.enable': '启用节点',
  'node.disable': '禁用节点',
  'node.probe': '探测节点',
  'node.dest_check': '检测握手目标',
  'node.install': '安装 sing-box',
  'node.deploy': '部署节点',
  'node.restart': '重启节点服务',
  'node.reset_host_key': '重置主机密钥',
  'user.create': '新增用户',
  'user.update': '编辑用户',
  'user.enable': '启用用户',
  'user.disable': '停用用户',
  'user.reset_traffic': '重置流量',
  'user.regenerate_uuid': '重新生成 UUID',
  'user.regenerate_sub_token': '重新生成订阅地址',
  'user.delete': '删除用户',
  'traffic.sync': '同步流量',
}

const columns = [
  { title: '时间', key: 'time', width: 160 },
  { title: '操作', key: 'action', width: 180 },
  { title: '目标', key: 'target', width: 150 },
  { title: '结果', key: 'result', width: 90 },
  { title: '来源', key: 'ip', width: 130 },
  { title: '详情', key: 'detail' },
]

const visible = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return logs.value.filter((l) => {
    if (onlyFailed.value && l.succeeded) return false
    if (!kw) return true
    const name = actionNames[l.action] ?? l.action
    return (
      name.toLowerCase().includes(kw) ||
      l.action.toLowerCase().includes(kw) ||
      l.target_id.toLowerCase().includes(kw) ||
      l.detail.toLowerCase().includes(kw)
    )
  })
})

async function load() {
  loading.value = true
  try {
    const result = await api.auditLogs({ limit: 200 })
    logs.value = result.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载审计日志失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <a-card title="审计日志">
    <template #extra>
      <a-space>
        <a-input v-model:value="keyword" placeholder="搜索操作、目标或详情" allow-clear style="width: 220px" />
        <a-checkbox v-model:checked="onlyFailed">只看失败</a-checkbox>
        <a-button :loading="loading" @click="load">刷新</a-button>
      </a-space>
    </template>

    <a-table
      :columns="columns"
      :data-source="visible"
      :loading="loading"
      row-key="id"
      size="middle"
      :pagination="{ pageSize: 20, showSizeChanger: false }"
      :scroll="{ x: 900 }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'time'">
          {{ formatTime(record.created_at) }}
        </template>
        <template v-else-if="column.key === 'action'">
          <div>{{ actionNames[record.action] ?? record.action }}</div>
          <div class="raw-action">{{ record.action }}</div>
        </template>
        <template v-else-if="column.key === 'target'">
          <span v-if="!record.target_id" class="muted">—</span>
          <span v-else class="mono">{{ record.target_id }}</span>
        </template>
        <template v-else-if="column.key === 'result'">
          <a-tag :color="record.succeeded ? 'green' : 'red'">
            {{ record.succeeded ? '成功' : '失败' }}
          </a-tag>
        </template>
        <template v-else-if="column.key === 'ip'">
          <span class="mono">{{ record.client_ip || '—' }}</span>
        </template>
        <template v-else-if="column.key === 'detail'">
          <span class="detail">{{ record.detail || '—' }}</span>
        </template>
      </template>
    </a-table>
  </a-card>
</template>

<style scoped>
.raw-action {
  color: rgb(0 0 0 / 45%);
  font-size: 11px;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
}

.muted {
  color: rgb(0 0 0 / 45%);
}

.detail {
  word-break: break-all;
}
</style>
