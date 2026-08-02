<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type AuditLog } from '@/api/client'

const logs = ref<AuditLog[]>([])
const loading = ref(true)

const columns = [
  { title: '时间', dataIndex: 'created_at', key: 'created_at', width: 200 },
  { title: '操作', dataIndex: 'action', key: 'action', width: 200 },
  { title: '结果', dataIndex: 'succeeded', key: 'succeeded', width: 100 },
  { title: '来源 IP', dataIndex: 'client_ip', key: 'client_ip', width: 160 },
  { title: '详情', dataIndex: 'detail', key: 'detail' },
]

async function load() {
  loading.value = true
  try {
    const result = await api.auditLogs(100, 0)
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
  <a-card>
    <template #extra>
      <a-button :loading="loading" @click="load">刷新</a-button>
    </template>
    <a-table
      :columns="columns"
      :data-source="logs"
      :loading="loading"
      row-key="id"
      size="middle"
      :pagination="{ pageSize: 20, showSizeChanger: false }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'succeeded'">
          <a-tag :color="record.succeeded ? 'green' : 'red'">
            {{ record.succeeded ? '成功' : '失败' }}
          </a-tag>
        </template>
      </template>
    </a-table>
  </a-card>
</template>
