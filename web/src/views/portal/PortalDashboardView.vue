<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { portalApi, ApiError, type PortalDashboard, type PublicAdjustment } from '@/api/client'
import { formatBytes, formatQuota, formatTime } from '@/utils/format'

const data = ref<PortalDashboard | null>(null)
const adjustments = ref<PublicAdjustment[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    const [d, adj] = await Promise.all([
      portalApi.dashboard(),
      // 调整记录是附加信息,取不到不该让整个首页空着。
      portalApi.adjustments().catch(() => ({ items: [] as PublicAdjustment[] })),
    ])
    data.value = d
    adjustments.value = adj.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载失败')
  } finally {
    loading.value = false
  }
}

function statusColor(status: string): string {
  if (status === 'ACTIVE') return 'green'
  if (status === 'DEPLOY_PENDING') return 'blue'
  return 'red'
}

function tierColor(code: string): string {
  if (code === 'root') return 'red'
  if (code === 'vip') return 'gold'
  return 'default'
}

// 进度条颜色随用量加深。80% 之前保持绿色,免得平时就一片橙红,
// 真的快用完时反而没人当回事。
function quotaStatus(percent: number): 'normal' | 'exception' {
  return percent >= 100 ? 'exception' : 'normal'
}

onMounted(load)
</script>

<template>
  <a-spin :spinning="loading">
    <template v-if="data">
      <a-alert
        v-for="(alert, i) in data.alerts"
        :key="i"
        :type="alert.level === 'error' ? 'error' : 'warning'"
        :message="alert.message"
        show-icon
        class="alert"
      />

      <a-card class="card">
        <div class="head">
          <div>
            <span class="name">{{ data.display_name }}</span>
            <a-tag :color="tierColor(data.tier_code)" class="tier">{{ data.tier_name }}</a-tag>
          </div>
          <a-tag :color="statusColor(data.status)">{{ data.status_text }}</a-tag>
        </div>
        <div v-if="!data.serviceable" class="reason">{{ data.reason }}</div>
      </a-card>

      <a-card title="流量" class="card">
        <a-row :gutter="[16, 16]">
          <a-col :xs="12" :sm="8">
            <a-statistic title="已用" :value="formatBytes(data.used_total)" />
          </a-col>
          <a-col :xs="12" :sm="8">
            <a-statistic title="总额度" :value="formatQuota(data.quota_bytes)" />
          </a-col>
          <a-col :xs="12" :sm="8">
            <!-- 额度为 0 时显示"不限量",不做除零也不显示一个假的百分比。 -->
            <a-statistic
              title="剩余"
              :value="data.used_percent === null ? '不限量' : formatBytes(data.remaining)"
            />
          </a-col>
        </a-row>
        <a-progress
          v-if="data.used_percent !== null"
          :percent="Math.min(data.used_percent, 100)"
          :status="quotaStatus(data.used_percent)"
          class="progress"
        />
        <div class="detail">
          <span>上行 {{ formatBytes(data.used_uplink) }}</span>
          <span>下行 {{ formatBytes(data.used_downlink) }}</span>
        </div>
      </a-card>

      <a-card title="有效期" class="card">
        <a-descriptions :column="{ xs: 1, sm: 2 }" size="small" bordered>
          <a-descriptions-item label="到期时间">
            {{ data.expires_at ? formatTime(data.expires_at) : '不过期' }}
          </a-descriptions-item>
          <a-descriptions-item label="剩余天数">
            {{ data.remaining_days === null ? '—' : `${data.remaining_days} 天` }}
          </a-descriptions-item>
          <a-descriptions-item label="最近重置">
            {{ data.last_reset_at ? formatTime(data.last_reset_at) : '从未' }}
          </a-descriptions-item>
          <a-descriptions-item label="下次流量重置">
            {{ data.next_reset_at ? formatTime(data.next_reset_at) : '不自动重置' }}
          </a-descriptions-item>
          <a-descriptions-item label="可用节点">{{ data.node_count }} 个</a-descriptions-item>
          <a-descriptions-item label="用户编号">{{ data.user_code }}</a-descriptions-item>
        </a-descriptions>
      </a-card>
      <a-card v-if="adjustments.length > 0" title="最近调整" class="card">
        <a-timeline>
          <a-timeline-item v-for="(a, i) in adjustments" :key="i">
            <div class="adj-head">
              <span>{{ a.action_text }}</span>
              <span v-if="a.quota_delta_bytes" class="adj-delta">
                {{ a.quota_delta_bytes > 0 ? '+' : '-' }}{{ formatBytes(Math.abs(a.quota_delta_bytes)) }}
              </span>
              <span v-else-if="a.expiry_delta_days" class="adj-delta">
                {{ a.expiry_delta_days > 0 ? '+' : '' }}{{ a.expiry_delta_days }} 天
              </span>
            </div>
            <div v-if="a.remark" class="adj-remark">{{ a.remark }}</div>
            <div class="adj-time">{{ formatTime(a.created_at) }}</div>
          </a-timeline-item>
        </a-timeline>
      </a-card>
    </template>
  </a-spin>
</template>

<style scoped>
.card {
  margin-bottom: 16px;
}

.alert {
  margin-bottom: 16px;
}

.head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
}

.name {
  font-size: 18px;
  font-weight: 600;
}

.tier {
  margin-left: 8px;
}

.reason {
  margin-top: 8px;
  color: #cf1322;
}

.progress {
  margin-top: 12px;
}

.adj-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.adj-delta {
  color: #389e0d;
  font-variant-numeric: tabular-nums;
}

.adj-remark {
  color: rgb(0 0 0 / 65%);
  font-size: 13px;
}

.adj-time {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.detail {
  margin-top: 8px;
  display: flex;
  gap: 16px;
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
</style>
