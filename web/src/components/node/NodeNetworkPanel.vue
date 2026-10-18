<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
import {
  familyText,
  probeMuted,
  probeValue,
  protoChip,
  rateName,
  rateOf,
  rateText,
  rateTitle,
  rttText,
} from '@/components/asset/probeView'

/**
 * 节点详情「网络监测」Tab,两块,数据分开存、分开算、分开显示:
 *
 *   - 出站网络质量:这台机器在本机测系统设置里的公共目标(ICMP / TCP)。这里只有本机的开关
 *     (跟随全局 / 启用 / 停用),目标本身在系统设置里建;
 *   - 入口连通性:主控对这台机器的管理通道与订阅地址做 TCP 建连。目标只能从下拉里选 ——
 *     候选是这台机器已登记的地址与端口,不收自由输入(那会让面板变成一个扫描器)。
 *
 * 打开这一页只读库,不触发任何一次探测。
 */
const props = defineProps<{ nodeId: number }>()

const data = ref<NodeProbe | null>(null)
const loading = ref(false)
const loadError = ref('')

const allSeries = computed(() => [...(data.value?.outbound ?? []), ...(data.value?.targets ?? [])])

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await api.nodeProbe(props.nodeId)
    if (selected.value == null || !allSeries.value.some((t) => t.id === selected.value)) {
      // 默认看第一条生效的出站序列;没有就看入口的第一个对外服务。
      const out = data.value.outbound.find((t) => t.enabled)
      const ts = data.value.targets
      selected.value = (out ?? ts.find((t) => t.kind === 'SERVICE') ?? ts[0])?.id ?? null
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
  if (!p.enabled) return '公共目标已全局暂停'
  return p.apply_all ? '全局:对全部节点启用' : '全局:没有对全部节点启用'
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
const isICMP = computed(() => selectedTarget.value?.protocol === 'ICMP')

function stateMeta(t: ProbeSummary): LbStatusMeta {
  switch (t.state) {
    case 'OK':
      return metaOf.ok('正常')
    case 'LOSSY':
      return metaOf.warn('部分丢包')
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
      <a-button size="small" :loading="loading" @click="load">
        <template #icon><LbIcon name="refresh" :size="14" /></template>
        刷新
      </a-button>
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

      <!-- ① 出站网络质量 -->
      <div class="nn__sec" data-testid="probe-outbound">
        <div class="nn__sec-head">
          <h3 class="nn__sec-title">
            出站网络质量
            <LbInfoTip
              :width="360"
              text="这台机器在本机测公共目标:ICMP 看往返时间与丢包率(按包),TCP 看建连时间与失败率。延迟在 VPS 上量,不含 SSH 的耗时。面板经已有的 SSH 连接临时启动一个辅助程序(/opt/litebox/litebox-probe,跑完就退出、不常驻、不监听端口);节点正在部署或同步时那一轮让路。采集失败(面板连不上这台机器)、工具不可用、DNS 失败都没有发起,不计入丢包率。"
            />
          </h3>
          <span class="nn__muted">节点本机 · 公共目标在<RouterLink to="/settings#probe-public">系统设置</RouterLink>里配置</span>
        </div>
        <div v-if="data.probe.outbound_available && missingHelpers.length" class="nn__note">
          <LbIcon name="info" :size="13" />主控缺少 {{ missingHelpers.join('、') }} 架构的辅助程序,那种架构的机器上会显示「工具不可用」(执行
          scripts/build-probe.sh)
        </div>
        <LbEmptyState
          v-if="!data.public.length"
          variant="empty"
          title="还没有公共探测目标"
          description="在系统设置里登记公共目标(比如公共 DNS 的 ICMP、某个网站的 TCP 443),再选择对哪些节点启用。升级不会替任何机器自动开启探测。"
        />
        <ul v-else class="nn__list">
          <li
            v-for="p in data.public"
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
                <span class="lb-chip">{{ p.protocol === 'ICMP' ? 'ICMP' : `TCP ${p.port}` }}</span>
                <span class="lb-chip">{{ familyText(p.family) }}</span>
                <span class="nn__addr lb-mono">{{ p.host }}</span>
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
                <span class="nn__muted lb-tabular" :title="rateTitle(seriesByPublic.get(p.id)!.h1, p.protocol, '近 1 小时')">
                  1h {{ rateName(p.protocol) }} {{ rateText(rateOf(seriesByPublic.get(p.id)!.h1, p.protocol)) }}
                  · 24h {{ rateText(rateOf(seriesByPublic.get(p.id)!.h24, p.protocol)) }}
                </span>
                <span v-if="seriesByPublic.get(p.id)!.state !== 'OK'" class="nn__muted">
                  {{ seriesByPublic.get(p.id)!.state_text }}
                </span>
              </div>
              <ProbeDots
                :dots="seriesByPublic.get(p.id)!.dots"
                :seconds="seriesByPublic.get(p.id)!.dot_seconds"
                :protocol="p.protocol"
                :muted="probeMuted(seriesByPublic.get(p.id)!.state)"
              />
            </template>
            <div v-else class="nn__muted">
              {{ p.effective ? '已启用,等待第一次探测' : p.mode === 'OFF' ? '这台机器上已停用' : inheritText(p) + ',这台机器不测' }}
              <template v-if="seriesByPublic.get(p.id) && !p.effective">· 历史保留,可点名称查看</template>
            </div>
          </li>
        </ul>
      </div>

      <!-- ② 入口连通性 -->
      <div class="nn__sec" data-testid="probe-ingress">
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
          <li v-for="t in data.targets" :key="t.id" class="nn__row" :class="{ 'nn__row--on': t.id === selected }">
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
            <ProbeDots :dots="t.dots" :seconds="t.dot_seconds" protocol="TCP" :muted="probeMuted(t.state)" />
            <div v-if="t.state === 'INVALID' || t.state === 'STALE' || t.state === 'STOPPED'" class="nn__muted">{{ t.state_text }}</div>
            <div class="nn__ops">
              <button type="button" class="lb-linkbtn" @click="toggle(t)">{{ t.enabled ? '暂停' : '恢复' }}</button>
              <button type="button" class="lb-linkbtn nn__danger" @click="remove(t)">删除</button>
            </div>
          </li>
        </ul>
      </div>

      <!-- 历史 -->
      <div v-if="selectedTarget" class="nn__hist">
        <div class="nn__hist-head">
          <div class="nn__hist-title">
            {{ selectedTarget.label }}
            <span class="nn__muted">
              · {{ selectedTarget.direction === 'OUTBOUND' ? '出站' : '入口' }} · {{ protoChip(selectedTarget) }} ·
              {{ familyText(selectedTarget.family) }} · {{ selectedTarget.source_label }}
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
          <ProbeChart :buckets="history.buckets" :protocol="selectedTarget.protocol" />
          <div class="nn__stats lb-tabular">
            <template v-if="isICMP">
              <div><span>丢包率(按包)</span><b>{{ rateText(history.stats.loss_rate) }}</b></div>
              <div><span>发 / 收</span><b>{{ history.stats.sent }} / {{ history.stats.received }}</b></div>
              <div><span>整轮全丢</span><b>{{ history.stats.failures }} / {{ history.stats.attempts }} 轮</b></div>
            </template>
            <template v-else>
              <div><span>失败率</span><b>{{ rateText(history.stats.failure_rate) }}</b></div>
              <div><span>TCP 连接次数</span><b>{{ history.stats.attempts }}</b></div>
              <div><span>失败</span><b>{{ history.stats.failures }}</b></div>
            </template>
            <div><span>平均 RTT</span><b>{{ rttText(history.stats.rtt_avg_ms) }}</b></div>
            <div><span>最小 / 最大</span><b>{{ rttText(history.stats.rtt_min_ms) }} / {{ rttText(history.stats.rtt_max_ms) }}</b></div>
            <div><span>DNS 失败(不计入)</span><b>{{ history.stats.dns_errors }}</b></div>
            <div v-if="selectedTarget.direction === 'OUTBOUND'">
              <span>采集失败 / 工具不可用(不计入)</span><b>{{ history.stats.ssh_errors }} / {{ history.stats.tool_errors }}</b>
            </div>
            <div><span>未执行(关机等)</span><b>{{ history.stats.skipped }}</b></div>
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
                  f.status === 'SSH_ERROR' || f.status === 'TOOL_ERROR' || f.status === 'SKIPPED'
                    ? 'nn__muted'
                    : f.status === 'DNS_ERROR' || f.status === 'LOSS' || f.status === 'BLOCKED'
                      ? 'nn__warn'
                      : 'nn__bad'
                "
              >
                {{ f.label }}
              </span>
              <span class="nn__muted">
                <template v-if="f.status === 'LOSS' || (isICMP && f.sent)">{{ f.received }}/{{ f.sent }} 收到 · </template>{{ f.error }}
              </span>
            </div>
          </div>
        </template>
      </div>
    </template>
    <div v-else class="nn__pad"><a-skeleton active :paragraph="{ rows: 3 }" /></div>
  </section>
</template>

<style scoped>
.nn__pad {
  padding: var(--sp-4) var(--sp-5);
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
