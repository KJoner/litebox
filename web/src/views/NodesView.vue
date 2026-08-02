<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type Node } from '@/api/client'
import { formatBytes, formatRelative } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import NodeDetailDrawer from '@/components/NodeDetailDrawer.vue'

const nodes = ref<Node[]>([])
const todayTraffic = ref<Record<number, number>>({})
const loading = ref(false)
const detailId = ref<number | null>(null)

const columns = [
  { title: '节点', key: 'name', width: 200 },
  { title: '状态', key: 'status', width: 110 },
  { title: 'sing-box', key: 'version', width: 200 },
  { title: '今日流量', key: 'traffic', width: 120 },
  { title: '最后心跳', key: 'heartbeat', width: 130 },
  { title: '操作', key: 'actions', width: 220 },
]

async function load() {
  loading.value = true
  try {
    const [n, t] = await Promise.all([api.nodes(), api.nodesTodayTraffic()])
    nodes.value = n.items
    todayTraffic.value = Object.fromEntries(t.items.map((x) => [x.node_id, x.bytes]))
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载节点列表失败')
  } finally {
    loading.value = false
  }
}

// ---------- 新增节点 ----------

const formOpen = ref(false)
const submitting = ref(false)
const form = reactive({
  name: '',
  host: '',
  ssh_port: 22,
  ssh_user: 'root',
  ssh_key: '',
  proxy_port: 443,
  api_port: 28080,
})

function openCreate() {
  Object.assign(form, {
    name: '',
    host: '',
    ssh_port: 22,
    ssh_user: 'root',
    ssh_key: '',
    proxy_port: 443,
    api_port: 28080,
  })
  formOpen.value = true
}

async function submit() {
  submitting.value = true
  try {
    const node = await api.createNode({ ...form })
    message.success(`节点已创建,请依次执行「探测」「安装」`)
    formOpen.value = false
    await load()
    detailId.value = node.id
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '创建节点失败')
  } finally {
    submitting.value = false
  }
}

// ---------- 行内操作 ----------

const busy = ref<Record<number, string>>({})

async function run(id: number, label: string, fn: () => Promise<unknown>, successText: string) {
  busy.value = { ...busy.value, [id]: label }
  try {
    await fn()
    message.success(successText)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    const next = { ...busy.value }
    delete next[id]
    busy.value = next
  }
}

function confirmDelete(n: Node) {
  Modal.confirm({
    title: `删除节点 ${n.name}?`,
    content: '面板将不再管理该节点。节点上的 sing-box 与配置不会被自动清除,需要手动处理。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => run(n.id, '删除', () => api.deleteNode(n.id), '节点已删除'),
  })
}

function toggleEnabled(n: Node) {
  const enable = n.status === 'DISABLED'
  run(
    n.id,
    enable ? '启用' : '禁用',
    () => api.setNodeEnabled(n.id, enable),
    enable ? '已启用' : '已禁用,该节点不再出现在用户订阅中',
  )
}

onMounted(load)
</script>

<template>
  <a-card>
    <template #title>节点管理</template>
    <template #extra>
      <a-space>
        <a-button :loading="loading" @click="load">刷新</a-button>
        <a-button type="primary" @click="openCreate">新增节点</a-button>
      </a-space>
    </template>

    <a-alert
      type="info"
      show-icon
      class="hint"
      message="一台机器只能作为一个节点"
      description="节点上的路径与服务名是固定的,两个节点指向同一主机会互相覆盖配置。"
    />

    <a-table
      :columns="columns"
      :data-source="nodes"
      :loading="loading"
      row-key="id"
      size="middle"
      :pagination="false"
      :scroll="{ x: 980 }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'">
          <a @click="detailId = record.id">{{ record.name }}</a>
          <div class="node-host">{{ record.host }}:{{ record.proxy_port }}</div>
        </template>

        <template v-else-if="column.key === 'status'">
          <StatusTag :status="record.status" kind="node" />
          <div v-if="busy[record.id]" class="busy">{{ busy[record.id] }}中…</div>
        </template>

        <template v-else-if="column.key === 'version'">
          <span v-if="!record.singbox_version" class="muted">未探测</span>
          <template v-else>
            <div class="version">{{ record.singbox_version }}</div>
            <div class="arch">{{ record.arch }}</div>
          </template>
        </template>

        <template v-else-if="column.key === 'traffic'">
          <span class="tabular">{{ formatBytes(todayTraffic[record.id] ?? 0) }}</span>
        </template>

        <template v-else-if="column.key === 'heartbeat'">
          <span class="muted">{{ formatRelative(record.last_heartbeat_at) }}</span>
        </template>

        <template v-else-if="column.key === 'actions'">
          <a-space size="small">
            <a @click="detailId = record.id">详情</a>
            <a @click="run(record.id, '探测', () => api.probeNode(record.id), '探测完成')">探测</a>
            <a @click="run(record.id, '部署', () => api.deployNode(record.id), '部署已执行,详情见部署记录')">
              部署
            </a>
            <a @click="toggleEnabled(record)">
              {{ record.status === 'DISABLED' ? '启用' : '禁用' }}
            </a>
            <a class="danger" @click="confirmDelete(record)">删除</a>
          </a-space>
        </template>
      </template>
    </a-table>
  </a-card>

  <a-modal
    v-model:open="formOpen"
    title="新增节点"
    :confirm-loading="submitting"
    ok-text="创建"
    cancel-text="取消"
    width="560"
    @ok="submit"
  >
    <a-form layout="vertical">
      <a-form-item label="节点名称" required extra="会显示在用户的客户端里">
        <a-input v-model:value="form.name" placeholder="例如:洛杉矶 01" />
      </a-form-item>
      <a-form-item label="主机地址" required>
        <a-input v-model:value="form.host" placeholder="IP 或域名" />
      </a-form-item>
      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="SSH 端口">
            <a-input-number v-model:value="form.ssh_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="SSH 用户">
            <a-input v-model:value="form.ssh_user" />
          </a-form-item>
        </a-col>
      </a-row>
      <a-form-item label="SSH 私钥" required extra="用主密钥加密后存储,不会再次显示">
        <a-textarea
          v-model:value="form.ssh_key"
          :rows="5"
          placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
        />
      </a-form-item>
      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="代理端口" extra="VLESS 对外监听端口">
            <a-input-number v-model:value="form.proxy_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="API 端口" extra="仅监听节点回环">
            <a-input-number v-model:value="form.api_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>
    </a-form>
  </a-modal>

  <NodeDetailDrawer :node-id="detailId" @close="detailId = null" @changed="load" />
</template>

<style scoped>
.hint {
  margin-bottom: 16px;
}

.node-host,
.arch {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.version {
  font-size: 13px;
}

.muted {
  color: rgb(0 0 0 / 45%);
}

.tabular {
  font-variant-numeric: tabular-nums;
}

.busy {
  color: #1677ff;
  font-size: 12px;
}

.danger {
  color: #cf1322;
}
</style>
