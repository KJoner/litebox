<script setup lang="ts">
/**
 * 仪表盘的节点运行状态区域。
 *
 * 只读数据库里已有的最近采样,不主动触发 SSH 采集 —— 每次刷新页面都去连
 * 一遍全部节点,10 台机器就是 10 条 SSH 会话,而 128MB 的小鸡上这笔开销
 * 比它提供的"实时"更值得在意。后端采集周期保持 5 分钟。
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { message } from 'ant-design-vue'
import { api, ApiError, type Node, type NodeMetrics } from '@/api/client'
import { formatBytes, formatRelative } from '@/utils/format'

const router = useRouter()
const nodes = ref<Node[]>([])
const metrics = ref<Record<number, NodeMetrics>>({})
const loading = ref(false)
let timer: ReturnType<typeof setInterval> | undefined

// 采样超过这个时长就认为数据已过期。取采集周期(5 分钟)的两倍:
// 只错过一次采集很常见(节点忙、网络抖),报"过期"会天天误报。
const STALE_MS = 10 * 60 * 1000

async function load() {
  loading.value = true
  try {
    const [n, m] = await Promise.all([
      api.nodes(),
      api.nodeMetricsLatest().catch(() => ({ items: [] as NodeMetrics[] })),
    ])
    nodes.value = n.items
    metrics.value = Object.fromEntries(m.items.map((x) => [x.node_id, x]))
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载节点状态失败')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  timer = setInterval(load, 60_000)
})
onUnmounted(() => clearInterval(timer))

function metricOf(id: number): NodeMetrics | undefined {
  return metrics.value[id]
}

function isStale(m: NodeMetrics): boolean {
  const t = new Date(m.collected_at).getTime()
  return Number.isNaN(t) || Date.now() - t > STALE_MS
}

function memPercent(m: NodeMetrics): number {
  return m.mem_total_kb > 0 ? (m.mem_used_kb / m.mem_total_kb) * 100 : 0
}

// 绿到 70%、黄到 90%、再上红。128MB 的机器内存曲线本来就贴着高位走,
// 阈值定得太低会天天报警,反而没人看。
function usageColor(percent: number): string {
  if (percent >= 90) return '#cf1322'
  if (percent >= 70) return '#d46b08'
  return '#389e0d'
}

// 节点状态与监控数据是两码事:采集失败不代表代理服务离线。
// 状态永远取节点自己的 status,监控只额外标一句"数据已过期"。
function statusTag(node: Node): { color: string; text: string } {
  switch (node.status) {
    case 'ONLINE':
      return { color: 'green', text: '在线' }
    case 'DISABLED':
      return { color: 'default', text: '已停用' }
    case 'DEPLOY_FAILED':
      return { color: 'red', text: '部署失败' }
    case 'OFFLINE':
      return { color: 'orange', text: '离线' }
    default:
      return { color: 'blue', text: '待部署' }
  }
}

function formatUptime(seconds: number): string {
  if (seconds <= 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  if (days > 0) return `${days} 天 ${hours} 小时`
  const minutes = Math.floor((seconds % 3600) / 60)
  return `${hours} 小时 ${minutes} 分`
}

const hasNodes = computed(() => nodes.value.length > 0)
</script>

<template>
  <a-card title="节点运行状态" size="small">
    <template #extra>
      <a-space>
        <span class="refresh-hint">每 60 秒自动刷新</span>
        <a-button size="small" :loading="loading" @click="load">刷新</a-button>
      </a-space>
    </template>

    <a-empty v-if="!hasNodes && !loading" description="还没有节点" />

    <a-row :gutter="[16, 16]">
      <!-- 大屏 3 张、中屏 2 张、小屏 1 张 -->
      <a-col v-for="node in nodes" :key="node.id" :xs="24" :md="12" :xl="8">
        <a-card size="small" class="node-card" hoverable @click="router.push('/nodes')">
          <div class="node-head">
            <div class="node-names">
              <div class="node-name">{{ node.name }}</div>
              <div v-if="node.display_name !== node.name" class="node-display">
                对外:{{ node.display_name }}
              </div>
            </div>
            <a-tag :color="statusTag(node).color">{{ statusTag(node).text }}</a-tag>
          </div>

          <a-alert
            v-if="!metricOf(node.id)"
            type="info"
            message="暂无监控数据"
            class="metric-alert"
          />
          <a-alert
            v-else-if="isStale(metricOf(node.id)!)"
            type="warning"
            :message="`监控数据已过期（${formatRelative(metricOf(node.id)!.collected_at)}）`"
            class="metric-alert"
          />

          <template v-if="metricOf(node.id)">
            <div class="metric-row">
              <span class="metric-label">CPU</span>
              <a-progress
                :percent="Math.min(metricOf(node.id)!.cpu_percent, 100)"
                :stroke-color="usageColor(metricOf(node.id)!.cpu_percent)"
                :show-info="false"
                size="small"
                class="metric-bar"
              />
              <span class="metric-value" :style="{ color: usageColor(metricOf(node.id)!.cpu_percent) }">
                {{ metricOf(node.id)!.cpu_percent.toFixed(0) }}%
              </span>
            </div>

            <div class="metric-row">
              <span class="metric-label">内存</span>
              <a-progress
                :percent="memPercent(metricOf(node.id)!)"
                :stroke-color="usageColor(memPercent(metricOf(node.id)!))"
                :show-info="false"
                size="small"
                class="metric-bar"
              />
              <span class="metric-value" :style="{ color: usageColor(memPercent(metricOf(node.id)!)) }">
                {{ memPercent(metricOf(node.id)!).toFixed(0) }}%
              </span>
            </div>
            <div class="metric-sub">
              {{ formatBytes(metricOf(node.id)!.mem_used_kb * 1024) }} /
              {{ formatBytes(metricOf(node.id)!.mem_total_kb * 1024) }}
            </div>

            <div class="net-row">
              <span class="net">↓ {{ formatBytes(metricOf(node.id)!.net_rx_bps) }}/s</span>
              <span class="net">↑ {{ formatBytes(metricOf(node.id)!.net_tx_bps) }}/s</span>
            </div>

            <div class="foot">
              <span>运行 {{ formatUptime(metricOf(node.id)!.uptime_seconds) }}</span>
              <span>{{ formatRelative(metricOf(node.id)!.collected_at) }}</span>
            </div>
          </template>
        </a-card>
      </a-col>
    </a-row>
  </a-card>
</template>

<style scoped>
.refresh-hint {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.node-card {
  height: 100%;
}

.node-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 8px;
}

.node-names {
  min-width: 0;
}

.node-name {
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.node-display {
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.metric-alert {
  margin-bottom: 8px;
  padding: 4px 8px;
  font-size: 12px;
}

.metric-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.metric-label {
  width: 32px;
  flex: none;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}

.metric-bar {
  flex: 1;
  min-width: 0;
  margin: 0;
}

.metric-value {
  width: 40px;
  flex: none;
  text-align: right;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.metric-sub {
  margin: 2px 0 6px 40px;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
  font-variant-numeric: tabular-nums;
}

.net-row {
  display: flex;
  gap: 16px;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.foot {
  margin-top: 8px;
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}
</style>
