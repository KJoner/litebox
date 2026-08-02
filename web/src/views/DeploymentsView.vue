<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type DeploymentRecord, type Node } from '@/api/client'
import { formatTime, shortHash } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import DeployStepList from '@/components/DeployStepList.vue'

const records = ref<DeploymentRecord[]>([])
const nodes = ref<Node[]>([])
const loading = ref(false)
const onlyFailed = ref(false)

const nodeNames = computed(() =>
  Object.fromEntries(nodes.value.map((n) => [n.id, n.name])) as Record<number, string>,
)

const visible = computed(() =>
  onlyFailed.value
    ? records.value.filter((r) => r.status === 'FAILED' || r.status === 'ROLLED_BACK')
    : records.value,
)

const columns = [
  { title: '节点', key: 'node', width: 180 },
  { title: 'revision', key: 'revision', width: 100 },
  { title: '配置哈希', key: 'hash', width: 140 },
  { title: '状态', key: 'status', width: 110 },
  { title: '开始时间', key: 'started', width: 160 },
  { title: '结果', key: 'result' },
]

async function load() {
  loading.value = true
  try {
    const [d, n] = await Promise.all([api.deployments(100), api.nodes()])
    records.value = d.items
    nodes.value = n.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载部署记录失败')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <a-card title="部署记录">
    <template #extra>
      <a-space>
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
      <template #expandedRowRender="{ record }">
        <DeployStepList :record="record" />
      </template>

      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'node'">
          {{ nodeNames[record.node_id] ?? `节点 ${record.node_id}` }}
        </template>
        <template v-else-if="column.key === 'revision'">
          <span class="tabular">{{ record.revision }}</span>
        </template>
        <template v-else-if="column.key === 'hash'">
          <span class="mono" :title="record.config_sha256">{{ shortHash(record.config_sha256) }}</span>
        </template>
        <template v-else-if="column.key === 'status'">
          <StatusTag :status="record.status" kind="deploy" />
        </template>
        <template v-else-if="column.key === 'started'">
          {{ formatTime(record.started_at) }}
        </template>
        <template v-else-if="column.key === 'result'">
          <span v-if="record.error_message" class="error-msg">{{ record.error_message }}</span>
          <span v-else-if="record.rollback_result" class="rollback">{{ record.rollback_result }}</span>
          <span v-else class="muted">正常</span>
        </template>
      </template>
    </a-table>
  </a-card>
</template>

<style scoped>
.tabular {
  font-variant-numeric: tabular-nums;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
}

.error-msg {
  color: #cf1322;
  word-break: break-all;
}

.rollback {
  color: #d46b08;
}

.muted {
  color: rgb(0 0 0 / 45%);
}
</style>
