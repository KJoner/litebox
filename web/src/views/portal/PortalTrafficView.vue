<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { portalApi, ApiError, type PortalTraffic } from '@/api/client'
import { formatBytes } from '@/utils/format'
import TrafficChart from '@/components/TrafficChart.vue'

const data = ref<PortalTraffic | null>(null)
const loading = ref(false)
// 只有 7 与 30 两档:后端也只接受这两个值,前端多给一档只会拿到回落后的 30 天,
// 用户会以为是页面坏了。
const days = ref(30)

async function load() {
  loading.value = true
  try {
    data.value = await portalApi.traffic(days.value)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载失败')
  } finally {
    loading.value = false
  }
}

watch(days, load)
onMounted(load)

const nodeColumns = [
  { title: '节点', dataIndex: 'display_name', key: 'name' },
  { title: '上行', key: 'uplink', width: 110 },
  { title: '下行', key: 'downlink', width: 110 },
  { title: '合计', key: 'total', width: 110 },
  { title: '占比', key: 'percent', width: 140 },
]
</script>

<template>
  <a-card title="我的流量">
    <template #extra>
      <a-radio-group v-model:value="days" size="small" button-style="solid">
        <a-radio-button :value="7">最近 7 天</a-radio-button>
        <a-radio-button :value="30">最近 30 天</a-radio-button>
      </a-radio-group>
    </template>

    <a-spin :spinning="loading">
      <template v-if="data">
        <a-row :gutter="[16, 16]" class="totals">
          <a-col :xs="8"><a-statistic title="合计" :value="formatBytes(data.total)" /></a-col>
          <a-col :xs="8"><a-statistic title="上行" :value="formatBytes(data.uplink)" /></a-col>
          <a-col :xs="8"><a-statistic title="下行" :value="formatBytes(data.downlink)" /></a-col>
        </a-row>

        <TrafficChart :data="data.daily" />

        <h4 class="section">按节点</h4>
        <a-table
          :columns="nodeColumns"
          :data-source="data.by_node"
          row-key="node_id"
          size="small"
          :pagination="false"
          :scroll="{ x: 600 }"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'uplink'">
              <span class="tabular">{{ formatBytes(record.uplink) }}</span>
            </template>
            <template v-else-if="column.key === 'downlink'">
              <span class="tabular">{{ formatBytes(record.downlink) }}</span>
            </template>
            <template v-else-if="column.key === 'total'">
              <span class="tabular">{{ formatBytes(record.total) }}</span>
            </template>
            <template v-else-if="column.key === 'percent'">
              <a-progress :percent="record.percent" size="small" />
            </template>
          </template>
        </a-table>
      </template>
    </a-spin>
  </a-card>
</template>

<style scoped>
.totals {
  margin-bottom: 16px;
}

.section {
  margin: 24px 0 8px;
}

.tabular {
  font-variant-numeric: tabular-nums;
}
</style>
