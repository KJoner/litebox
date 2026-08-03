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
  { title: '操作', key: 'actions', width: 260 },
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

// ---------- 新增 / 编辑节点 ----------

const formOpen = ref(false)
const submitting = ref(false)
// 非 null 表示编辑该节点,null 表示新增。
const editingId = ref<number | null>(null)
const form = reactive({
  name: '',
  host: '',
  ssh_port: 22,
  ssh_user: 'root',
  ssh_key: '',
  proxy_port: 443,
  listen_port: 0,
  api_port: 28080,
})

function openCreate() {
  editingId.value = null
  Object.assign(form, {
    name: '',
    host: '',
    ssh_port: 22,
    ssh_user: 'root',
    ssh_key: '',
    proxy_port: 443,
    listen_port: 0,
    api_port: 28080,
  })
  formOpen.value = true
}

function openEdit(n: Node) {
  editingId.value = n.id
  Object.assign(form, {
    name: n.name,
    host: n.host,
    ssh_port: n.ssh_port,
    ssh_user: n.ssh_user,
    // 私钥不回显,留空即保持原值。
    ssh_key: '',
    proxy_port: n.proxy_port,
    // 与公网端口相同时按"未配置转发"展示,免得看起来像特意填了两个一样的值。
    listen_port: n.listen_port === n.proxy_port ? 0 : n.listen_port,
    api_port: n.api_port,
  })
  formOpen.value = true
}

async function submit() {
  submitting.value = true
  try {
    if (editingId.value === null) {
      const node = await api.createNode({ ...form })
      message.success('节点已创建,请依次执行「探测」「安装」')
      formOpen.value = false
      await load()
      detailId.value = node.id
    } else {
      // 先取出 id:确认框是异步的,期间 editingId 可能已被下一次开表单改掉。
      const id = editingId.value
      const { effect } = await api.updateNode(id, { ...form })
      formOpen.value = false
      await load()
      if (effect.needs_deploy) {
        Modal.confirm({
          title: '配置已保存,但尚未在节点上生效',
          content: `${effect.changes.join(';')}。这些改动进入了节点配置,需要重新部署才生效,部署会重启 sing-box 并断开当前在线连接。`,
          okText: '立即部署',
          cancelText: '稍后手动部署',
          onOk: () => run(id, '部署', () => api.deployNode(id), '部署已执行,详情见部署记录'),
        })
      } else {
        message.success(effect.changes.length ? '已保存' : '没有任何改动')
      }
    }
  } catch (err) {
    message.error(
      err instanceof ApiError ? err.message : editingId.value === null ? '创建节点失败' : '保存失败',
    )
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
          <div class="node-host">
            {{ record.host }}:{{ record.proxy_port }}
            <span v-if="record.listen_port !== record.proxy_port">
              → 主机 {{ record.listen_port }}
            </span>
          </div>
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
            <a @click="openEdit(record)">编辑</a>
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
    :title="editingId === null ? '新增节点' : '编辑节点'"
    :confirm-loading="submitting"
    :ok-text="editingId === null ? '创建' : '保存'"
    cancel-text="取消"
    width="560"
    @ok="submit"
  >
    <a-form layout="vertical">
      <a-form-item label="节点名称" required extra="会显示在用户的客户端里">
        <a-input v-model:value="form.name" placeholder="例如:洛杉矶 01" />
      </a-form-item>
      <a-form-item label="主机地址" required extra="面板用它连 SSH,客户端也用它连代理">
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
      <a-form-item
        label="SSH 私钥"
        :required="editingId === null"
        :extra="
          editingId === null
            ? '用主密钥加密后存储,不会再次显示'
            : '留空表示保持原私钥不变;填入新私钥即完成轮换'
        "
      >
        <a-textarea
          v-model:value="form.ssh_key"
          :rows="5"
          placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
        />
      </a-form-item>
      <a-row :gutter="12">
        <a-col :span="8">
          <a-form-item label="公网代理端口" extra="写进订阅">
            <a-input-number
              v-model:value="form.proxy_port"
              :min="1"
              :max="65535"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="8">
          <a-form-item label="主机代理端口" extra="留空=与公网相同">
            <a-input-number
              v-model:value="form.listen_port"
              :min="0"
              :max="65535"
              placeholder="不填"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="8">
          <a-form-item label="API 端口" extra="仅监听节点回环">
            <a-input-number v-model:value="form.api_port" :min="1" :max="65535" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>
      <a-alert
        v-if="form.listen_port && form.listen_port !== form.proxy_port"
        type="warning"
        show-icon
        :message="`需要自行把 ${form.host || '节点'}:${form.proxy_port} 转发到本机 ${form.listen_port}`"
        description="面板不会创建这条转发规则。NAT 主机由服务商的端口映射完成,自建则用 nginx stream 或 iptables DNAT;sing-box 只负责监听主机端口。"
      />
      <p class="port-hint">
        直连节点不用管「主机代理端口」。只有公网端口与 sing-box 实际监听的端口不一致时才填 ——
        NAT 小鸡的端口映射,或者 443 被 nginx 占着需要转发到别的端口。
      </p>
      <p v-if="editingId !== null" class="port-hint">
        REALITY 握手目标不在这里改:它必须从节点本机实测通过才能保存,请到节点详情里检测后应用。
      </p>
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

.port-hint {
  margin: 12px 0 0;
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
  line-height: 1.7;
}

.danger {
  color: #cf1322;
}
</style>
