<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ConfigDiff,
  type DailyPoint,
  type DeploymentRecord,
  type DestCheckResult,
  type Node,
  type NodeMetrics,
  type ProbeResult,
} from '@/api/client'
import { formatBytes, formatRelative, formatTime, shortHash } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import TrafficChart from '@/components/TrafficChart.vue'
import MetricsChart from '@/components/MetricsChart.vue'
import DeployStepList from '@/components/DeployStepList.vue'

const props = defineProps<{ nodeId: number | null }>()
const emit = defineEmits<{ close: []; changed: [] }>()

const node = ref<Node | null>(null)
const probe = ref<ProbeResult | null>(null)
const deployments = ref<DeploymentRecord[]>([])
const diff = ref<ConfigDiff | null>(null)
const daily = ref<DailyPoint[]>([])
const destResults = ref<DestCheckResult[]>([])
const loading = ref(false)
const running = ref('')

async function load(id: number) {
  loading.value = true
  try {
    const [n, d, t] = await Promise.all([
      api.node(id),
      api.nodeDeployments(id, 10),
      api.nodeTraffic(id, 30),
    ])
    node.value = n
    deployments.value = d.items
    daily.value = t.daily
    await loadMetrics(id)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '加载节点详情失败')
  } finally {
    loading.value = false
  }
}

watch(
  () => props.nodeId,
  (id) => {
    probe.value = null
    diff.value = null
    destResults.value = []
    metrics.value = null
    metricsHistory.value = []
    if (id !== null) load(id)
    else node.value = null
  },
  { immediate: true },
)

async function run(label: string, fn: () => Promise<unknown>, done: string) {
  running.value = label
  try {
    await fn()
    message.success(done)
    emit('changed')
    if (props.nodeId !== null) await load(props.nodeId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : `${label}失败`)
  } finally {
    running.value = ''
  }
}

async function doTestSSH() {
  running.value = '测试 SSH'
  try {
    const r = await api.testNodeSSH(props.nodeId!)
    Modal.info({ title: 'SSH 连接正常', content: r.uname, okText: '知道了' })
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : 'SSH 测试失败')
  } finally {
    running.value = ''
  }
}

async function doInstall() {
  running.value = '安装'
  try {
    const r = await api.installNode(props.nodeId!)
    message.success(`sing-box 与 ${r.init_system} 服务定义已就绪`)
    emit('changed')
    if (props.nodeId !== null) await load(props.nodeId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '安装失败')
  } finally {
    running.value = ''
  }
}

async function doProbe() {
  running.value = '探测'
  try {
    probe.value = await api.probeNode(props.nodeId!)
    emit('changed')
    if (props.nodeId !== null) await load(props.nodeId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '探测失败')
  } finally {
    running.value = ''
  }
}

async function doScanDests() {
  running.value = '扫描握手目标'
  try {
    const r = await api.scanNodeDests(props.nodeId!)
    destResults.value = r.items
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '扫描失败')
  } finally {
    running.value = ''
  }
}

async function applyDest(dest: string) {
  await run('应用握手目标', () => api.checkNodeDest(props.nodeId!, dest, true), `已应用 ${dest}`)
  destResults.value = []
}

async function doDiff() {
  running.value = '比对配置'
  try {
    diff.value = await api.nodeConfigDiff(props.nodeId!)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '比对失败')
  } finally {
    running.value = ''
  }
}

async function doDeploy() {
  running.value = '部署'
  try {
    const result = await api.deployNode(props.nodeId!)
    if (result.status === 'SUCCESS') {
      message.success(`部署成功 revision ${result.revision}`)
    } else {
      // 部署失败时步骤明细才是最有用的信息,直接展开。
      Modal.error({
        title: `部署${result.status === 'ROLLED_BACK' ? '失败并已回滚' : '失败'}`,
        content: result.error_message || '(无错误信息)',
        width: 560,
        okText: '知道了',
      })
    }
    emit('changed')
    if (props.nodeId !== null) await load(props.nodeId)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '部署失败')
  } finally {
    running.value = ''
  }
}

function confirmRestart() {
  Modal.confirm({
    title: '重启节点服务?',
    content: '这是运维用的直接重启,不会先同步流量。常规的用户变更请使用「部署」。',
    okText: '重启',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => run('重启', () => api.restartNode(props.nodeId!), '已重启'),
  })
}

function confirmResetHostKey() {
  Modal.confirm({
    title: '重置 SSH 主机密钥?',
    content: '仅在节点重装后使用。重置后下次连接会重新固定主机密钥,期间失去中间人防护。',
    okText: '重置',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => run('重置主机密钥', () => api.resetNodeHostKey(props.nodeId!), '已重置'),
  })
}

// ---------- 重新引导 ----------

const bootstrapOpen = ref(false)
const bootstrapPassword = ref('')

function openBootstrap() {
  bootstrapPassword.value = ''
  bootstrapOpen.value = true
}

async function doBootstrap() {
  const id = props.nodeId!
  const password = bootstrapPassword.value
  bootstrapOpen.value = false
  // 口令用完立刻抹掉,不留在组件状态里。
  bootstrapPassword.value = ''

  running.value = '引导'
  try {
    const r = await api.bootstrapNode(id, password)
    message.success(r.already_present ? '节点上已有面板公钥,连接正常' : '面板公钥已装入并验证通过')
    emit('changed')
    await load(id)
  } catch (err) {
    Modal.error({
      title: '引导失败',
      content: err instanceof ApiError ? err.message : '引导失败',
      width: 620,
      okText: '知道了',
    })
  } finally {
    running.value = ''
  }
}

function confirmUninstall() {
  Modal.confirm({
    title: '卸载节点上的 sing-box?',
    content:
      '会停止并删除 litebox-singbox 服务、它的 systemd 单元或 OpenRC 脚本,以及 /opt/litebox 目录,' +
      '不触碰机器上的其他服务。用户会立刻断线。节点记录保留,重新「安装」「部署」即可恢复。',
    okText: '卸载',
    okType: 'danger',
    cancelText: '取消',
    width: 520,
    onOk: () => run('卸载', () => api.uninstallNode(props.nodeId!), '节点上的服务与配置已移除'),
  })
}

// ---------- 资源监控 ----------

const metrics = ref<NodeMetrics | null>(null)
const metricsHistory = ref<NodeMetrics[]>([])
// 趋势区间。默认 6 小时,更长的区间用于回看"昨晚是不是 OOM 过"。
// 上限 168 小时(7 天)与后端的默认保留期一致 —— 给一个更长的选项,
// 只会让用户看到一段被清理掉的空白。
const metricsHours = ref(6)

async function loadMetrics(id: number) {
  try {
    const r = await api.nodeMetricsHistory(id, metricsHours.value)
    metricsHistory.value = r.items
    metrics.value = r.items.length > 0 ? r.items[r.items.length - 1] : null
  } catch {
    // 监控可以在配置里整个关掉,取不到就当没有,不打扰其他信息的展示。
    metricsHistory.value = []
    metrics.value = null
  }
}

watch(metricsHours, () => {
  if (props.nodeId !== null) loadMetrics(props.nodeId)
})

const metricLabels = computed(() => metricsHistory.value.map((m) => m.collected_at))

const cpuSeries = computed(() => [
  {
    name: 'CPU',
    color: '#2a78d6',
    values: metricsHistory.value.map((m) => m.cpu_percent),
  },
])

const memSeries = computed(() => [
  {
    name: '内存',
    color: '#7d3fc0',
    values: metricsHistory.value.map((m) => memPercent(m)),
  },
])

// 上下行画在同一张图上:分成两张就看不出"下行涨的时候上行有没有跟着涨",
// 而那正是判断链路是否正常的第一眼依据。
const netSeries = computed(() => [
  { name: '下行', color: '#2a78d6', values: metricsHistory.value.map((m) => m.net_rx_bps) },
  { name: '上行', color: '#d46b08', values: metricsHistory.value.map((m) => m.net_tx_bps) },
])

function formatPercent(v: number): string {
  return `${v.toFixed(0)}%`
}

function formatRate(v: number): string {
  return `${formatBytes(v)}/s`
}

async function doCollectMetrics() {
  running.value = '采集资源'
  try {
    metrics.value = await api.collectNodeMetrics(props.nodeId!)
    await loadMetrics(props.nodeId!)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '采集失败')
  } finally {
    running.value = ''
  }
}

function memPercent(m: NodeMetrics): number {
  return m.mem_total_kb > 0 ? (m.mem_used_kb / m.mem_total_kb) * 100 : 0
}

function diskPercent(m: NodeMetrics): number {
  return m.disk_total_kb > 0 ? (m.disk_used_kb / m.disk_total_kb) * 100 : 0
}

function usageStatus(percent: number): 'normal' | 'exception' | 'active' {
  if (percent >= 90) return 'exception'
  if (percent >= 70) return 'active'
  return 'normal'
}

function formatUptime(seconds: number): string {
  if (seconds <= 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分`
  return `${minutes} 分`
}
</script>

<template>
  <a-drawer
    :open="nodeId !== null"
    :title="node ? `${node.name} · ${node.host}` : '节点详情'"
    width="720"
    @close="emit('close')"
  >
    <a-spin :spinning="loading || running !== ''" :tip="running ? `${running}中…` : undefined">
      <template v-if="node">
        <a-descriptions :column="2" size="small" bordered>
          <a-descriptions-item label="状态">
            <StatusTag :status="node.status" kind="node" />
          </a-descriptions-item>
          <a-descriptions-item label="最后心跳">
            {{ formatRelative(node.last_heartbeat_at) }}
          </a-descriptions-item>
          <a-descriptions-item label="SSH">
            {{ node.ssh_user }}@{{ node.host }}:{{ node.ssh_port }}
          </a-descriptions-item>
          <a-descriptions-item label="代理端口">
            <template v-if="node.listen_port === node.proxy_port">{{ node.proxy_port }}</template>
            <template v-else>
              公网 {{ node.proxy_port }} → 主机 {{ node.listen_port }}
              <div class="hint">需自行配置端口转发,面板只让 sing-box 监听主机端口</div>
            </template>
          </a-descriptions-item>
          <a-descriptions-item label="架构">{{ node.arch || '未探测' }}</a-descriptions-item>
          <a-descriptions-item label="sing-box">
            {{ node.singbox_version || '未安装' }}
          </a-descriptions-item>
          <a-descriptions-item label="构建标签" :span="2">
            <span class="mono">{{ node.singbox_build_tags || '—' }}</span>
          </a-descriptions-item>
          <a-descriptions-item label="握手目标">
            {{ node.reality_dest }}:{{ node.reality_dest_port }}
          </a-descriptions-item>
          <a-descriptions-item label="最大 TLS 记录">
            <span v-if="!node.handshake_max_record_size" class="muted">未检测</span>
            <span v-else :class="node.handshake_max_record_size > 8192 ? 'bad' : 'good'">
              {{ node.handshake_max_record_size }} / 8192
            </span>
          </a-descriptions-item>
          <a-descriptions-item label="配置版本">{{ node.config_revision }}</a-descriptions-item>
          <a-descriptions-item label="已部署配置">
            <span class="mono" :title="node.deployed_config_sha256">
              {{ shortHash(node.deployed_config_sha256) }}
            </span>
          </a-descriptions-item>
        </a-descriptions>

        <div class="actions">
          <a-space wrap>
            <a-button size="small" @click="doTestSSH">测试 SSH</a-button>
            <a-button size="small" @click="openBootstrap">重新引导</a-button>
            <a-button size="small" @click="doProbe">探测</a-button>
            <a-button
              size="small"
              @click="doInstall"
            >
              安装 sing-box
            </a-button>
            <a-button size="small" @click="doScanDests">扫描握手目标</a-button>
            <a-button size="small" @click="doDiff">比对配置</a-button>
            <a-button size="small" type="primary" @click="doDeploy">部署</a-button>
            <a-button
              size="small"
              @click="run('同步流量', () => api.syncNodeTraffic(nodeId!), '流量已同步')"
            >
              同步流量
            </a-button>
            <a-button size="small" @click="doCollectMetrics">采集资源</a-button>
            <a-button size="small" danger @click="confirmRestart">重启服务</a-button>
            <a-button size="small" danger @click="confirmResetHostKey">重置主机密钥</a-button>
            <a-button size="small" danger @click="confirmUninstall">卸载服务</a-button>
          </a-space>
        </div>

        <div class="section-title">资源</div>
        <a-empty
          v-if="!metrics"
          :image="undefined"
          description="还没有采样。采集按固定间隔在后台进行,也可以点上面的「采集资源」立刻取一次。"
        />
        <template v-else>
          <a-row :gutter="16" class="metric-row">
            <a-col :span="8">
              <div class="metric-title">CPU</div>
              <a-progress
                :percent="Math.round(metrics.cpu_percent)"
                :status="usageStatus(metrics.cpu_percent)"
                size="small"
              />
            </a-col>
            <a-col :span="8">
              <div class="metric-title">内存</div>
              <a-progress
                :percent="Math.round(memPercent(metrics))"
                :status="usageStatus(memPercent(metrics))"
                size="small"
              />
              <div class="hint tabular">
                {{ formatBytes(metrics.mem_used_kb * 1024) }} /
                {{ formatBytes(metrics.mem_total_kb * 1024) }}
              </div>
            </a-col>
            <a-col :span="8">
              <div class="metric-title">磁盘(根分区)</div>
              <a-progress
                :percent="Math.round(diskPercent(metrics))"
                :status="usageStatus(diskPercent(metrics))"
                size="small"
              />
              <div class="hint tabular">
                {{ formatBytes(metrics.disk_used_kb * 1024) }} /
                {{ formatBytes(metrics.disk_total_kb * 1024) }}
              </div>
            </a-col>
          </a-row>
          <a-descriptions :column="2" size="small" bordered class="block">
            <a-descriptions-item label="网络速率">
              <span class="tabular">
                ↓ {{ formatBytes(metrics.net_rx_bps) }}/s ↑ {{ formatBytes(metrics.net_tx_bps) }}/s
              </span>
            </a-descriptions-item>
            <a-descriptions-item label="1 分钟负载">
              <span class="tabular">{{ metrics.load1.toFixed(2) }}</span>
            </a-descriptions-item>
            <a-descriptions-item label="已运行">
              {{ formatUptime(metrics.uptime_seconds) }}
            </a-descriptions-item>
            <a-descriptions-item label="采样时间">
              {{ formatRelative(metrics.collected_at) }}
              <span class="hint">(区间内共 {{ metricsHistory.length }} 次)</span>
            </a-descriptions-item>
          </a-descriptions>

          <div class="section-title">
            资源趋势
            <a-radio-group v-model:value="metricsHours" size="small" class="hours">
              <a-radio-button :value="6">6 小时</a-radio-button>
              <a-radio-button :value="24">24 小时</a-radio-button>
              <a-radio-button :value="72">3 天</a-radio-button>
              <a-radio-button :value="168">7 天</a-radio-button>
            </a-radio-group>
          </div>
          <div class="chart-label">CPU 使用率</div>
          <MetricsChart
            :series="cpuSeries"
            :labels="metricLabels"
            :format="formatPercent"
            :max-override="100"
            :height="140"
          />
          <div class="chart-label">内存使用率</div>
          <MetricsChart
            :series="memSeries"
            :labels="metricLabels"
            :format="formatPercent"
            :max-override="100"
            :height="140"
          />
          <div class="chart-label">网络速率</div>
          <MetricsChart
            :series="netSeries"
            :labels="metricLabels"
            :format="formatRate"
            :height="140"
          />
        </template>

        <a-alert
          v-if="probe && probe.problems.length > 0"
          type="error"
          show-icon
          class="block"
          message="探测发现问题"
        >
          <template #description>
            <div v-for="p in probe.problems" :key="p">{{ p }}</div>
          </template>
        </a-alert>
        <a-descriptions v-else-if="probe" :column="2" size="small" bordered class="block">
          <a-descriptions-item label="系统">{{ probe.os_name }}</a-descriptions-item>
          <a-descriptions-item label="内核">{{ probe.kernel }}</a-descriptions-item>
          <a-descriptions-item label="内存">{{ probe.mem_total_mb }} MB</a-descriptions-item>
          <a-descriptions-item label="init 系统">
            <template v-if="probe.init_system">
              {{ probe.init_system }}
              <span class="hint">{{ probe.init_version }}</span>
            </template>
            <span v-else class="hint">未检测到</span>
          </a-descriptions-item>
          <a-descriptions-item label="含 v2ray_api" :span="2">
            {{ probe.has_v2ray_api ? '是' : '否 —— 流量统计将无法工作' }}
          </a-descriptions-item>
        </a-descriptions>

        <template v-if="destResults.length > 0">
          <div class="section-title">握手目标检测结果</div>
          <a-table
            :columns="[
              { title: '目标', dataIndex: 'server', key: 'server' },
              { title: '最大记录', dataIndex: 'max_record_size', key: 'size', width: 110 },
              { title: '曲线', dataIndex: 'curve_name', key: 'curve', width: 100 },
              { title: 'ALPN', dataIndex: 'alpn', key: 'alpn', width: 80 },
              { title: '', key: 'action', width: 80 },
            ]"
            :data-source="destResults"
            row-key="server"
            size="small"
            :pagination="false"
          >
            <template #bodyCell="{ column, record }">
              <template v-if="column.key === 'server'">
                <span :class="record.usable ? '' : 'bad'">{{ record.server }}</span>
                <div v-if="record.problems.length" class="muted small">
                  {{ record.problems.join(';') }}
                </div>
              </template>
              <template v-else-if="column.key === 'size'">
                <span class="tabular" :class="record.max_record_size > 8192 ? 'bad' : ''">
                  {{ record.max_record_size }}
                </span>
              </template>
              <template v-else-if="column.key === 'action'">
                <a v-if="record.usable" @click="applyDest(record.server)">应用</a>
                <span v-else class="muted">不可用</span>
              </template>
            </template>
          </a-table>
        </template>

        <template v-if="diff">
          <div class="section-title">配置差异</div>
          <a-alert
            :type="diff.in_sync ? 'success' : 'warning'"
            show-icon
            :message="diff.in_sync ? '节点配置与期望状态一致' : '节点配置与期望状态不一致'"
            :description="diff.diff.summary"
          />
          <div class="diff-users">
            期望用户:
            <a-tag v-for="u in diff.desired_users ?? []" :key="u">{{ u }}</a-tag>
            <span v-if="!diff.desired_users?.length" class="muted">(无)</span>
          </div>
        </template>

        <div class="section-title">最近 30 天流量</div>
        <TrafficChart :data="daily" />

        <div class="section-title">部署记录</div>
        <a-empty v-if="deployments.length === 0" description="尚未部署过" :image="undefined" />
        <a-collapse v-else accordion>
          <a-collapse-panel v-for="d in deployments" :key="d.id">
            <template #header>
              <span class="deploy-head">
                revision {{ d.revision }}
                <StatusTag :status="d.status" kind="deploy" />
                <span class="muted small">{{ formatTime(d.started_at) }}</span>
              </span>
            </template>
            <DeployStepList :record="d" />
          </a-collapse-panel>
        </a-collapse>
      </template>
    </a-spin>
  </a-drawer>

  <a-modal
    v-model:open="bootstrapOpen"
    title="重新引导节点"
    ok-text="开始引导"
    cancel-text="取消"
    width="560"
    @ok="doBootstrap"
  >
    <p class="modal-text">
      把面板专用公钥装进节点的 authorized_keys,装完会用它真连一次做验证。
      节点上已经有这把公钥时不会重复追加。
    </p>
    <a-form layout="vertical">
      <a-form-item
        label="节点登录密码"
        extra="留空则改用主控本机 ~/.ssh 与 /etc/litebox/keys 下的私钥去装。密码只用这一次,不会保存。"
      >
        <a-input-password
          v-model:value="bootstrapPassword"
          autocomplete="new-password"
          placeholder="留空表示用主控本机私钥"
        />
      </a-form-item>
    </a-form>
  </a-modal>
</template>

<style scoped>
.chart-label {
  margin: 12px 0 4px;
  font-size: 12px;
  color: rgb(0 0 0 / 45%);
}

.hours {
  margin-left: auto;
}

.actions {
  margin: 16px 0;
}

.hint {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.metric-row {
  margin: 8px 0 4px;
}

.metric-title {
  margin-bottom: 4px;
  font-size: 12px;
  color: rgb(0 0 0 / 65%);
}

.modal-text {
  margin: 0 0 16px;
  color: rgb(0 0 0 / 65%);
}

.block {
  margin-top: 16px;
}

.section-title {
  margin: 20px 0 8px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 8px;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
  word-break: break-all;
}

.muted {
  color: rgb(0 0 0 / 45%);
}

.small {
  font-size: 12px;
}

.tabular {
  font-variant-numeric: tabular-nums;
}

.good {
  color: #389e0d;
}

.bad {
  color: #cf1322;
}

.diff-users {
  margin-top: 8px;
  font-size: 13px;
}

.deploy-head {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
</style>
