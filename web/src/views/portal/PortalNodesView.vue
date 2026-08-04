<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { portalApi, ApiError, type PortalNode } from '@/api/client'
import { formatBytes, formatRelative } from '@/utils/format'

const nodes = ref<PortalNode[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    nodes.value = (await portalApi.nodes()).items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载失败')
  } finally {
    loading.value = false
  }
}

function statusTag(node: PortalNode): { color: string; text: string } {
  if (node.status === 'disabled') return { color: 'default', text: '已停用' }
  if (node.status === 'maintenance') return { color: 'orange', text: '维护中' }
  return { color: 'green', text: '正常' }
}

function tierColor(code: string): string {
  if (code === 'root') return 'red'
  if (code === 'vip') return 'gold'
  return 'default'
}

onMounted(load)
</script>

<template>
  <a-card title="我的节点">
    <template #extra>
      <a-button size="small" :loading="loading" @click="load">刷新</a-button>
    </template>

    <a-empty v-if="!loading && nodes.length === 0" description="还没有可用节点,请联系管理员" />

    <a-row :gutter="[16, 16]">
      <a-col v-for="node in nodes" :key="node.id" :xs="24" :sm="12" :lg="8">
        <a-card size="small" class="node-card">
          <div class="node-head">
            <span class="node-name">{{ node.display_name }}</span>
            <a-tag :color="statusTag(node).color">{{ statusTag(node).text }}</a-tag>
          </div>

          <div class="node-meta">
            <a-tag :color="tierColor(node.tier_code)" size="small">{{ node.tier_name }}</a-tag>
            <!-- 只说明订阅里会多一条 IPv6 条目,不给出地址 —— 那是节点信息。 -->
            <a-tag v-if="node.supports_ipv6" color="blue" size="small">IPv6</a-tag>
            <span class="proto">{{ node.protocol }}</span>
            <span class="port">端口 {{ node.public_port }}</span>
          </div>

          <a-alert
            v-if="node.maintenance_message"
            type="warning"
            :message="node.maintenance_message"
            class="node-alert"
          />
          <!-- 已下架但没写维护说明时也要给一句话:否则用户只看到"维护中"
               三个字,不知道要不要重新导入订阅。 -->
          <a-alert
            v-else-if="node.status === 'maintenance'"
            type="warning"
            message="该节点暂未下发到订阅,恢复后会自动出现"
            class="node-alert"
          />
          <div v-else-if="node.public_remark" class="remark">{{ node.public_remark }}</div>

          <a-descriptions :column="1" size="small" class="node-traffic">
            <a-descriptions-item label="今日">
              {{ formatBytes(node.today_bytes) }}
            </a-descriptions-item>
            <a-descriptions-item label="本月">
              {{ formatBytes(node.month_bytes) }}
            </a-descriptions-item>
            <a-descriptions-item label="累计">
              {{ formatBytes(node.total_bytes) }}
            </a-descriptions-item>
            <a-descriptions-item label="最近更新">
              {{ formatRelative(node.last_seen_at) }}
            </a-descriptions-item>
          </a-descriptions>
        </a-card>
      </a-col>
    </a-row>
  </a-card>
</template>

<style scoped>
.node-card {
  height: 100%;
}

.node-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.node-name {
  font-size: 15px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.node-meta {
  margin-top: 6px;
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}

.node-alert {
  margin-top: 8px;
}

.remark {
  margin-top: 8px;
  font-size: 12px;
  color: rgb(0 0 0 / 65%);
}

.node-traffic {
  margin-top: 8px;
  font-variant-numeric: tabular-nums;
}
</style>
