<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Modal, message } from 'ant-design-vue'
import { api, ApiError, type BoceTaskView, type BoceVantage, type NodeBoce, type ProbeSummary } from '@/api/client'
import { LbIcon, LbInfoTip, LbStatusTag, metaOf, type LbStatusMeta } from '@/components/lb'
import { formatRelative, formatTime } from '@/utils/format'
import ProbeDots from '@/components/asset/ProbeDots.vue'
import { probeMuted, probeValue, rateOf, rateText, rateTitle } from '@/components/asset/probeView'
import BoceTaskFormModal from './BoceTaskFormModal.vue'
import BoceRunsModal from './BoceRunsModal.vue'
import { newRequestID, points, runStateMeta, scheduleText, yuan } from './boceText'

/**
 * 节点详情「网络监测」里的 Boce 外部入站拨测:这台节点的任务、每个拨测点的序列、手动检测与执行记录。
 *
 * 打开这里只读库;会花钱的只有「手动检测」(先确认、经预算、重复点击只算一次)。
 * 任务的增删改、开关、暂停都不会创建付费检测。
 */
const props = defineProps<{
  nodeId: number
  /** 这台节点的 Boce 序列汇总(来自 GET /api/nodes/{id}/probe 的 boce 字段)。 */
  series: ProbeSummary[]
  selected: number | null
  /** 关键词(名称 / 地区 / 运营商)。 */
  keyword: string
}>()
const emit = defineEmits<{ (e: 'select', id: number): void; (e: 'changed'): void }>()

const data = ref<NodeBoce | null>(null)
const vantages = ref<BoceVantage[]>([])
const loading = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [n, v] = await Promise.all([api.nodeBoce(props.nodeId), api.boceVantages()])
    data.value = n
    vantages.value = v.vantages
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取 Boce 任务失败'
  } finally {
    loading.value = false
  }
}
watch(() => props.nodeId, () => void load(), { immediate: true })

// 手动检测之后结果是异步出的(10 秒 ~ 2 分钟):隔几次刷新一下,有界。
const timers: ReturnType<typeof setTimeout>[] = []
function refreshLater() {
  for (const ms of [15000, 45000, 130000]) {
    timers.push(
      setTimeout(() => {
        void load()
        emit('changed')
      }, ms),
    )
  }
}
onBeforeUnmount(() => timers.forEach(clearTimeout))

const kw = computed(() => props.keyword.trim().toLowerCase())
const seriesBy = computed(() => {
  const m = new Map<number, ProbeSummary[]>()
  for (const s of props.series) {
    const list = m.get(s.boce_task_id) ?? []
    list.push(s)
    m.set(s.boce_task_id, list)
  }
  return m
})
function rowMatch(s: ProbeSummary): boolean {
  if (!kw.value) return true
  return `${s.label} ${s.boce?.isp_name ?? ''} ${s.boce?.vantage_name ?? ''} ${s.boce?.task_name ?? ''}`.toLowerCase().includes(kw.value)
}
const shownTasks = computed(() =>
  (data.value?.tasks ?? []).filter(
    (t) =>
      !kw.value ||
      `${t.name} ${t.target_label}`.toLowerCase().includes(kw.value) ||
      (seriesBy.value.get(t.id) ?? []).some(rowMatch),
  ),
)
function rowsOf(t: BoceTaskView): ProbeSummary[] {
  const list = seriesBy.value.get(t.id) ?? []
  const match = list.filter(rowMatch)
  return match.length || !kw.value ? match : list
}

// ---------- 节点暂停 ----------

const savingPause = ref(false)
async function setPaused(v: boolean) {
  savingPause.value = true
  try {
    await api.setNodeBocePaused(props.nodeId, v)
    message.success(v ? '已暂停这台节点的 Boce 自动检测(手动检测仍可用,历史保留)' : '已恢复这台节点的 Boce 自动检测')
    await load()
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingPause.value = false
  }
}

// ---------- 任务操作 ----------

const formOpen = ref(false)
const formTask = ref<BoceTaskView | null>(null)
const formCopy = ref(false)
function openCreate() {
  formTask.value = null
  formCopy.value = false
  formOpen.value = true
}
function openEdit(t: BoceTaskView) {
  formTask.value = t
  formCopy.value = false
  formOpen.value = true
}
function openCopy(t: BoceTaskView) {
  formTask.value = t
  formCopy.value = true
  formOpen.value = true
}
async function onSaved() {
  formOpen.value = false
  await load()
  emit('changed')
}

const busy = ref<number | null>(null)
async function setEnabled(t: BoceTaskView, v: boolean) {
  busy.value = t.id
  try {
    await api.setBoceTaskEnabled(t.id, v)
    message.success(v ? `已开启「${t.name}」的自动检测` : `已关闭「${t.name}」的自动检测(历史保留)`)
    await load()
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    busy.value = null
  }
}

function runNow(t: BoceTaskView) {
  const rid = newRequestID()
  const est = t.points_per_round
  Modal.confirm({
    title: `立即检测一次「${t.name}」?`,
    content:
      `这会马上创建一次付费检测:${t.vantage_ids.length} 个拨测点,估算 ${est} 波点(约 ${yuan(est * (data.value?.point_price ?? 0.002))})。\n` +
      '同样经过预算检查;重复点击、刷新重发只算一次。结果约 10 秒 ~ 2 分钟出来。',
    okText: '检测',
    cancelText: '取消',
    onOk: () => {
      void doRun(t, rid)
    },
  })
}

async function doRun(t: BoceTaskView, rid: string) {
  busy.value = t.id
  try {
    const r = await api.runBoceTask(t.id, rid)
    switch (r.state) {
      case 'POLLING':
        message.success(`已创建 Boce 任务(估算 ${r.points} 波点),结果出来后自动刷新`)
        refreshLater()
        break
      case 'SKIPPED':
        message.warning(`没有执行:${r.error.replace(/^BUDGET:/, '')}`)
        break
      case 'UNKNOWN':
        message.warning(r.error || '创建结果不确定:为避免重复扣费不会重发')
        break
      default:
        message.error(r.error || r.state_text)
    }
    await load()
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '检测失败')
  } finally {
    busy.value = null
  }
}

async function archive(t: BoceTaskView, v: boolean) {
  busy.value = t.id
  try {
    await api.archiveBoceTask(t.id, v)
    message.success(v ? `已归档「${t.name}」:不再执行,历史保留` : `已恢复「${t.name}」:自动检测仍是关的,需要时再打开`)
    await load()
    emit('changed')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '操作失败')
  } finally {
    busy.value = null
  }
}

function remove(t: BoceTaskView) {
  Modal.confirm({
    title: `删除 Boce 任务「${t.name}」?`,
    content: '这个任务的序列与检测历史(明细与小时汇总)会一起删除;执行记录与估算消耗保留(预算不会因此多出来)。只想停下用「归档」。',
    okText: '删除',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void (async () => {
        try {
          await api.deleteBoceTask(t.id)
          message.success('已删除')
          await load()
          emit('changed')
        } catch (err) {
          message.error(err instanceof ApiError ? err.message : '删除失败')
        }
      })()
    },
  })
}

const runsOpen = ref(false)
const runsTask = ref<BoceTaskView | null>(null)
function openRuns(t: BoceTaskView) {
  runsTask.value = t
  runsOpen.value = true
}

function taskMeta(t: BoceTaskView): LbStatusMeta {
  if (t.archived_at) return metaOf.mute('已归档', 'square')
  if (t.auto_paused_reason) return metaOf.warn('已自动停下')
  if (t.target_error) return metaOf.bad('目标已失效', 'triangle')
  if (!t.enabled) return metaOf.paused('自动检测关')
  if (t.auto_reason) return metaOf.paused('暂不执行')
  return metaOf.ok('自动检测开')
}

function seriesMeta(s: ProbeSummary): LbStatusMeta {
  switch (s.state) {
    case 'OK':
      return metaOf.ok('正常')
    case 'LOSSY':
      return metaOf.warn('部分丢包')
    case 'FAILING':
      return metaOf.warn('最近一次连不上')
    case 'DOWN':
      return metaOf.bad(`连续 ${s.consecutive_failures} 轮失败`)
    case 'VANTAGE_ERROR':
      return { text: '拨测点异常', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'API_ERROR':
      return { text: '接口异常', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'EXPIRED':
      return { text: '未取得结果', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'SKIPPED':
      return metaOf.paused('未执行')
    case 'PAUSED':
      return metaOf.paused('未在测')
    case 'STALE':
      return { text: '数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
    case 'INVALID':
      return metaOf.bad('目标已失效', 'triangle')
    case 'STOPPED':
      return metaOf.mute('已关机', 'square')
    case 'DISABLED':
      return metaOf.mute('节点已停用', 'square')
  }
  return metaOf.mute('等待首次检测', 'ring')
}
</script>

<template>
  <div class="nbp" data-testid="probe-boce">
    <div class="nbp__head">
      <h3 class="nbp__title">
        外部入站拨测(Boce)
        <LbInfoTip
          :width="380"
          text="Boce 是付费的第三方拨测服务:从它在各省各运营商(以及海外)的拨测点对这台机器的订阅入口做 TCPing,看「从外面某个地区连进来」怎么样。请求由主控发起,节点上什么都不装。比例叫 TCPing 丢包率(按 Boce 报告的包数),与主控的 TCP 连接失败率、节点的 ICMP 丢包率是不同的口径;拨测点异常、接口异常、未取得结果都不计入。"
        />
      </h3>
      <label class="nbp__pause">
        <a-switch
          :checked="data?.paused ?? false"
          size="small"
          :loading="savingPause"
          :disabled="!data"
          aria-label="暂停本机 Boce 自动检测"
          @change="(v: unknown) => setPaused(!!v)"
        />
        <span>暂停本机自动检测</span>
      </label>
      <a-button size="small" class="nbp__new" data-testid="boce-task-new" :disabled="!data" @click="openCreate">
        <template #icon><LbIcon name="plus" :size="14" /></template>
        新建拨测任务
      </a-button>
    </div>

    <div v-if="loadError" class="lb-error-strip">{{ loadError }}</div>
    <template v-else-if="data">
      <div v-if="data.blocked" class="nbp__note">
        <LbIcon name="info" :size="13" />{{ data.blocked }} —— 不会创建任何付费检测。
        <RouterLink to="/settings#set-boce">去设置</RouterLink>
      </div>
      <div v-if="!data.tasks.length" class="nbp__muted nbp__empty">
        这台节点还没有 Boce 拨测任务。新建任务时选订阅入口、拨测点与时间表;新任务默认不开自动检测,看过费用估算再打开。
      </div>
      <div v-else-if="!shownTasks.length" class="nbp__muted nbp__empty">没有符合筛选条件的任务</div>
      <ul v-else class="nbp__tasks">
        <li v-for="t in shownTasks" :key="t.id" class="nbp__task" :data-testid="`boce-task-${t.id}`">
          <div class="nbp__task-top">
            <div class="nbp__task-name">
              <b class="lb-ellipsis">{{ t.name }}</b>
              <LbStatusTag :meta="taskMeta(t)" />
            </div>
            <a-switch
              v-if="!t.archived_at"
              :checked="t.enabled"
              size="small"
              :loading="busy === t.id"
              :aria-label="`${t.name} 自动检测`"
              @change="(v: unknown) => setEnabled(t, !!v)"
            />
          </div>
          <div class="nbp__meta lb-tabular">
            <span class="lb-chip">TCPing</span>
            <span class="lb-mono nbp__muted">{{ t.target_host ? `${t.target_host}:${t.target_port}` : t.ref }}</span>
            <span>{{ t.vantage_ids.length }} 个拨测点</span>
            <span>{{ scheduleText(t) }}({{ t.timezone }})</span>
            <span>本月已用 {{ points(t.month_points) }} · 预计 {{ points(t.plan_month_points) }} / 月</span>
            <span v-if="t.month_points_limit">任务上限 {{ points(t.month_points_limit) }} / 月</span>
          </div>
          <div class="nbp__meta lb-tabular">
            <span v-if="t.next_run_at">下次 {{ formatTime(t.next_run_at) }}</span>
            <span v-else-if="t.auto_reason" class="nbp__muted">{{ t.auto_reason }}</span>
            <template v-if="t.last_run">
              <span>上次 {{ formatRelative(t.last_run.created_at) }}</span>
              <LbStatusTag :meta="runStateMeta(t.last_run.state)" />
            </template>
            <span v-else class="nbp__muted">还没有执行过</span>
          </div>
          <div v-if="t.target_error" class="nbp__warn">{{ t.target_error }}</div>
          <div v-if="t.auto_paused_reason" class="nbp__warn">{{ t.auto_paused_reason }}</div>
          <div v-else-if="t.last_error && t.last_state !== 'DONE'" class="nbp__muted">{{ t.last_error }}</div>

          <ul v-if="rowsOf(t).length" class="nbp__series">
            <li
              v-for="s in rowsOf(t)"
              :key="s.id"
              class="nbp__row"
              :class="{ 'nbp__row--on': s.id === selected }"
              :data-testid="`boce-series-${s.id}`"
            >
              <button type="button" class="nbp__sel" :aria-pressed="s.id === selected" @click="emit('select', s.id)">
                <span class="nbp__label lb-ellipsis">{{ s.boce?.vantage_name ?? s.label }}</span>
                <span v-if="s.boce?.area === 'OVERSEA'" class="lb-chip">海外</span>
                <span v-if="s.boce?.missing" class="lb-chip nbp__chip-warn" title="已从 Boce 节点列表里消失;不会被自动换成别的付费拨测点">已从列表消失</span>
                <LbStatusTag :meta="seriesMeta(s)" />
                <span v-if="s.last && (s.last.status === 'OK' || s.last.status === 'LOSS')" class="nbp__rtt lb-tabular" :title="s.state_text">
                  {{ probeValue(s) }}
                </span>
                <span class="nbp__muted lb-tabular" :title="rateTitle(s.h24, 'BOCE', '近 24 小时')">
                  24h TCPing 丢包 {{ rateText(rateOf(s.h24, 'BOCE')) }}
                </span>
              </button>
              <ProbeDots :dots="s.dots" :seconds="s.dot_seconds" metric="BOCE" :muted="probeMuted(s.state)" />
              <div v-if="s.state !== 'OK' && s.state !== 'PENDING'" class="nbp__muted">{{ s.state_text }}</div>
            </li>
          </ul>

          <div class="nbp__ops">
            <button v-if="!t.archived_at" type="button" class="lb-linkbtn" :disabled="busy === t.id" @click="runNow(t)">
              手动检测
            </button>
            <button type="button" class="lb-linkbtn" @click="openRuns(t)">执行记录</button>
            <button v-if="!t.archived_at" type="button" class="lb-linkbtn" @click="openEdit(t)">编辑</button>
            <button type="button" class="lb-linkbtn" @click="openCopy(t)">复制</button>
            <button type="button" class="lb-linkbtn" @click="archive(t, !t.archived_at)">{{ t.archived_at ? '恢复' : '归档' }}</button>
            <button type="button" class="lb-linkbtn nbp__danger" @click="remove(t)">删除</button>
          </div>
        </li>
      </ul>
    </template>
    <a-skeleton v-else-if="loading" active :paragraph="{ rows: 2 }" />

    <BoceTaskFormModal
      v-if="data"
      :open="formOpen"
      :node-id="nodeId"
      :node="data"
      :vantages="vantages"
      :task="formTask"
      :copy="formCopy"
      @close="formOpen = false"
      @saved="onSaved"
    />
    <BoceRunsModal :open="runsOpen" :task="runsTask" :vantages="vantages" @close="runsOpen = false" />
  </div>
</template>

<style scoped>
.nbp {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.nbp__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2) var(--sp-3);
}
.nbp__title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}
.nbp__pause {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.nbp__new {
  margin-left: auto;
}
.nbp__note {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.nbp__tasks,
.nbp__series {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.nbp__task {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: var(--sp-3);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
}
.nbp__task-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.nbp__task-name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
.nbp__meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.nbp__row {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: var(--sp-2);
  border-radius: var(--r-sm);
  background: var(--surface2);
}
.nbp__row--on {
  outline: 1px solid var(--brand-border);
  background: var(--brand-bg);
}
.nbp__sel {
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
.nbp__label {
  font-weight: 600;
  max-width: 100%;
}
.nbp__rtt {
  font-weight: 600;
  font-size: var(--fs-sm);
}
.nbp__chip-warn {
  color: var(--warn);
  background: var(--warn-bg);
}
.nbp__ops {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-3);
  font-size: var(--fs-sm);
}
.nbp__danger {
  color: var(--bad);
}
.nbp__warn {
  font-size: var(--fs-xs);
  color: var(--warn);
}
.nbp__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.nbp__empty {
  padding: var(--sp-2) 0;
}
</style>
