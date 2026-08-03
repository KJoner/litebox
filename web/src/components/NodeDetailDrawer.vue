<script setup lang="ts">
import { ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ConfigDiff,
  type DailyPoint,
  type DeploymentRecord,
  type DestCheckResult,
  type Node,
  type ProbeResult,
} from '@/api/client'
import { formatRelative, formatTime, shortHash } from '@/utils/format'
import StatusTag from '@/components/StatusTag.vue'
import TrafficChart from '@/components/TrafficChart.vue'
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
            <a-button size="small" @click="doProbe">探测</a-button>
            <a-button
              size="small"
              @click="run('安装', () => api.installNode(nodeId!), 'sing-box 与 systemd 单元已就绪')"
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
            <a-button size="small" danger @click="confirmRestart">重启服务</a-button>
            <a-button size="small" danger @click="confirmResetHostKey">重置主机密钥</a-button>
          </a-space>
        </div>

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
          <a-descriptions-item label="systemd">{{ probe.systemd_version }}</a-descriptions-item>
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
</template>

<style scoped>
.actions {
  margin: 16px 0;
}

.hint {
  color: rgb(0 0 0 / 45%);
  font-size: 12px;
}

.block {
  margin-top: 16px;
}

.section-title {
  margin: 20px 0 8px;
  font-weight: 500;
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
