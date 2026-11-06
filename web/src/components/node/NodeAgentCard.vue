<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type Node } from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'
import { formatUTCTime } from '@/utils/format'

/**
 * 可选节点 Agent(VNext AGENT01–06)。这一版只做状态采集:节点主动 HTTPS 推给主控,
 * 握手之后资源采样由它写,SSH 那一路对这台机器让路;它不碰代理配置,不执行任何命令。
 * 默认不装;内存不足 256 MB 的机器上不装。卸载只移除 Agent 本身,回到 SSH。
 */
const props = defineProps<{ node: Node }>()

type AgentInfo = Awaited<ReturnType<typeof api.nodeAgent>>
const info = ref<AgentInfo | null>(null)
const loadError = ref('')
const busy = ref('')
const lastSteps = ref<string[]>([])
const lastNote = ref('')

const tooSmall = computed(() => (props.node.mem_total_mb ?? 0) > 0 && (props.node.mem_total_mb ?? 0) < (info.value?.min_memory_mb ?? 256))
const installed = computed(() => !!info.value?.installed)

async function load() {
  loadError.value = ''
  try {
    info.value = await api.nodeAgent(props.node.id)
  } catch (e) {
    loadError.value = e instanceof ApiError ? e.message : '读不到 Agent 状态'
  }
}
onMounted(load)
watch(() => props.node.id, load)

function install() {
  lbDangerConfirm({
    title: `在 ${props.node.name} 上启用 Agent?`,
    okText: '安装并启用',
    okType: 'primary',
    impacts: [
      '上传一个约 6 MB 的静态二进制,装成服务(systemd 带 48 MB 内存上限 / OpenRC 由 supervise-daemon 看着)',
      '它只做状态采集,由节点主动连主控的「订阅站点根」地址上报;不监听端口,不碰 sing-box 与它的配置',
      '收到第一次上报之后,这台机器的资源采样改由它写;它超过三个间隔没报,SSH 自动接手',
      '不重启任何代理服务、不断开任何连接;随时可以卸载回 SSH',
    ],
    onOk: () => {
      void run('正在安装 Agent', async () => {
        const r = await api.installNodeAgent(props.node.id)
        lastSteps.value = r.result?.steps ?? []
        lastNote.value = r.result?.note ?? ''
        if (r.error) message.error(r.error)
        else message.success(r.result.state === 'ACTIVE' ? 'Agent 已启用,资源采集改由它写' : 'Agent 已装上,还没收到它的上报')
      })
    },
  })
}

function uninstall() {
  lbDangerConfirm({
    title: `卸载 ${props.node.name} 上的 Agent?`,
    okText: '卸载',
    impacts: ['令牌先作废,资源采集立刻回到 SSH', '只删除 Agent 本身(服务定义、二进制、配置、本地队列),不动 sing-box 与它的配置'],
    onOk: () => {
      void run('正在卸载 Agent', async () => {
        const r = await api.uninstallNodeAgent(props.node.id)
        lastSteps.value = r.steps ?? []
        lastNote.value = ''
        if (r.error) message.error(r.error)
        else message.success('已卸载,资源采集回到 SSH')
      })
    },
  })
}

async function run(label: string, fn: () => Promise<void>) {
  busy.value = label
  try {
    await fn()
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '操作失败')
  } finally {
    busy.value = ''
    await load()
  }
}
</script>

<template>
  <!-- 只渲染卡片正文:卡片的外框与标题在 NodeDetailView 里(那里的 scoped 样式管不到子组件内部)。 -->
  <div class="ag">
    <div v-if="loadError" class="ag__note">{{ loadError }}</div>
    <template v-else-if="info">
      <div class="ag__note">资源采集现在由 <b>{{ info.metrics_writer === 'AGENT' ? 'Agent' : 'SSH' }}</b> 写</div>
      <div v-if="installed" class="ag__facts">
        <div><span>状态</span><b>{{ info.status.state === 'ACTIVE' ? '在上报' : '已安装,还没收到上报' }}</b></div>
        <div><span>版本</span><b class="lb-mono">{{ info.status.version || '—' }}</b></div>
        <div><span>最后上报</span><b>{{ info.status.last_seen_at ? formatUTCTime(info.status.last_seen_at) : '—' }}</b></div>
        <div><span>Agent 常驻内存</span><b class="lb-mono">{{ info.status.rss_kb ? `${(info.status.rss_kb / 1024).toFixed(1)} MiB` : '—' }}</b></div>
        <div v-if="info.status.dropped_total"><span>丢弃 / 不可信样本</span><b>{{ info.status.dropped_total }}</b></div>
      </div>
      <div v-else class="ag__note">
        没有启用。<template v-if="tooSmall">这台机器内存 {{ node.mem_total_mb }} MB,低于 {{ info.min_memory_mb }} MB,不装 Agent。</template>
      </div>
      <div v-if="info.status.last_error && installed" class="ag__warn">{{ info.status.last_error }}</div>
      <div v-if="lastNote" class="ag__warn">{{ lastNote }}</div>
      <ul v-if="lastSteps.length" class="ag__steps">
        <li v-for="s in lastSteps" :key="s">{{ s }}</li>
      </ul>
      <div class="ag__ops">
        <a-button v-if="!installed" size="small" :disabled="tooSmall || !!busy" :loading="!!busy" @click="install">启用 Agent</a-button>
        <a-button v-else size="small" :disabled="!!busy" :loading="!!busy" @click="uninstall">卸载 Agent</a-button>
        <span v-if="busy" class="ag__note">{{ busy }}…</span>
      </div>
    </template>
  </div>
</template>

<style scoped>
.ag__note {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ag__facts {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
  gap: var(--sp-2);
  font-size: var(--fs-xs);
}
.ag__facts span {
  display: block;
  color: var(--text2);
}
.ag__warn {
  margin-top: var(--sp-2);
  color: var(--warn);
  font-size: var(--fs-xs);
}
.ag__steps {
  margin: var(--sp-2) 0 0;
  padding-left: 18px;
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ag__ops {
  margin-top: var(--sp-2);
  display: flex;
  gap: var(--sp-2);
  align-items: center;
}
</style>
