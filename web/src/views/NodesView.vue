<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type Node,
  type NodeCycleUsage,
  type NodeMetrics,
} from '@/api/client'
import { formatBytes, formatRelative, formatUTCDay, formatUTCTime } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import NodeDetailDrawer from '@/components/NodeDetailDrawer.vue'

const nodes = ref<Node[]>([])
const todayTraffic = ref<Record<number, number>>({})
// 周期流量批量取回:每行单独请求的话,10 台机器就是 10 次全表扫 ledger。
const cycleTraffic = ref<Record<number, NodeCycleUsage>>({})
const metrics = ref<Record<number, NodeMetrics>>({})
const loading = ref(false)
const detailId = ref<number | null>(null)

const tiers = ref<AccessTier[]>([])

const columns = [
  { title: '节点', key: 'name', width: 230 },
  { title: '访问等级', key: 'tier', width: 130 },
  { title: '状态', key: 'status', width: 110 },
  { title: 'sing-box', key: 'version', width: 170 },
  { title: '资源', key: 'resource', width: 150 },
  { title: '网速', key: 'net', width: 130 },
  { title: '今日流量', key: 'traffic', width: 110 },
  { title: '周期流量', key: 'cycle', width: 190 },
  { title: '最后心跳', key: 'heartbeat', width: 120 },
  { title: '操作', key: 'actions', width: 260 },
]

async function load() {
  loading.value = true
  try {
    // 资源采样是可选能力(可以在配置里关掉),取不到不能拖垮整个列表。
    const [n, t, c, m, tr] = await Promise.all([
      api.nodes(),
      api.nodesTodayTraffic(),
      api.nodesCycleTraffic(),
      api.nodeMetricsLatest().catch(() => ({ items: [] as NodeMetrics[] })),
      api.accessTiers(),
    ])
    nodes.value = n.items
    todayTraffic.value = Object.fromEntries(t.items.map((x) => [x.node_id, x.bytes]))
    cycleTraffic.value = Object.fromEntries(c.items.map((x) => [x.node_id, x]))
    metrics.value = Object.fromEntries(m.items.map((x) => [x.node_id, x]))
    tiers.value = tr.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载节点列表失败')
  } finally {
    loading.value = false
  }
}

// 等级配色按"能用到的节点越多颜色越重"排,让越权配置在列表里一眼看得出来。
function tierColor(code: string): string {
  if (code === 'root') return 'red'
  if (code === 'vip') return 'gold'
  return 'default'
}

function memPercent(m: NodeMetrics): number {
  return m.mem_total_kb > 0 ? (m.mem_used_kb / m.mem_total_kb) * 100 : 0
}

// 使用率的着色阈值。绿到 70%、黄到 90%、再上红 —— 128MB 的小机器上,
// 内存曲线本来就贴着高位走,阈值定得太低会天天报警,反而没人看。
function usageColor(percent: number): string {
  if (percent >= 90) return '#cf1322'
  if (percent >= 70) return '#d46b08'
  return '#389e0d'
}

// ---------- 节点周期流量 ----------

// 进度条颜色按后端给出的告警等级取,不在前端重算阈值 ——
// 边界(80/95/100%)只能有一份定义,两边各算一次迟早会在临界点上对不齐。
const cycleColors: Record<NodeCycleUsage['warning_level'], string> = {
  UNLIMITED: '#8c8c8c',
  NORMAL: '#389e0d',
  WARNING: '#d46b08',
  DANGER: '#cf1322',
  EXCEEDED: '#cf1322',
}

const GIB = 1024 ** 3
const TIB = 1024 ** 4

// 表单里的额度用「数值 + 单位」两个控件,提交时换算成字节。
const quotaValue = ref<number | null>(null)
const quotaUnit = ref<'GB' | 'TB'>('GB')

function quotaBytes(): number {
  if (!quotaValue.value || quotaValue.value <= 0) return 0
  return Math.round(quotaValue.value * (quotaUnit.value === 'TB' ? TIB : GIB))
}

function setQuotaFields(bytes: number) {
  if (!bytes || bytes <= 0) {
    quotaValue.value = null
    quotaUnit.value = 'GB'
    return
  }
  // 整数 TB 才用 TB 显示,否则 1.5TB 会变成一个不好读的小数。
  if (bytes % TIB === 0) {
    quotaValue.value = bytes / TIB
    quotaUnit.value = 'TB'
  } else {
    quotaValue.value = Math.round((bytes / GIB) * 100) / 100
    quotaUnit.value = 'GB'
  }
}

// 编辑时展示的"下一次重置"取后端已算好的值。
// 前端不自己按重置日推算 —— 那等于把周期边界的逻辑抄第二份,
// 短月份、跨年这些情况迟早会和后端算得不一样。
const savedNextReset = computed(() =>
  editingId.value === null ? null : (cycleTraffic.value[editingId.value]?.next_reset_at ?? null),
)

// 表单里的周期设置与已保存的不同时,上面那个时间就不再对应当前选择。
const cycleDirty = computed(() => {
  if (editingId.value === null) return false
  const n = nodes.value.find((x) => x.id === editingId.value)
  if (!n) return false
  return n.traffic_reset_cycle !== form.traffic_reset_cycle ||
    n.traffic_reset_day !== form.traffic_reset_day
})

// ---------- 新增 / 编辑节点 ----------

const formOpen = ref(false)
const submitting = ref(false)
// 非 null 表示编辑该节点,null 表示新增。
const editingId = ref<number | null>(null)
// 节点接入方式。面板持有一把专用密钥,这三种方式都只是"怎么把它的公钥装进节点"。
//   password  —— 填节点口令,面板用一次,不保存;
//   local-key —— 用主控本机 ~/.ssh 里的私钥去装;
//   manual    —— 管理员已经自己装好了公钥,或者要给这个节点单配一把私钥。
type AccessMode = 'password' | 'local-key' | 'manual'
const accessMode = ref<AccessMode>('password')

const form = reactive({
  name: '',
  display_name: '',
  host: '',
  ipv6_address: '',
  traffic_reset_cycle: 'NONE' as 'NONE' | 'MONTHLY',
  traffic_reset_day: 1,
  ssh_port: 22,
  ssh_user: 'root',
  ssh_key: '',
  root_password: '',
  proxy_port: 443,
  listen_port: 0,
  api_port: 28080,
  access_tier_id: 1,
  sort_order: 0,
  subscription_enabled: true,
  public_remark: '',
  maintenance_message: '',
})

function openCreate() {
  editingId.value = null
  accessMode.value = 'password'
  setQuotaFields(0)
  Object.assign(form, {
    name: '',
    display_name: '',
    host: '',
    ipv6_address: '',
    traffic_reset_cycle: 'NONE',
    traffic_reset_day: 1,
    ssh_port: 22,
    ssh_user: 'root',
    ssh_key: '',
    root_password: '',
    proxy_port: 443,
    listen_port: 0,
    api_port: 28080,
    access_tier_id: 1,
    sort_order: 0,
    subscription_enabled: true,
    public_remark: '',
    maintenance_message: '',
  })
  formOpen.value = true
}

function openEdit(n: Node) {
  editingId.value = n.id
  accessMode.value = 'manual'
  setQuotaFields(n.traffic_quota_bytes)
  Object.assign(form, {
    name: n.name,
    display_name: n.display_name,
    host: n.host,
    ipv6_address: n.ipv6_address,
    traffic_reset_cycle: n.traffic_reset_cycle,
    traffic_reset_day: n.traffic_reset_day,
    ssh_port: n.ssh_port,
    ssh_user: n.ssh_user,
    // 私钥不回显,留空即保持原值。
    ssh_key: '',
    root_password: '',
    proxy_port: n.proxy_port,
    // 与公网端口相同时按"未配置转发"展示,免得看起来像特意填了两个一样的值。
    listen_port: n.listen_port === n.proxy_port ? 0 : n.listen_port,
    api_port: n.api_port,
    access_tier_id: n.access_tier_id,
    sort_order: n.sort_order,
    subscription_enabled: n.subscription_enabled,
    public_remark: n.public_remark,
    maintenance_message: n.maintenance_message,
  })
  formOpen.value = true
}

async function submit() {
  submitting.value = true
  try {
    if (editingId.value === null) {
      // 接入方式决定后端走哪条引导路径:只有 manual 会带私钥,
      // 也只有 password 会带口令,不能把两者一起发过去。
      const result = await api.createNode({
        name: form.name,
        display_name: form.display_name,
        access_tier_id: form.access_tier_id,
        sort_order: form.sort_order,
        host: form.host,
        ipv6_address: form.ipv6_address,
        traffic_quota_bytes: quotaBytes(),
        traffic_reset_cycle: form.traffic_reset_cycle,
        traffic_reset_day: form.traffic_reset_day,
        ssh_port: form.ssh_port,
        ssh_user: form.ssh_user,
        proxy_port: form.proxy_port,
        listen_port: form.listen_port,
        api_port: form.api_port,
        ssh_key: accessMode.value === 'manual' ? form.ssh_key : '',
        root_password: accessMode.value === 'password' ? form.root_password : '',
      })
      formOpen.value = false
      // 口令只在这一次请求里用到,立刻从内存里抹掉,免得留在表单状态里。
      form.root_password = ''
      form.ssh_key = ''
      await load()
      if (result.bootstrap_error) {
        Modal.error({
          title: '节点已创建,但公钥没能装上去',
          content: `${result.bootstrap_error}\n\n节点记录已保留,处理好之后可以在节点详情里点「重新引导」重试。`,
          width: 620,
          okText: '知道了',
        })
      } else {
        message.success('节点已创建,请依次执行「探测」「安装」')
      }
      detailId.value = result.node.id
    } else {
      // 先取出 id:确认框是异步的,期间 editingId 可能已被下一次开表单改掉。
      const id = editingId.value
      // 逐字段列出而不是 { ...form }:表单是新增与编辑共用的,里面有
      // root_password 这类只属于新增的字段,而更新接口对未知字段是拒绝的
      // (DisallowUnknownFields),整个提交会以"请求格式错误"失败。
      const { effect } = await api.updateNode(id, {
        name: form.name,
        display_name: form.display_name,
        host: form.host,
        // 留空即清空 IPv6,订阅里的 IPv6 条目随即消失。
        ipv6_address: form.ipv6_address,
        traffic_quota_bytes: quotaBytes(),
        traffic_reset_cycle: form.traffic_reset_cycle,
        traffic_reset_day: form.traffic_reset_day,
        ssh_port: form.ssh_port,
        ssh_user: form.ssh_user,
        ssh_key: form.ssh_key,
        proxy_port: form.proxy_port,
        listen_port: form.listen_port,
        api_port: form.api_port,
        access_tier_id: form.access_tier_id,
        sort_order: form.sort_order,
        subscription_enabled: form.subscription_enabled,
        public_remark: form.public_remark,
        maintenance_message: form.maintenance_message,
      })
      formOpen.value = false
      await load()
      if (effect.tier_changed) {
        // 等级变更由面板自动标脏,不需要管理员再挑时机 —— 拖着不部署
        // 等于被移出的用户还能继续用,提示里要把这一点说清楚。
        message.success(
          `已保存:${effect.changes.join(';')}。访问等级变了,受影响节点已排入自动重新部署。`,
        )
      } else if (effect.needs_deploy) {
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
      :scroll="{ x: 1450 }"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'">
          <a @click="detailId = record.id">{{ record.name }}</a>
          <!-- 内部名称与展示名称不同时两个都列出来:管理员按内部名称找机器,
               用户报的却是展示名称,只显示一个必然对不上号。 -->
          <div v-if="record.display_name !== record.name" class="display-name">
            对外:{{ record.display_name }}
          </div>
          <div class="node-host">
            {{ record.host }}:{{ record.proxy_port }}
            <span v-if="record.listen_port !== record.proxy_port">
              → 主机 {{ record.listen_port }}
            </span>
          </div>
          <!-- IPv6 只进订阅,不参与 SSH,所以单独一行而不是和端口拼在一起。 -->
          <div v-if="record.ipv6_address" class="node-host">
            <a-tag color="blue" class="v6-tag">IPv6</a-tag>{{ record.ipv6_address }}
          </div>
        </template>

        <template v-else-if="column.key === 'tier'">
          <a-tag :color="tierColor(record.access_tier_code)">{{ record.access_tier_name }}</a-tag>
          <div v-if="!record.subscription_enabled" class="off-sub">已停发订阅</div>
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

        <template v-else-if="column.key === 'resource'">
          <span v-if="!metrics[record.id]" class="muted">—</span>
          <template v-else>
            <div class="metric">
              <span class="metric-label">CPU</span>
              <span
                class="tabular"
                :style="{ color: usageColor(metrics[record.id].cpu_percent) }"
              >
                {{ metrics[record.id].cpu_percent.toFixed(0) }}%
              </span>
            </div>
            <div class="metric">
              <span class="metric-label">内存</span>
              <span class="tabular" :style="{ color: usageColor(memPercent(metrics[record.id])) }">
                {{ memPercent(metrics[record.id]).toFixed(0) }}%
              </span>
              <span class="metric-sub">
                {{ formatBytes(metrics[record.id].mem_used_kb * 1024) }} /
                {{ formatBytes(metrics[record.id].mem_total_kb * 1024) }}
              </span>
            </div>
          </template>
        </template>

        <template v-else-if="column.key === 'net'">
          <span v-if="!metrics[record.id]" class="muted">—</span>
          <template v-else>
            <div class="metric tabular">↓ {{ formatBytes(metrics[record.id].net_rx_bps) }}/s</div>
            <div class="metric tabular">↑ {{ formatBytes(metrics[record.id].net_tx_bps) }}/s</div>
          </template>
        </template>

        <template v-else-if="column.key === 'traffic'">
          <span class="tabular">{{ formatBytes(todayTraffic[record.id] ?? 0) }}</span>
        </template>

        <template v-else-if="column.key === 'cycle'">
          <span v-if="!cycleTraffic[record.id]" class="muted">—</span>
          <template v-else>
            <div class="metric tabular">
              {{ formatBytes(cycleTraffic[record.id].used_bytes) }}
              /
              <span v-if="cycleTraffic[record.id].unlimited" class="muted">不限量</span>
              <span v-else>{{ formatBytes(cycleTraffic[record.id].quota_bytes) }}</span>
            </div>
            <!-- 不限量节点不画进度条:没有分母,画出来只能是 0% 或 100%,两种都是错的。 -->
            <a-progress
              v-if="!cycleTraffic[record.id].unlimited"
              :percent="Math.min(cycleTraffic[record.id].usage_percent ?? 0, 100)"
              :stroke-color="cycleColors[cycleTraffic[record.id].warning_level]"
              :show-info="false"
              size="small"
            />
            <div class="cycle-foot">
              <a-tag v-if="cycleTraffic[record.id].exceeded" color="red">已超额</a-tag>
              <span v-else-if="cycleTraffic[record.id].next_reset_at" class="muted">
                {{ formatUTCDay(cycleTraffic[record.id].next_reset_at) }}重置
              </span>
              <span v-else class="muted">累计流量</span>
            </div>
          </template>
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
      <a-form-item label="内部名称" required extra="只在管理后台出现,可以写机房、供应商、到期日">
        <a-input v-model:value="form.name" placeholder="例如:LAX-cn2gia-到期20261201" />
      </a-form-item>
      <a-form-item
        label="展示名称"
        extra="用户订阅与门户里显示的就是它,留空则与内部名称相同"
      >
        <a-input v-model:value="form.display_name" placeholder="例如:洛杉矶 01" />
      </a-form-item>
      <a-row :gutter="12">
        <a-col :span="12">
          <a-form-item label="访问等级" extra="等级不高于用户等级的节点会被自动继承">
            <a-select v-model:value="form.access_tier_id">
              <a-select-option v-for="t in tiers" :key="t.id" :value="t.id">
                {{ t.name }}
              </a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="12">
          <a-form-item label="排序" extra="数值小的排在订阅前面">
            <a-input-number v-model:value="form.sort_order" style="width: 100%" />
          </a-form-item>
        </a-col>
      </a-row>
      <template v-if="editingId !== null">
        <a-form-item extra="关掉后不再进入新生成的订阅,节点、历史流量与部署记录都保留">
          <a-checkbox v-model:checked="form.subscription_enabled">下发到用户订阅</a-checkbox>
        </a-form-item>
        <a-form-item label="公开备注" extra="用户门户可见,例如「晚高峰限速」">
          <a-input v-model:value="form.public_remark" :maxlength="128" />
        </a-form-item>
        <a-form-item label="维护说明" extra="节点维护时给用户看的提示,留空表示无">
          <a-input v-model:value="form.maintenance_message" :maxlength="128" />
        </a-form-item>
      </template>
      <a-form-item
        label="IPv4 地址"
        required
        extra="用于 SSH 管理、节点部署和 IPv4 订阅,必须填写"
      >
        <a-input v-model:value="form.host" placeholder="例如:192.0.2.10" />
      </a-form-item>
      <a-form-item
        label="IPv6 地址"
        extra="选填。目前只用于订阅下发,填写后订阅中将额外生成「展示名称-IPV6」条目;清空即撤下该条目,都不需要重新部署"
      >
        <a-input v-model:value="form.ipv6_address" placeholder="例如:2602:fed2:7116:2110::1" />
      </a-form-item>

      <a-row :gutter="12">
        <a-col :span="10">
          <a-form-item label="节点流量限额" extra="留空表示不限量">
            <a-input-number
              v-model:value="quotaValue"
              :min="0"
              :precision="2"
              placeholder="不限量"
              style="width: 100%"
            />
          </a-form-item>
        </a-col>
        <a-col :span="6">
          <a-form-item label="单位">
            <a-select v-model:value="quotaUnit">
              <a-select-option value="GB">GB</a-select-option>
              <a-select-option value="TB">TB</a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
        <a-col :span="8">
          <a-form-item label="重置周期">
            <a-select v-model:value="form.traffic_reset_cycle">
              <a-select-option value="NONE">不重置</a-select-option>
              <a-select-option value="MONTHLY">每月</a-select-option>
            </a-select>
          </a-form-item>
        </a-col>
      </a-row>
      <a-form-item
        v-if="form.traffic_reset_cycle === 'MONTHLY'"
        label="每月重置日"
        extra="边界统一取 UTC 00:00。当月没有该日时按当月最后一天处理(31 日在二月即 28 或 29 日)"
      >
        <a-input-number
          v-model:value="form.traffic_reset_day"
          :min="1"
          :max="31"
          style="width: 160px"
        />
      </a-form-item>
      <p v-if="editingId !== null && savedNextReset" class="port-hint">
        下一次重置:{{ formatUTCTime(savedNextReset) }}
        <span v-if="cycleDirty">(按已保存的设置计算,保存后会按新设置重新计算)</span>
      </p>
      <p class="port-hint">
        节点额度只用于统计与预警:超额会在仪表盘和列表里标红,但不会停掉 sing-box、
        不会禁用节点,也不会把节点从订阅里摘掉 —— 那会同时打断这台机器上的全部用户。
      </p>
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
      <template v-if="editingId === null">
        <a-form-item label="接入方式" extra="面板有一把自己的专用密钥,这里选的是怎么把它的公钥装进节点">
          <a-radio-group v-model:value="accessMode" button-style="solid">
            <a-radio-button value="password">节点密码</a-radio-button>
            <a-radio-button value="local-key">主控本机私钥</a-radio-button>
            <a-radio-button value="manual">手工指定私钥</a-radio-button>
          </a-radio-group>
        </a-form-item>

        <a-form-item
          v-if="accessMode === 'password'"
          label="节点登录密码"
          required
          extra="只用于把面板公钥装进节点的那一次连接,用完即弃,不会保存,也不会写进日志"
        >
          <a-input-password
            v-model:value="form.root_password"
            autocomplete="new-password"
            placeholder="该节点 root 的密码"
          />
        </a-form-item>

        <a-alert
          v-else-if="accessMode === 'local-key'"
          type="info"
          show-icon
          class="mode-hint"
          message="用主控本机的私钥去装公钥"
          description="面板会在自己进程的 ~/.ssh 与 /etc/litebox/keys 下找一把能登录该节点的私钥。找不到或登录不上时,改用「节点密码」。"
        />

        <a-form-item
          v-else
          label="SSH 私钥"
          required
          extra="给这个节点单配一把私钥,用主密钥加密后存储,不会再次显示"
        >
          <a-textarea
            v-model:value="form.ssh_key"
            :rows="5"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
          />
        </a-form-item>
      </template>

      <a-form-item
        v-else
        label="SSH 私钥"
        extra="留空表示保持不变(用面板专用密钥的节点请一直留空);填入新私钥则给这个节点单独换一把"
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

.display-name {
  color: rgb(0 0 0 / 65%);
  font-size: 12px;
}

.off-sub {
  color: #d46b08;
  font-size: 12px;
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

.metric {
  font-size: 12px;
  line-height: 1.6;
}

.metric-label {
  display: inline-block;
  width: 30px;
  color: rgb(0 0 0 / 45%);
}

.metric-sub {
  margin-left: 6px;
  color: rgb(0 0 0 / 45%);
}

.v6-tag {
  margin-right: 4px;
  transform: scale(0.85);
  transform-origin: left center;
}

.cycle-foot {
  margin-top: 2px;
  font-size: 12px;
  line-height: 1.5;
}

.mode-hint {
  margin-bottom: 24px;
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
