<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type DashboardSummary } from '@/api/client'

const summary = ref<DashboardSummary | null>(null)
const loading = ref(true)

function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** i).toFixed(i === 0 ? 0 : 2)} ${units[i]}`
}

onMounted(async () => {
  try {
    summary.value = await api.dashboardSummary()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载仪表盘数据失败')
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <a-spin :spinning="loading">
    <a-row :gutter="[16, 16]">
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="用户总数" :value="summary?.user_total ?? 0" />
          <template #actions>
            <span class="hint">启用中 {{ summary?.user_active ?? 0 }}</span>
          </template>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="节点总数" :value="summary?.node_total ?? 0" />
          <template #actions>
            <span class="hint">在线 {{ summary?.node_online ?? 0 }}</span>
          </template>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="今日流量" :value="formatBytes(summary?.traffic_today ?? 0)" />
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="本月流量" :value="formatBytes(summary?.traffic_month ?? 0)" />
        </a-card>
      </a-col>

      <a-col :xs="24" :sm="8">
        <a-card>
          <a-statistic
            title="超额用户"
            :value="summary?.quota_exceeded ?? 0"
            :value-style="{ color: (summary?.quota_exceeded ?? 0) > 0 ? '#cf1322' : undefined }"
          />
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="8">
        <a-card>
          <a-statistic title="即将到期" :value="summary?.expiring_soon ?? 0" />
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="8">
        <a-card>
          <a-statistic
            title="部署失败"
            :value="summary?.failed_deploys ?? 0"
            :value-style="{ color: (summary?.failed_deploys ?? 0) > 0 ? '#cf1322' : undefined }"
          />
        </a-card>
      </a-col>
    </a-row>

    <a-alert
      class="phase-note"
      type="info"
      show-icon
      message="Phase 1:项目骨架"
      description="用户管理、节点管理与流量统计将在 Phase 2 至 Phase 4 接入,当前指标除用户数与节点数外均为占位值。"
    />
  </a-spin>
</template>

<style scoped>
.hint {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.phase-note {
  margin-top: 24px;
}
</style>
