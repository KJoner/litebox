<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Modal, message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type NodeProbe,
  type ProbeCandidate,
  type ProbeHistory,
  type ProbeSummary,
} from '@/api/client'
import { LbEmptyState, LbIcon, LbInfoTip, LbStatusTag, metaOf, type LbStatusMeta } from '@/components/lb'
import { formatRelative, formatTime } from '@/utils/format'
import ProbeDots from '@/components/asset/ProbeDots.vue'
import ProbeChart from '@/components/asset/ProbeChart.vue'
import { familyText, rateText, rttText } from '@/components/asset/assetModel'

/**
 * 节点详情「网络监测」Tab:这台机器的 TCP 探测目标与历史。
 *
 * 目标只能从下拉里选 —— 候选是这台机器已登记的管理通道与订阅地址条目,不收自由输入的地址与端口
 * (那会让这个面板变成一个每分钟去连任意地址的扫描器)。打开这一页只读库,不触发任何一次探测。
 */
const props = defineProps<{ nodeId: number }>()

const data = ref<NodeProbe | null>(null)
const loading = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    data.value = await api.nodeProbe(props.nodeId)
    if (selected.value == null || !data.value.targets.some((t) => t.id === selected.value)) {
      // 默认看第一个对外服务:用户连的是它,管理通道排在后面。
      const ts = data.value.targets
      selected.value = (ts.find((t) => t.kind === 'SERVICE') ?? ts[0])?.id ?? null
    }
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取探测目标失败'
  } finally {
    loading.value = false
  }
}

// ---------- 新增目标 ----------

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

// ---------- 暂停 / 删除 ----------

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
    title: `删除监测目标「${t.label}」?`,
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

// ---------- 历史 ----------

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

const selectedTarget = computed(() => data.value?.targets.find((t) => t.id === selected.value) ?? null)

function stateMeta(t: ProbeSummary): LbStatusMeta {
  switch (t.state) {
    case 'OK':
      return metaOf.ok('正常')
    case 'FAILING':
      return metaOf.warn('最近一次失败')
    case 'DOWN':
      return metaOf.bad(`连续 ${t.consecutive_failures} 次失败`)
    case 'PAUSED':
      return metaOf.paused('已暂停')
    case 'DISABLED':
      return metaOf.mute('节点已停用', 'square')
    case 'STOPPED':
      return metaOf.mute('已关机', 'square')
    case 'INVALID':
      return metaOf.bad('目标已失效', 'triangle')
    case 'STALE':
      return { text: '数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    default:
      return metaOf.mute('等待首次探测', 'ring')
  }
}
</script>

<template>
  <section class="lb-card lb-card--flush nn" aria-label="网络监测">
    <div class="lb-card__head">
      <div>
        <h2 class="lb-card__title">
          网络监测
          <LbInfoTip
            :width="340"
            text="主控按固定间隔对目标端口做一次 TCP 三次握手,记下耗时与成败。TCP 能连上只说明端口可连接,不等于代理协议可用(那是部署里的真实拨测),也不是 ICMP 丢包。探测失败只推送,不会自动重启、重新部署或停机。"
          />
        </h2>
        <div class="lb-card__desc">
          <template v-if="data">
            来源:{{ data.probe.sources.map((s) => s.label).join('、') }} · 每 {{ data.probe.interval_sec }} 秒 · 超时
            {{ data.probe.timeout_ms }} ms
            <template v-if="data.probe.alert_after">· 连续 {{ data.probe.alert_after }} 次失败推送</template>
            <template v-else>· 不推送</template>
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
            <div class="lb-notice__title">TCP 探测已在系统设置里全局关闭</div>
            <div class="lb-notice__text">目标与历史都保留;在「系统设置 → TCP 探测」里重新打开后继续。</div>
          </div>
          <div class="lb-notice__actions"><RouterLink to="/settings">去设置</RouterLink></div>
        </div>
      </div>

      <!-- 新增 -->
      <div class="nn__add">
        <a-select
          v-model:value="pick"
          class="nn__pick"
          placeholder="选择要监测的地址与端口"
          :disabled="!freeCandidates.length"
          aria-label="选择监测目标"
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

      <LbEmptyState
        v-if="!data.targets.length"
        variant="empty"
        title="还没有监测目标"
        description="从上面选一个地址与端口开始。升级不会替任何机器自动建目标,面板不会凭空去连你的机器。"
      />
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
            <span class="nn__rtt lb-tabular" :title="t.state_text">
              {{ t.last?.status === 'OK' ? rttText(t.last.rtt_ms) : t.last?.label ?? '—' }}
            </span>
            <span class="nn__muted lb-tabular">1h {{ rateText(t.h1.failure_rate) }} · 24h {{ rateText(t.h24.failure_rate) }}</span>
          </div>
          <ProbeDots :dots="t.dots" :seconds="t.dot_seconds" :muted="t.state === 'PAUSED' || t.state === 'INVALID'" />
          <div v-if="t.state === 'INVALID' || t.state === 'STALE' || t.state === 'STOPPED'" class="nn__muted">{{ t.state_text }}</div>
          <div class="nn__ops">
            <button type="button" class="lb-linkbtn" @click="toggle(t)">{{ t.enabled ? '暂停' : '恢复' }}</button>
            <button type="button" class="lb-linkbtn nn__danger" @click="remove(t)">删除</button>
          </div>
        </li>
      </ul>

      <!-- 历史 -->
      <div v-if="selectedTarget" class="nn__hist">
        <div class="nn__hist-head">
          <div class="nn__hist-title">
            {{ selectedTarget.label }}
            <span class="nn__muted">· {{ familyText(selectedTarget.family) }} · {{ selectedTarget.source_label }}</span>
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
          <ProbeChart :buckets="history.buckets" />
          <div class="nn__stats lb-tabular">
            <div><span>失败率</span><b>{{ rateText(history.stats.failure_rate) }}</b></div>
            <div><span>TCP 连接次数</span><b>{{ history.stats.attempts }}</b></div>
            <div><span>失败</span><b>{{ history.stats.failures }}</b></div>
            <div><span>平均 RTT</span><b>{{ rttText(history.stats.rtt_avg_ms) }}</b></div>
            <div><span>最小 / 最大</span><b>{{ rttText(history.stats.rtt_min_ms) }} / {{ rttText(history.stats.rtt_max_ms) }}</b></div>
            <div><span>DNS 失败(不计入分母)</span><b>{{ history.stats.dns_errors }}</b></div>
            <div><span>未执行(关机等)</span><b>{{ history.stats.skipped }}</b></div>
            <div>
              <span>上次成功</span>
              <b>{{ selectedTarget.last_success_at ? formatRelative(selectedTarget.last_success_at) : '—' }}</b>
            </div>
          </div>
          <div class="nn__muted">
            统计口径从 {{ formatTime(selectedTarget.generation_since) }} 起(地址或端口变更后重新计);
            每格 {{ Math.round(history.bucket_seconds / 60) }} 分钟。
          </div>
          <div v-if="history.failures.length" class="nn__fails">
            <div class="nn__fails-title">最近的失败</div>
            <div v-for="(f, i) in history.failures.slice(0, 10)" :key="i" class="nn__fail lb-tabular">
              <span>{{ formatTime(f.at) }}</span>
              <span :class="f.status === 'DNS_ERROR' ? 'nn__warn' : 'nn__bad'">{{ f.label }}</span>
              <span class="nn__muted">{{ f.error }}</span>
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
.nn__add {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-4) var(--sp-5) var(--sp-2);
}
.nn__pick {
  flex: 1 1 320px;
  max-width: 560px;
}
.nn__note {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  margin: 0 var(--sp-5) var(--sp-2);
  font-size: var(--fs-xs);
  color: var(--warn);
}
.nn__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nn__list {
  list-style: none;
  margin: 0;
  padding: var(--sp-2) var(--sp-5) var(--sp-3);
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
