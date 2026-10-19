<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { Modal, message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type NodeProbe,
  type NodePublicProbe,
  type ProbeBindingMode,
  type ProbeCandidate,
  type ProbeHistory,
  type ProbeSummary,
} from '@/api/client'
import { LbEmptyState, LbIcon, LbInfoTip, LbStatusTag, metaOf, type LbStatusMeta } from '@/components/lb'
import { formatRelative, formatTime } from '@/utils/format'
import ProbeDots from '@/components/asset/ProbeDots.vue'
import ProbeChart from '@/components/asset/ProbeChart.vue'
import NodeTargetPicker from '@/components/probe/NodeTargetPicker.vue'
import CardDisplayModal from '@/components/probe/CardDisplayModal.vue'
import NodeBocePanel from '@/components/boce/NodeBocePanel.vue'
import {
  familyText,
  ispText,
  originText,
  probeMuted,
  probeValue,
  protoChip,
  rateFullName,
  rateName,
  rateOf,
  rateText,
  rateTitle,
  rttText,
} from '@/components/asset/probeView'

/**
 * 节点详情「网络监测」Tab,三块,数据分开存、分开算、分开显示:
 *
 *   - 出站网络质量:这台机器在本机测目标库里的公共目标(ICMP / TCP 建连 / TCP 握手近似)。
 *     这里有本机的开关(跟随全局 / 启用 / 停用)、从目标库里挑目标、试测,以及本机的覆盖
 *     (暂停本机、改间隔 —— 只能往「少测」那边改:全局关闭与来源暂停对每一台都生效);
 *     目标本身在「探测目标库」里建;
 *   - 入口连通性:主控对这台机器的管理通道与订阅地址做 TCP 建连。目标只能从下拉里选 ——
 *     候选是这台机器已登记的地址与端口,不收自由输入(那会让面板变成一个扫描器);
 *   - 外部入站拨测(Boce,付费):Boce 的拨测点从各地区 / 运营商对订阅入口做 TCPing,按任务的时间表执行。
 *
 * 顶上的筛选按来源(节点本机 / 主控 / Boce)与关键词过滤三块的行;「卡片展示设置」决定仪表盘卡片上
 * 入站、出站各显示哪 3 项 —— 只改展示,不改检测。打开这一页只读库,不触发任何一次探测、不调 Boce。
 */
const props = defineProps<{ nodeId: number }>()

const data = ref<NodeProbe | null>(null)
const loading = ref(false)
const loadError = ref('')

const allSeries = computed(() => [
  ...(data.value?.outbound ?? []),
  ...(data.value?.targets ?? []),
  ...(data.value?.boce ?? []),
])

// ---------- 筛选:来源 + 关键词(只过滤显示,不影响检测) ----------

type SourceFilter = 'ALL' | 'NODE' | 'MASTER' | 'BOCE'
const sourceFilter = ref<SourceFilter>('ALL')
const keyword = ref('')
const sourceOptions: { value: SourceFilter; label: string }[] = [
  { value: 'ALL', label: '全部来源' },
  { value: 'NODE', label: '节点本机(出站)' },
  { value: 'MASTER', label: '主控(入站)' },
  { value: 'BOCE', label: 'Boce(入站)' },
]
function showSource(s: SourceFilter): boolean {
  return sourceFilter.value === 'ALL' || sourceFilter.value === s
}
function kwMatch(...parts: (string | undefined)[]): boolean {
  const k = keyword.value.trim().toLowerCase()
  return !k || parts.join(' ').toLowerCase().includes(k)
}

const cardOpen = ref(false)
async function onCardSaved() {
  cardOpen.value = false
  await load()
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await api.nodeProbe(props.nodeId)
    nodeForm.paused = data.value.node_settings.paused
    nodeForm.interval = data.value.node_settings.interval_sec || null
    if (selected.value == null || !allSeries.value.some((t) => t.id === selected.value)) {
      // 默认看第一条生效的出站序列;没有就看入口的第一个对外服务。
      const out = data.value.outbound.find((t) => t.enabled)
      const ts = data.value.targets
      selected.value =
        (out ?? ts.find((t) => t.kind === 'SERVICE') ?? ts[0] ?? data.value.boce.find((t) => t.enabled))?.id ?? null
    }
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取探测数据失败'
  } finally {
    loading.value = false
  }
}

// ---------- 出站:本机开关 ----------

/** 每个公共目标在这台机器上的序列(没有生效过就没有)。 */
const seriesByPublic = computed(() => {
  const m = new Map<number, ProbeSummary>()
  for (const s of data.value?.outbound ?? []) m.set(s.public_id, s)
  return m
})

const modeText: Record<ProbeBindingMode, string> = { INHERIT: '跟随全局', ON: '启用', OFF: '停用' }

function inheritText(p: NodePublicProbe): string {
  if (p.pause_reason) return p.pause_reason
  return p.apply_all ? '全局:对全部节点启用' : '全局:没有对全部节点启用'
}

/**
 * 这一块列哪些目标:在测的、本机显式开 / 关过的、有过历史的,以及「全部节点」但被暂停了的。
 * 目标库可以有几百项,全列出来就找不到这台机器真正在测的那几个了;其余的在「选择公共目标」里挑。
 */
const shownPublics = computed(() =>
  (data.value?.public ?? []).filter(
    (p) =>
      (p.effective || p.mode !== 'INHERIT' || seriesByPublic.value.has(p.id) || (p.apply_all && !!p.node_pause_reason)) &&
      kwMatch(p.name, p.host, p.province, p.city, p.region, p.isp, ispText(p.isp)),
  ),
)
const shownTargets = computed(() => (data.value?.targets ?? []).filter((t) => kwMatch(t.label, t.resolved_host)))

function rowNote(p: NodePublicProbe): string {
  if (p.effective) return '已启用,等待第一次探测'
  if (p.node_pause_reason) return p.node_pause_reason + ',这台机器不测'
  if (p.mode === 'OFF') return '这台机器上已停用'
  return inheritText(p) + ',这台机器不测'
}

// ---------- 出站:本机覆盖 ----------

const nodeForm = reactive({ paused: false, interval: null as number | null })
const savingNode = ref(false)
async function saveNodeSettings() {
  savingNode.value = true
  try {
    await api.setNodeProbeSettings(props.nodeId, { paused: nodeForm.paused, interval_sec: nodeForm.interval ?? 0 })
    message.success(nodeForm.paused ? '已暂停这台机器的出站探测(历史保留)' : '已保存')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
    if (data.value) {
      nodeForm.paused = data.value.node_settings.paused
      nodeForm.interval = data.value.node_settings.interval_sec || null
    }
  } finally {
    savingNode.value = false
  }
}

const pickerOpen = ref(false)
async function onPicked() {
  pickerOpen.value = false
  await load()
}

const savingBinding = ref<number | null>(null)
async function setMode(p: NodePublicProbe, mode: ProbeBindingMode) {
  if (mode === p.mode) return
  savingBinding.value = p.id
  try {
    await api.setProbeBinding(props.nodeId, p.id, mode)
    message.success(`「${p.name}」在这台机器上改为${modeText[mode]}`)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingBinding.value = null
  }
}

// ---------- 入口:新增目标 ----------

const pick = ref<string | undefined>(undefined)
const adding = ref(false)
const candKey = (c: ProbeCandidate) => `${c.ref}|${c.family}`
const freeCandidates = computed(() => {
  const used = new Set((data.value?.targets ?? []).map((t) => `${t.ref}|${t.family}`))
  return (data.value?.candidates ?? []).filter((c) => !used.has(candKey(c)))
})
const picked = computed(() => freeCandidates.value.find((c) => candKey(c) === pick.value) ?? null)

async function addTarget() {
  const c = picked.value
  if (!c) return
  adding.value = true
  try {
    const t = await api.createProbeTarget(props.nodeId, { ref: c.ref, family: c.family })
    message.success(`已开始监测「${t.label}」,下一轮探测后出现数据`)
    pick.value = undefined
    selected.value = t.id
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '添加失败')
  } finally {
    adding.value = false
  }
}

// ---------- 入口:暂停 / 删除 ----------

async function toggle(t: ProbeSummary) {
  try {
    await api.updateProbeTarget(t.id, { enabled: !t.enabled })
    message.success(t.enabled ? '已暂停监测(历史保留,不再告警)' : '已恢复监测')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  }
}

function remove(t: ProbeSummary) {
  Modal.confirm({
    title: `删除入口监测目标「${t.label}」?`,
    content: '这个目标的探测历史(明细与小时汇总)会一起删除。只想暂时停下用「暂停」,历史会保留。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void (async () => {
        try {
          await api.deleteProbeTarget(t.id)
          message.success('已删除')
          await load()
        } catch (err) {
          message.error(err instanceof ApiError ? err.message : '删除失败')
        }
      })()
    },
  })
}

// ---------- 历史(两块共用一个) ----------

const selected = ref<number | null>(null)
const range = ref<'1h' | '24h' | '7d'>('24h')
const history = ref<ProbeHistory | null>(null)
const historyError = ref('')
const historyLoading = ref(false)
let seq = 0

async function loadHistory() {
  const id = selected.value
  history.value = null
  historyError.value = ''
  if (id == null) return
  const my = ++seq
  historyLoading.value = true
  try {
    const h = await api.probeHistory(id, range.value)
    if (my === seq) history.value = h
  } catch (err) {
    if (my === seq) historyError.value = err instanceof ApiError ? err.message : '读取历史失败'
  } finally {
    if (my === seq) historyLoading.value = false
  }
}

watch(() => props.nodeId, () => {
  selected.value = null
  void load()
}, { immediate: true })
watch([selected, range], () => void loadHistory())

const selectedTarget = computed(() => allSeries.value.find((t) => t.id === selected.value) ?? null)
const selMetric = computed(() => selectedTarget.value?.metric ?? 'CONNECT')

function stateMeta(t: ProbeSummary): LbStatusMeta {
  switch (t.state) {
    case 'OK':
      return metaOf.ok('正常')
    case 'LOSSY':
      return metaOf.warn(t.metric === 'SYN' ? '部分无应答' : '部分丢包')
    case 'FAILING':
      return metaOf.warn('最近一次失败')
    case 'DOWN':
      return metaOf.bad(`连续 ${t.consecutive_failures} 次失败`)
    case 'PAUSED':
      return metaOf.paused(t.direction === 'OUTBOUND' ? '未启用' : '已暂停')
    case 'DISABLED':
      return metaOf.mute('节点已停用', 'square')
    case 'STOPPED':
      return metaOf.mute('已关机', 'square')
    case 'INVALID':
      return metaOf.bad('目标已失效', 'triangle')
    case 'STALE':
      return { text: '数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'COLLECT_FAILED':
      return { text: '采集失败', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'TOOL_ERROR':
      return metaOf.warn('工具不可用', 'triangle')
    case 'BLOCKED':
      return metaOf.warn('目标受限', 'triangle')
    case 'VANTAGE_ERROR':
      return { text: '拨测点异常', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'API_ERROR':
      return { text: '接口异常', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'EXPIRED':
      return { text: '未取得结果', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'SKIPPED':
      return metaOf.paused('未执行')
    default:
      return metaOf.mute('等待首次探测', 'ring')
  }
}

const missingHelpers = computed(() => {
  const h = data.value?.probe.helpers ?? {}
  return ['amd64', 'arm64'].filter((a) => !h[a])
})
</script>

<template>
  <section class="lb-card lb-card--flush nn" aria-label="网络监测">
    <div class="lb-card__head">
      <div>
        <h2 class="lb-card__title">网络监测</h2>
        <div class="lb-card__desc">
          <template v-if="data">
            <template v-if="data.probe.alert_after">连续 {{ data.probe.alert_after }} 次失败推送</template>
            <template v-else>不推送</template>
            · 探测失败只推送,不会自动重启、重新部署或停机
          </template>
        </div>
      </div>
      <div class="nn__head-ops">
        <a-button size="small" data-testid="card-display-open" :disabled="!data" @click="cardOpen = true">
          <template #icon><LbIcon name="layout-grid" :size="14" /></template>
          卡片展示设置
        </a-button>
        <a-button size="small" :loading="loading" @click="load">
          <template #icon><LbIcon name="refresh" :size="14" /></template>
          刷新
        </a-button>
      </div>
    </div>

    <div v-if="loadError" class="nn__pad"><div class="lb-error-strip">{{ loadError }}</div></div>
    <template v-else-if="data">
      <div v-if="!data.probe.enabled" class="nn__pad">
        <div class="lb-notice">
          <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
          <div class="lb-notice__body">
            <div class="lb-notice__title">网络探测已在系统设置里全局关闭</div>
            <div class="lb-notice__text">目标与历史都保留;在「系统设置 → 网络探测」里重新打开后继续。</div>
          </div>
          <div class="lb-notice__actions"><RouterLink to="/settings#tcpprobe">去设置</RouterLink></div>
        </div>
      </div>

      <div class="lb-filter nn__filter" data-testid="probe-filter">
        <a-select v-model:value="sourceFilter" class="nn__filter-sel" aria-label="来源" :dropdown-match-select-width="false">
          <a-select-option v-for="o in sourceOptions" :key="o.value" :value="o.value">{{ o.label }}</a-select-option>
        </a-select>
        <a-input v-model:value="keyword" allow-clear size="small" class="nn__filter-q" placeholder="搜索目标 / 地区 / 运营商" aria-label="搜索测量目标">
          <template #prefix><LbIcon name="search" :size="13" /></template>
        </a-input>
      </div>

      <!-- ① 出站网络质量 -->
      <div v-if="showSource('NODE')" class="nn__sec" data-testid="probe-outbound">
        <div class="nn__sec-head">
          <h3 class="nn__sec-title">
            出站网络质量
            <LbInfoTip
              :width="380"
              text="这台机器在本机测公共目标:ICMP 看往返时间与真实丢包率(按包);TCP 建连看建连时间与连接失败率;TCP 握手近似看握手超时无应答率(基于系统 TCP 建连,不是原始套接字的 SYN 探测,不是丢包率)。延迟在 VPS 上量,不含 SSH 的耗时。面板经已有的 SSH 连接临时启动一个辅助程序(/opt/litebox/litebox-probe,跑完就退出、不常驻、不监听端口);节点正在部署或同步时那一轮让路。采集失败(面板连不上这台机器)、工具不可用、DNS 失败都没有发起,不计入任何比例。"
            />
          </h3>
          <span class="nn__muted">节点本机 · 目标在<RouterLink to="/probe-library">探测目标库</RouterLink>里配置</span>
          <a-button size="small" class="nn__pick-btn" data-testid="probe-pick-open" :disabled="!data.public.length" @click="pickerOpen = true">
            <template #icon><LbIcon name="plus" :size="14" /></template>
            选择公共目标
          </a-button>
        </div>
        <div class="nn__node" data-testid="probe-node-settings">
          <label class="nn__node-item">
            <a-switch v-model:checked="nodeForm.paused" size="small" :loading="savingNode" @change="saveNodeSettings" />
            <span>暂停本机出站探测</span>
            <LbInfoTip :width="300" text="只停这一台,目标与历史都保留。单台节点只能暂停、不能强开:全局关闭与来源暂停对每一台都生效。" />
          </label>
          <label class="nn__node-item">
            <span>本机探测间隔</span>
            <a-input-number
              v-model:value="nodeForm.interval"
              size="small"
              :min="30"
              :max="3600"
              :precision="0"
              placeholder="跟随各目标"
              addon-after="秒"
              class="nn__node-num"
            />
            <a-button size="small" :loading="savingNode" @click="saveNodeSettings">保存</a-button>
            <LbInfoTip :width="300" text="留空 = 跟随各目标自己的间隔。设了之后这台机器上全部出站目标都按它,但不能低于各自来源的下限(第三方来源默认 300 秒)。" />
          </label>
        </div>
        <div v-if="data.probe.outbound_available && missingHelpers.length" class="nn__note">
          <LbIcon name="info" :size="13" />主控缺少 {{ missingHelpers.join('、') }} 架构的辅助程序,那种架构的机器上会显示「工具不可用」(执行
          scripts/build-probe.sh)
        </div>
        <LbEmptyState
          v-if="!data.public.length"
          variant="empty"
          title="目标库还是空的"
          description="在「探测目标库」里登记目标(自己的机器、或者从 Zstatic / TcpQuality 挑几个),再选择对哪些节点启用。升级不会替任何机器自动开启探测。"
        />
        <div v-else-if="!shownPublics.length" class="nn__muted nn__empty">
          这台机器还没有启用任何公共目标。点「选择公共目标」从目标库里挑;第三方目标要先在目标库里确认授权。
        </div>
        <ul v-else class="nn__list">
          <li
            v-for="p in shownPublics"
            :key="p.id"
            class="nn__row"
            :class="{ 'nn__row--on': seriesByPublic.get(p.id)?.id === selected && selected != null }"
            :data-testid="`probe-public-${p.id}`"
          >
            <div class="nn__row-top">
              <button
                type="button"
                class="nn__sel"
                :disabled="!seriesByPublic.get(p.id)"
                :aria-pressed="seriesByPublic.get(p.id)?.id === selected"
                @click="selected = seriesByPublic.get(p.id)?.id ?? selected"
              >
                <span class="nn__label lb-ellipsis">{{ p.name }}</span>
                <span class="lb-chip">{{ protoChip(p) }}</span>
                <span class="lb-chip">{{ familyText(p.family) }}</span>
                <span v-if="p.third_party" class="lb-chip">{{ originText(p.source) }}</span>
                <span class="nn__addr lb-mono">{{ p.host }}</span>
                <span class="nn__muted lb-tabular">每 {{ p.node_interval_sec }} 秒</span>
              </button>
              <div class="lb-seg lb-seg--sm nn__mode" role="radiogroup" :aria-label="`${p.name} 在这台机器上`">
                <button
                  v-for="m in (['INHERIT', 'ON', 'OFF'] as const)"
                  :key="m"
                  type="button"
                  role="radio"
                  class="lb-seg__item"
                  :class="{ 'lb-seg__item--on': p.mode === m }"
                  :aria-checked="p.mode === m"
                  :disabled="savingBinding === p.id"
                  :title="m === 'INHERIT' ? inheritText(p) : undefined"
                  @click="setMode(p, m)"
                >
                  {{ modeText[m] }}
                </button>
              </div>
            </div>
            <template v-if="seriesByPublic.get(p.id) && p.effective">
              <div class="nn__state">
                <LbStatusTag :meta="stateMeta(seriesByPublic.get(p.id)!)" />
                <span class="nn__rtt lb-tabular" :title="seriesByPublic.get(p.id)!.state_text">
                  {{ probeValue(seriesByPublic.get(p.id)!) }}
                </span>
                <span class="nn__muted lb-tabular" :title="rateTitle(seriesByPublic.get(p.id)!.h1, p.metric, '近 1 小时')">
                  1h {{ rateName(p.metric) }} {{ rateText(rateOf(seriesByPublic.get(p.id)!.h1, p.metric)) }}
                  · 24h {{ rateText(rateOf(seriesByPublic.get(p.id)!.h24, p.metric)) }}
                </span>
                <span v-if="seriesByPublic.get(p.id)!.state !== 'OK'" class="nn__muted">
                  {{ seriesByPublic.get(p.id)!.state_text }}
                </span>
              </div>
              <ProbeDots
                :dots="seriesByPublic.get(p.id)!.dots"
                :seconds="seriesByPublic.get(p.id)!.dot_seconds"
                :metric="p.metric"
                :muted="probeMuted(seriesByPublic.get(p.id)!.state)"
              />
            </template>
            <div v-else class="nn__muted">
              {{ rowNote(p) }}
              <template v-if="seriesByPublic.get(p.id) && !p.effective">· 历史保留,可点名称查看</template>
            </div>
          </li>
        </ul>
      </div>

      <!-- ② 入口连通性 -->
      <div v-if="showSource('MASTER')" class="nn__sec" data-testid="probe-ingress">
        <div class="nn__sec-head">
          <h3 class="nn__sec-title">
            入口连通性
            <LbInfoTip
              :width="340"
              text="主控按固定间隔对这台机器的端口做一次 TCP 三次握手,记下耗时与成败。它回答的是「从主控连不连得上这台机器」,与上面的出站网络质量是两个问题;TCP 能连上只说明端口可连接,不等于代理协议可用。"
            />
          </h3>
          <span class="nn__muted">
            {{ data.probe.sources.find((s) => s.code === 'MASTER')?.label ?? '主控探测' }} · TCP · 每 {{ data.probe.interval_sec }} 秒 ·
            超时 {{ data.probe.timeout_ms }} ms
          </span>
        </div>
        <div class="nn__add">
          <a-select
            v-model:value="pick"
            class="nn__pick"
            placeholder="选择要监测的地址与端口"
            :disabled="!freeCandidates.length"
            aria-label="选择入口监测目标"
            show-search
            option-filter-prop="label"
          >
            <a-select-option
              v-for="c in freeCandidates"
              :key="candKey(c)"
              :value="candKey(c)"
              :label="`${c.label} ${familyText(c.family)} ${c.host}:${c.port}`"
            >
              {{ c.label }} · {{ familyText(c.family) }} · {{ c.host }}:{{ c.port }}
            </a-select-option>
          </a-select>
          <a-button type="primary" :loading="adding" :disabled="!picked" @click="addTarget">
            <template #icon><LbIcon name="plus" :size="14" /></template>
            开始监测
          </a-button>
          <span v-if="!freeCandidates.length" class="nn__muted">这台机器已登记的地址与端口都在监测了</span>
        </div>
        <div v-if="picked?.note" class="nn__note"><LbIcon name="info" :size="13" />{{ picked.note }}</div>

        <div v-if="!data.targets.length" class="nn__muted nn__empty">
          没有入口监测目标。从上面选一个地址与端口开始;升级不会替任何机器自动建目标。
        </div>
        <ul v-else class="nn__list">
          <li v-for="t in shownTargets" :key="t.id" class="nn__row" :class="{ 'nn__row--on': t.id === selected }">
            <button type="button" class="nn__sel" :aria-pressed="t.id === selected" @click="selected = t.id">
              <span class="nn__label lb-ellipsis">{{ t.label }}</span>
              <span class="lb-chip">{{ t.kind === 'MGMT' ? '管理通道' : '对外服务' }}</span>
              <span class="lb-chip">{{ familyText(t.family) }}</span>
              <span class="lb-chip">TCP</span>
              <span class="nn__addr lb-mono">{{ t.resolved_host }}:{{ t.resolved_port }}</span>
            </button>
            <div class="nn__state">
              <LbStatusTag :meta="stateMeta(t)" />
              <span class="nn__rtt lb-tabular" :title="t.state_text">{{ probeValue(t) }}</span>
              <span class="nn__muted lb-tabular">1h {{ rateText(t.h1.failure_rate) }} · 24h {{ rateText(t.h24.failure_rate) }}</span>
            </div>
            <ProbeDots :dots="t.dots" :seconds="t.dot_seconds" metric="CONNECT" :muted="probeMuted(t.state)" />
            <div v-if="t.state === 'INVALID' || t.state === 'STALE' || t.state === 'STOPPED'" class="nn__muted">{{ t.state_text }}</div>
            <div class="nn__ops">
              <button type="button" class="lb-linkbtn" @click="toggle(t)">{{ t.enabled ? '暂停' : '恢复' }}</button>
              <button type="button" class="lb-linkbtn nn__danger" @click="remove(t)">删除</button>
            </div>
          </li>
        </ul>
      </div>

      <!-- ③ 外部入站拨测(Boce) -->
      <div v-if="showSource('BOCE')" class="nn__sec">
        <NodeBocePanel
          :node-id="nodeId"
          :series="data.boce"
          :selected="selected"
          :keyword="keyword"
          @select="(id: number) => (selected = id)"
          @changed="load"
        />
      </div>

      <!-- 历史 -->
      <div v-if="selectedTarget" class="nn__hist">
        <div class="nn__hist-head">
          <div class="nn__hist-title">
            {{ selectedTarget.label }}
            <span class="nn__muted">
              · {{ selectedTarget.direction === 'OUTBOUND' ? '出站' : '入站' }} · {{ protoChip(selectedTarget) }} ·
              {{ familyText(selectedTarget.family) }} · {{ selectedTarget.source_label }}
              <template v-if="selectedTarget.boce"> · 任务「{{ selectedTarget.boce.task_name }}」</template>
            </span>
          </div>
          <div class="lb-seg lb-seg--sm" role="tablist" aria-label="时间范围">
            <button
              v-for="r in (['1h', '24h', '7d'] as const)"
              :key="r"
              type="button"
              role="tab"
              class="lb-seg__item"
              :class="{ 'lb-seg__item--on': range === r }"
              :aria-selected="range === r"
              @click="range = r"
            >
              {{ r === '1h' ? '1 小时' : r === '24h' ? '24 小时' : '7 天' }}
            </button>
          </div>
        </div>
        <div v-if="historyError" class="lb-error-strip">{{ historyError }}</div>
        <a-skeleton v-else-if="historyLoading && !history" active :paragraph="{ rows: 4 }" />
        <template v-else-if="history">
          <ProbeChart :buckets="history.buckets" :metric="selMetric" />
          <div class="nn__stats lb-tabular">
            <template v-if="selMetric === 'LOSS'">
              <div><span>{{ rateFullName('LOSS') }}(按包)</span><b>{{ rateText(history.stats.loss_rate) }}</b></div>
              <div><span>发 / 收</span><b>{{ history.stats.sent }} / {{ history.stats.received }}</b></div>
              <div><span>整轮全丢</span><b>{{ history.stats.failures }} / {{ history.stats.attempts }} 轮</b></div>
            </template>
            <template v-else-if="selMetric === 'SYN'">
              <div><span>{{ rateFullName('SYN') }}</span><b>{{ rateText(history.stats.loss_rate) }}</b></div>
              <div><span>发出 SYN / 有应答</span><b>{{ history.stats.sent }} / {{ history.stats.received }}</b></div>
              <div><span>整轮失败(含 RST)</span><b>{{ history.stats.failures }} / {{ history.stats.attempts }} 轮</b></div>
            </template>
            <template v-else-if="selMetric === 'BOCE'">
              <div><span>{{ rateFullName('BOCE') }}(按包)</span><b>{{ rateText(history.stats.loss_rate) }}</b></div>
              <div><span>TCPing 发 / 成功</span><b>{{ history.stats.sent }} / {{ history.stats.received }}</b></div>
              <div><span>整轮连不上</span><b>{{ history.stats.failures }} / {{ history.stats.attempts }} 轮</b></div>
            </template>
            <template v-else>
              <div><span>{{ rateFullName('CONNECT') }}</span><b>{{ rateText(history.stats.failure_rate) }}</b></div>
              <div><span>TCP 连接次数</span><b>{{ history.stats.attempts }}</b></div>
              <div><span>失败</span><b>{{ history.stats.failures }}</b></div>
            </template>
            <div><span>平均 RTT</span><b>{{ rttText(history.stats.rtt_avg_ms) }}</b></div>
            <div><span>最小 / 最大</span><b>{{ rttText(history.stats.rtt_min_ms) }} / {{ rttText(history.stats.rtt_max_ms) }}</b></div>
            <div v-if="selMetric !== 'BOCE'"><span>DNS 失败(不计入)</span><b>{{ history.stats.dns_errors }}</b></div>
            <div v-if="selMetric === 'BOCE'">
              <span>拨测点异常 / 接口异常 / 未取得结果(不计入)</span>
              <b>{{ history.stats.vantage_errors }} / {{ history.stats.api_errors }} / {{ history.stats.expired }}</b>
            </div>
            <div v-if="selectedTarget.direction === 'OUTBOUND'">
              <span>采集失败 / 工具不可用(不计入)</span><b>{{ history.stats.ssh_errors }} / {{ history.stats.tool_errors }}</b>
            </div>
            <div><span>未执行({{ selMetric === 'BOCE' ? '预算保护、关机等' : '关机等' }})</span><b>{{ history.stats.skipped }}</b></div>
            <div>
              <span>上次可达</span>
              <b>{{ selectedTarget.last_success_at ? formatRelative(selectedTarget.last_success_at) : '—' }}</b>
            </div>
          </div>
          <div class="nn__muted">
            统计口径从 {{ formatTime(selectedTarget.generation_since) }} 起(地址或端口变更后重新计);
            每格 {{ Math.round(history.bucket_seconds / 60) }} 分钟。
            <template v-if="selectedTarget.last?.peer">最近一次连的是 {{ selectedTarget.last.peer }}。</template>
          </div>
          <div v-if="history.failures.length" class="nn__fails">
            <div class="nn__fails-title">最近的异常</div>
            <div v-for="(f, i) in history.failures.slice(0, 10)" :key="i" class="nn__fail lb-tabular">
              <span>{{ formatTime(f.at) }}</span>
              <span
                :class="
                  f.status === 'SSH_ERROR' || f.status === 'TOOL_ERROR' || f.status === 'SKIPPED' ||
                  f.status === 'VANTAGE_ERROR' || f.status === 'API_ERROR' || f.status === 'EXPIRED'
                    ? 'nn__muted'
                    : f.status === 'DNS_ERROR' || f.status === 'LOSS' || f.status === 'BLOCKED'
                      ? 'nn__warn'
                      : 'nn__bad'
                "
              >
                {{ f.label }}
              </span>
              <span class="nn__muted">
                <template v-if="f.status === 'LOSS' || (selMetric !== 'CONNECT' && f.sent)">
                  {{ f.received }}/{{ f.sent }} {{ selMetric === 'SYN' ? '有应答' : selMetric === 'BOCE' ? '成功' : '收到' }} ·
                </template>{{ f.error }}
              </span>
            </div>
          </div>
        </template>
      </div>
    </template>
    <div v-else class="nn__pad"><a-skeleton active :paragraph="{ rows: 3 }" /></div>
    <NodeTargetPicker
      v-if="data"
      :open="pickerOpen"
      :node-id="nodeId"
      :publics="data.public"
      @close="pickerOpen = false"
      @saved="onPicked"
    />
    <CardDisplayModal
      v-if="data"
      :open="cardOpen"
      :node-id="nodeId"
      :prefs="data.card.prefs"
      :pick="data.card.pick"
      :ingress="[...data.targets, ...data.boce]"
      :outbound="data.outbound"
      @close="cardOpen = false"
      @saved="onCardSaved"
    />
  </section>
</template>

<style scoped>
.nn__pad {
  padding: var(--sp-4) var(--sp-5);
}
.nn__head-ops {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
}
.nn__filter {
  padding: var(--sp-3) var(--sp-5) 0;
  flex-wrap: wrap;
  gap: var(--sp-2);
}
.nn__filter-sel {
  width: 160px;
}
.nn__filter-q {
  width: 240px;
  max-width: 100%;
}
.nn__sec {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-4) var(--sp-5) var(--sp-3);
  border-top: 1px solid var(--sep2);
}
.nn__sec-head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--sp-2);
}
.nn__sec-title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}
.nn__pick-btn {
  margin-left: auto;
}
.nn__node {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2) var(--sp-5);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-sm);
  background: var(--surface2);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.nn__node-item {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
}
.nn__node-num {
  width: 150px;
}
.nn__add {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.nn__pick {
  flex: 1 1 320px;
  max-width: 560px;
}
.nn__note {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.nn__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nn__empty {
  padding: var(--sp-2) 0;
}
.nn__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.nn__row {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.nn__row--on {
  border-color: var(--brand-border);
  background: var(--brand-bg);
}
.nn__row-top {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.nn__sel {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  padding: 0;
  border: none;
  background: transparent;
  text-align: left;
  cursor: pointer;
  color: inherit;
  font: inherit;
  min-width: 0;
}
.nn__sel:disabled {
  cursor: default;
}
.nn__mode {
  flex: none;
}
.nn__label {
  font-weight: 600;
  color: var(--text);
  max-width: 100%;
}
.nn__addr {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nn__state {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.nn__rtt {
  font-weight: 600;
  font-size: var(--fs-sm);
}
.nn__ops {
  display: flex;
  gap: var(--sp-3);
  font-size: var(--fs-sm);
}
.nn__danger {
  color: var(--bad);
}
.nn__hist {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  padding: var(--sp-4) var(--sp-5);
  border-top: 1px solid var(--sep2);
}
.nn__hist-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.nn__hist-title {
  font-weight: 600;
}
.nn__stats {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: var(--sp-2);
}
.nn__stats > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-sm);
  background: var(--surface2);
}
.nn__stats span {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.nn__stats b {
  font-size: var(--fs-md);
  color: var(--text);
}
.nn__fails {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: var(--fs-sm);
}
.nn__fails-title {
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text2);
}
.nn__fail {
  display: grid;
  grid-template-columns: 128px 96px minmax(0, 1fr);
  gap: var(--sp-2);
}
.nn__bad {
  color: var(--bad);
}
.nn__warn {
  color: var(--warn);
}
@media (max-width: 767px) {
  .nn__fail {
    grid-template-columns: minmax(0, 1fr);
    gap: 0;
  }
}
</style>
