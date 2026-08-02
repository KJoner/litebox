<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api, ApiError, type DashboardSummary, type DeploymentRecord, type TrafficStatus } from '@/api/client'
import { formatBytes, formatTime } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'

const router = useRouter()
const summary = ref<DashboardSummary | null>(null)
const failedDeploys = ref<DeploymentRecord[]>([])
const trafficStatus = ref<TrafficStatus | null>(null)
const loading = ref(true)

// 需要提醒的指标单独拎出来:数字为 0 时保持中性,不要满屏红色。
const alerts = computed(() => {
  const s = summary.value
  if (!s) return []
  const items: { label: string; value: number; to: string }[] = []
  if (s.quota_exceeded > 0) items.push({ label: '流量用尽', value: s.quota_exceeded, to: '/users' })
  if (s.expiring_soon > 0) items.push({ label: '7 天内到期', value: s.expiring_soon, to: '/users' })
  if (s.failed_deploys > 0)
    items.push({ label: '部署失败', value: s.failed_deploys, to: '/deployments' })
  return items
})

async function load() {
  loading.value = true
  try {
    const [s, deploys, status] = await Promise.all([
      api.dashboardSummary(),
      api.deployments(20),
      api.trafficStatus(),
    ])
    summary.value = s
    failedDeploys.value = deploys.items
      .filter((d) => d.status === 'FAILED' || d.status === 'ROLLED_BACK')
      .slice(0, 5)
    trafficStatus.value = status
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载仪表盘数据失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <a-spin :spinning="loading">
    <a-row :gutter="[16, 16]">
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="stat-card" @click="router.push('/users')">
          <a-statistic title="用户" :value="summary?.user_total ?? 0" />
          <div class="stat-hint">启用中 {{ summary?.user_active ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card class="stat-card" @click="router.push('/nodes')">
          <a-statistic title="节点" :value="summary?.node_total ?? 0" />
          <div class="stat-hint">在线 {{ summary?.node_online ?? 0 }}</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="今日流量" :value="formatBytes(summary?.traffic_today ?? 0)" />
          <div class="stat-hint">UTC 时区</div>
        </a-card>
      </a-col>
      <a-col :xs="24" :sm="12" :lg="6">
        <a-card>
          <a-statistic title="本月流量" :value="formatBytes(summary?.traffic_month ?? 0)" />
          <div class="stat-hint">UTC 时区</div>
        </a-card>
      </a-col>
    </a-row>

    <a-row v-if="alerts.length > 0" :gutter="[16, 16]" class="section">
      <a-col v-for="a in alerts" :key="a.label" :xs="24" :sm="8">
        <a-card class="stat-card alert-card" @click="router.push(a.to)">
          <a-statistic :title="a.label" :value="a.value" :value-style="{ color: '#cf1322' }" />
        </a-card>
      </a-col>
    </a-row>

    <a-row :gutter="[16, 16]" class="section">
      <a-col :xs="24" :lg="14">
        <a-card title="最近失败的部署" size="small">
          <template #extra>
            <a @click="router.push('/deployments')">全部记录</a>
          </template>
          <a-empty v-if="failedDeploys.length === 0" description="没有失败的部署" />
          <a-list v-else :data-source="failedDeploys" size="small">
            <template #renderItem="{ item }">
              <a-list-item>
                <a-list-item-meta>
                  <template #title>
                    <a @click="router.push(`/nodes/${item.node_id}`)">
                      节点 {{ item.node_id }} · revision {{ item.revision }}
                    </a>
                    <StatusTag :status="item.status" kind="deploy" class="tag-inline" />
                  </template>
                  <template #description>
                    <div class="deploy-error">{{ item.error_message || '(无错误信息)' }}</div>
                    <div class="deploy-time">{{ formatTime(item.started_at) }}</div>
                  </template>
                </a-list-item-meta>
              </a-list-item>
            </template>
          </a-list>
        </a-card>
      </a-col>

      <a-col :xs="24" :lg="10">
        <a-card title="流量同步" size="small">
          <a-descriptions :column="1" size="small">
            <a-descriptions-item label="上次同步">
              {{ formatTime(trafficStatus?.last_run) }}
            </a-descriptions-item>
            <a-descriptions-item label="同步失败的节点">
              <span v-if="!trafficStatus?.failing_nodes?.length">无</span>
              <div v-else>
                <div v-for="f in trafficStatus.failing_nodes" :key="f.node_id" class="sync-fail">
                  <a @click="router.push(`/nodes/${f.node_id}`)">节点 {{ f.node_id }}</a>
                  <div class="sync-fail-msg">{{ f.error }}</div>
                </div>
              </div>
            </a-descriptions-item>
          </a-descriptions>
          <a-alert
            v-if="trafficStatus?.failing_nodes?.length"
            type="warning"
            show-icon
            message="同步失败不会影响已入库的流量,但期间产生的流量要等节点恢复后才会补记。"
          />
        </a-card>
      </a-col>
    </a-row>
  </a-spin>
</template>

<style scoped>
.section {
  margin-top: 16px;
}

.stat-card {
  cursor: pointer;
  transition: box-shadow 0.2s;
}

.stat-card:hover {
  box-shadow: 0 2px 8px rgb(0 0 0 / 10%);
}

.stat-hint {
  margin-top: 4px;
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.tag-inline {
  margin-left: 8px;
}

.deploy-error {
  color: rgb(0 0 0 / 65%);
  word-break: break-all;
}

.deploy-time {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  margin-top: 2px;
}

.sync-fail {
  margin-bottom: 8px;
}

.sync-fail-msg {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  word-break: break-all;
}
</style>
