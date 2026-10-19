<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Modal, message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type BoceOverview,
  type BoceScheduleSpec,
  type BoceSettingsInput,
  type BoceTaskView,
} from '@/api/client'
import { LbIcon, LbInfoTip, LbStatusTag, LbTimeText, metaOf, type LbStatusMeta } from '@/components/lb'
import { formatTime } from '@/utils/format'
import BoceScheduleEditor from './BoceScheduleEditor.vue'
import { COMMON_TIMEZONES, points, runStateMeta, scheduleText, specOf, yuan } from './boceText'

/**
 * 系统设置「Boce 外部拨测」:Key、连接状态、余额、拨测点同步、全局预算、默认计划、全局启停,
 * 以及全部任务的费用总览。
 *
 * **未明确启用不花钱**:默认关、日 / 月上限默认 0(= 没有设,不创建任何付费检测)。校验 Key、刷新余额、
 * 同步拨测点三个按钮调的是 Boce 的免费接口;这一页本身只读库。Key 只在服务端保存(主密钥加密),
 * 这里只看得到末 4 位。面板自己的估算消耗与 Boce 账户的真实余额分开显示 —— 余额可能被面板以外的调用消耗。
 */
const ov = ref<BoceOverview | null>(null)
const tasks = ref<BoceTaskView[]>([])
const loadError = ref('')
const loading = ref(false)

const form = reactive({
  enabled: false,
  api_key: '',
  points_per_check: 1,
  point_price: '0.002',
  daily_points: 0,
  monthly_points: 0,
  run_points: 10,
  alert_percent: 80,
  low_balance_points: 0,
  timezone: 'Asia/Shanghai',
})
const plan = ref<BoceScheduleSpec | null>(null)
const saving = ref(false)
const formError = ref('')

function fill(o: BoceOverview) {
  const s = o.settings
  Object.assign(form, {
    enabled: s.enabled,
    api_key: '',
    points_per_check: s.points_per_check,
    point_price: s.point_price,
    daily_points: s.daily_points,
    monthly_points: s.monthly_points,
    run_points: s.run_points,
    alert_percent: s.alert_percent,
    low_balance_points: s.low_balance_points,
    timezone: s.timezone,
  })
  plan.value = specOf(s.default_schedule)
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const [o, t] = await Promise.all([api.boceOverview(), api.boceTasks()])
    ov.value = o
    tasks.value = t.tasks
    fill(o)
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '读取 Boce 设置失败'
  } finally {
    loading.value = false
  }
}
onMounted(load)

const dirty = computed(() => {
  const s = ov.value?.settings
  if (!s || !plan.value) return false
  return (
    form.api_key.trim() !== '' ||
    form.enabled !== s.enabled ||
    form.points_per_check !== s.points_per_check ||
    form.point_price !== s.point_price ||
    form.daily_points !== s.daily_points ||
    form.monthly_points !== s.monthly_points ||
    form.run_points !== s.run_points ||
    form.alert_percent !== s.alert_percent ||
    form.low_balance_points !== s.low_balance_points ||
    form.timezone !== s.timezone ||
    JSON.stringify(plan.value) !== JSON.stringify(specOf(s.default_schedule))
  )
})

function body(apiKey?: string): BoceSettingsInput {
  return {
    enabled: form.enabled,
    points_per_check: form.points_per_check,
    point_price: String(form.point_price),
    daily_points: form.daily_points,
    monthly_points: form.monthly_points,
    run_points: form.run_points,
    alert_percent: form.alert_percent,
    low_balance_points: form.low_balance_points,
    timezone: form.timezone,
    default_schedule: plan.value!,
    ...(apiKey !== undefined ? { api_key: apiKey } : {}),
  }
}

async function doSave(apiKey?: string) {
  saving.value = true
  formError.value = ''
  try {
    await api.updateBoceSettings(body(apiKey))
    message.success('已保存(改动记进审计日志)')
    await load()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    saving.value = false
  }
}

function save() {
  const s = ov.value?.settings
  const key = form.api_key.trim() || undefined
  // 打开开关、提高上限:先说清楚会发生什么。
  const raising = s && (form.daily_points > s.daily_points || form.monthly_points > s.monthly_points)
  if (s && ((!s.enabled && form.enabled) || raising)) {
    const planned = ov.value?.plan
    Modal.confirm({
      title: !s.enabled && form.enabled ? '启用 Boce 外部拨测?' : '提高 Boce 预算上限?',
      content:
        (!s.enabled && form.enabled
          ? `启用之后,开着自动检测的任务会按时间表创建付费检测(当前估算约 ${planned?.others_month_points ?? 0} 波点 / 月,约 ${yuan(planned?.total_yuan_month)})。`
          : '') +
        `\n每日上限 ${form.daily_points}、每月上限 ${form.monthly_points} 波点;到上限就停止新建付费检测,结果查询与原有探测不受影响。这次改动会记进审计日志。`,
      okText: '确认保存',
      cancelText: '取消',
      onOk: () => {
        void doSave(key)
      },
    })
    return
  }
  void doSave(key)
}

function clearKey() {
  Modal.confirm({
    title: '清空 Boce API Key?',
    content: '清空之后不再创建任何付费检测,校验 / 余额 / 同步也用不了;已有的任务、历史与执行记录保留。',
    okText: '清空',
    okType: 'danger',
    cancelText: '取消',
    onOk: () => {
      void doSave('')
    },
  })
}

const verifying = ref(false)
async function verify() {
  verifying.value = true
  try {
    const b = await api.boceVerify()
    message.success(`校验通过:余额 ¥${b.balance},剩余 ${b.point} 波点`)
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '校验失败')
    await load()
  } finally {
    verifying.value = false
  }
}

const refreshing = ref(false)
async function refreshBalance() {
  refreshing.value = true
  try {
    await api.boceBalance()
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '查询余额失败')
    await load()
  } finally {
    refreshing.value = false
  }
}

const syncing = ref(false)
async function sync() {
  syncing.value = true
  try {
    const r = await api.boceSyncVantages()
    message.success(
      r.results.map((x) => `${x.area === 'OVERSEA' ? '海外' : '大陆'} ${x.total} 个(新增 ${x.added}、消失 ${x.missing})`).join(';'),
    )
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '同步失败,已保留原有缓存')
  } finally {
    syncing.value = false
    await load()
  }
}

const resuming = ref(false)
async function resume() {
  resuming.value = true
  try {
    await api.boceResume()
    message.success('已恢复:之后按时间表与预算照常执行')
    await load()
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '恢复失败')
  } finally {
    resuming.value = false
  }
}

const statusMeta = computed<LbStatusMeta>(() => {
  const o = ov.value
  if (!o) return metaOf.mute('读取中', 'ring')
  if (o.state.auto_paused) return metaOf.bad('已自动暂停', 'triangle')
  if (!o.settings.enabled) return metaOf.paused('未启用')
  if (o.blocked) return metaOf.warn('不会执行')
  return metaOf.ok('启用中')
})

const verifyMeta = computed<LbStatusMeta>(() => {
  const st = ov.value?.state.verify_state
  if (st === 'OK') return metaOf.ok('校验通过')
  if (st === 'ERROR') return metaOf.bad('校验失败', 'triangle')
  return metaOf.mute('还没校验', 'ring')
})

const tzOptions = computed(() => (COMMON_TIMEZONES.includes(form.timezone) ? COMMON_TIMEZONES : [form.timezone, ...COMMON_TIMEZONES]))

function taskMeta(t: BoceTaskView): LbStatusMeta {
  if (t.archived_at) return metaOf.mute('已归档', 'square')
  if (t.auto_reason) return metaOf.paused(t.enabled ? '暂不执行' : '自动检测关')
  return metaOf.ok('自动检测开')
}
</script>

<template>
  <div class="bsc" data-testid="boce-settings">
    <div v-if="loadError" class="lb-error-strip">{{ loadError }}</div>
    <template v-else-if="ov">
      <!-- 状态 -->
      <div class="lb-card bsc__card">
        <div class="bsc__status">
          <LbStatusTag :meta="statusMeta" />
          <span v-if="ov.blocked" class="bsc__muted">{{ ov.blocked }}</span>
          <span v-else class="bsc__muted">开着自动检测的任务会按时间表与预算创建付费检测</span>
          <span v-if="!ov.available" class="bsc__warn">这个面板没有接入 Boce 引擎</span>
        </div>
        <div v-if="ov.state.auto_paused" class="lb-notice bsc__notice">
          <span class="lb-notice__icon lb-notice__icon--warn"><LbIcon name="circle-alert" :size="18" /></span>
          <div class="lb-notice__body">
            <div class="lb-notice__title">已自动暂停({{ formatTime(ov.state.auto_paused_at) }})</div>
            <div class="lb-notice__text">{{ ov.state.auto_pause_reason }}。暂停期间不再创建付费检测,已经建了的照常取结果;原有探测不受影响。</div>
          </div>
          <div class="lb-notice__actions">
            <a-button size="small" :loading="resuming" data-testid="boce-resume" @click="resume">处理好了,恢复</a-button>
          </div>
        </div>
        <div class="bsc__grid lb-tabular">
          <div>
            <span>今日估算消耗</span>
            <b>{{ points(ov.usage.today_points) }} / {{ ov.usage.daily_limit ? points(ov.usage.daily_limit) : '未设上限' }}</b>
          </div>
          <div>
            <span>本月估算消耗</span>
            <b>{{ points(ov.usage.month_points) }} / {{ ov.usage.monthly_limit ? points(ov.usage.monthly_limit) : '未设上限' }}</b>
          </div>
          <div>
            <span>累计估算费用</span>
            <b>{{ points(ov.usage.total_points) }} · {{ yuan(ov.usage.total_yuan) }}</b>
          </div>
          <div>
            <span>
              Boce 账户余额(真实数字)
              <LbInfoTip :width="300" text="来自 Boce 余额接口,可能被面板以外的调用消耗;不能拿它的变化去算面板某个任务花了多少。面板自己的消耗看左边的估算。" />
            </span>
            <b v-if="ov.state.balance_points != null">¥{{ ov.state.balance_yuan }} · {{ points(ov.state.balance_points) }}</b>
            <b v-else>未查询</b>
          </div>
          <div>
            <span>全部自动任务(估算)</span>
            <b v-if="ov.plan">{{ points(ov.plan.others_month_points) }} / 月 · {{ yuan(ov.plan.total_yuan_month) }}</b>
            <b v-else>—</b>
          </div>
          <div>
            <span>按余额约可运行</span>
            <b>{{ ov.plan?.days_runnable != null ? `${Math.floor(ov.plan.days_runnable)} 天` : '—' }}</b>
          </div>
        </div>
        <div class="bsc__muted">
          预算按 {{ ov.usage.timezone }} 切日 / 切月。余额 <LbTimeText v-if="ov.state.balance_at" :value="ov.state.balance_at" /><template v-else>从没</template>
          查询<template v-if="ov.state.balance_error">(上次失败:{{ ov.state.balance_error }})</template>。
          <template v-if="ov.state.consecutive_api_errors">接口已连续异常 {{ ov.state.consecutive_api_errors }} 次:{{ ov.state.last_api_error }}</template>
        </div>
      </div>

      <!-- Key 与拨测点 -->
      <div class="lb-card bsc__card">
        <div class="lb-group">
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">
              API Key
              <LbInfoTip :width="300" text="只在服务端保存(主密钥加密),页面只显示末 4 位,也不会写进日志与审计。在 Boce 的 API 控制台申请。" />
            </span>
            <div class="bsc__inline">
              <a-input-password
                v-model:value="form.api_key"
                :placeholder="ov.settings.key_configured ? `已配置 ${ov.settings.key_hint},留空不改` : '粘贴 API Key'"
                autocomplete="off"
                aria-label="Boce API Key"
                class="bsc__key"
              />
              <a-button size="small" :loading="verifying" :disabled="!ov.settings.key_configured" data-testid="boce-verify" @click="verify">
                校验连通性
              </a-button>
              <a-button v-if="ov.settings.key_configured" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="clearKey">清空</a-button>
            </div>
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">连接状态</span>
            <div class="bsc__inline">
              <LbStatusTag :meta="verifyMeta" />
              <span class="bsc__muted">
                <template v-if="ov.state.verified_at"><LbTimeText :value="ov.state.verified_at" /></template>
                <template v-if="ov.state.verify_error"> · {{ ov.state.verify_error }}</template>
              </span>
              <a-button size="small" :loading="refreshing" :disabled="!ov.settings.key_configured" @click="refreshBalance">刷新余额</a-button>
            </div>
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">
              拨测点列表
              <LbInfoTip :width="300" text="缓存在面板里,打开页面不请求 Boce;启用后每天自动刷新一次。上游列表里消失的拨测点只标记、不删除,任务里选着的不会被换成别的付费拨测点;上游失败时保留原有缓存。" />
            </span>
            <div class="bsc__inline">
              <span class="lb-tabular">大陆 {{ ov.vantage_counts.cn }} · 海外 {{ ov.vantage_counts.oversea }}</span>
              <span v-if="ov.vantage_counts.missing" class="bsc__warn">已消失 {{ ov.vantage_counts.missing }}</span>
              <span class="bsc__muted">
                <template v-if="ov.state.vantages_synced_at">同步于 <LbTimeText :value="ov.state.vantages_synced_at" /></template>
                <template v-else>还没同步过</template>
                <template v-if="ov.state.vantages_error"> · {{ ov.state.vantages_error }}</template>
              </span>
              <a-button size="small" :loading="syncing" :disabled="!ov.settings.key_configured" data-testid="boce-sync" @click="sync">同步节点列表</a-button>
            </div>
          </div>
        </div>
      </div>

      <!-- 开关、预算与默认计划 -->
      <div class="lb-card bsc__card">
        <div v-if="formError" class="lb-error-strip">{{ formError }}</div>
        <div class="lb-group">
          <div class="lb-group__row bsc__row bsc__row--switch">
            <span class="lb-group__label">
              启用 Boce 外部拨测
              <LbInfoTip :width="320" text="全局开关,默认关。关着时不创建任何付费检测(手动检测也不行),已经建了的照常取结果;任务、历史与执行记录都保留。" />
            </span>
            <a-switch v-model:checked="form.enabled" aria-label="启用 Boce 外部拨测" />
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">
              每日 / 每月波点上限
              <LbInfoTip :width="320" text="硬限制。0 = 没有设:没有预算时不创建任何付费检测。创建之前按计划预留,多个任务并发也不会越过上限;到上限就停止新建(记「未执行」),到 80%(可改)推一次提醒。" />
            </span>
            <div class="bsc__inline">
              <a-input-number v-model:value="form.daily_points" :min="0" :max="10000000" :precision="0" addon-after="/ 日" aria-label="每日波点上限" />
              <a-input-number v-model:value="form.monthly_points" :min="0" :max="100000000" :precision="0" addon-after="/ 月" aria-label="每月波点上限" />
            </div>
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">单次任务上限</span>
            <a-input-number v-model:value="form.run_points" :min="1" :max="500" :precision="0" addon-after="波点 / 轮" aria-label="单次任务上限" />
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">
              估算单价
              <LbInfoTip :width="300" text="TCPing 与 Ping 都按:每个拨测点每次约 1 波点,1 波点约 ¥0.002。只用于估算与预算,实际以 Boce 账单为准。" />
            </span>
            <div class="bsc__inline">
              <a-input-number v-model:value="form.points_per_check" :min="1" :max="100" :precision="0" addon-after="波点 / 拨测点 / 次" aria-label="每拨测点每次波点" />
              <a-input v-model:value="form.point_price" class="bsc__price" addon-before="¥" addon-after="/ 波点" aria-label="每波点单价" />
            </div>
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">提醒</span>
            <div class="bsc__inline">
              <a-input-number v-model:value="form.alert_percent" :min="1" :max="100" :precision="0" addon-before="预算到" addon-after="%" aria-label="预算提醒百分比" />
              <a-input-number v-model:value="form.low_balance_points" :min="0" :max="1000000000" :precision="0" addon-before="余额低于" addon-after="波点" aria-label="低余额提醒" />
            </div>
          </div>
          <div class="lb-group__row bsc__row">
            <span class="lb-group__label">预算时区</span>
            <a-select v-model:value="form.timezone" show-search class="bsc__tz" aria-label="预算时区">
              <a-select-option v-for="z in tzOptions" :key="z" :value="z">{{ z }}</a-select-option>
            </a-select>
          </div>
        </div>
        <div class="bsc__sub">新建任务的默认计划</div>
        <BoceScheduleEditor v-if="plan" v-model="plan" />
        <div class="bsc__actions">
          <span v-if="dirty" class="bsc__dirty">已改动</span>
          <a-button v-if="dirty" size="small" class="lb-btn-ghost lb-btn-ghost--text" @click="fill(ov)">还原</a-button>
          <a-button type="primary" size="small" :disabled="!dirty" :loading="saving" data-testid="boce-save" @click="save">保存 Boce 设置</a-button>
        </div>
      </div>

      <!-- 全部任务 -->
      <div class="lb-card lb-card--flush bsc__tasks">
        <div class="bsc__tasks-head">
          <span class="bsc__sub">全部拨测任务({{ tasks.length }})</span>
          <span class="bsc__muted">任务在节点详情的「网络监测」里建与改;批量应用在节点列表里勾选节点后操作</span>
        </div>
        <div v-if="!tasks.length" class="bsc__muted bsc__empty">还没有任务</div>
        <ul v-else class="bsc__list">
          <li v-for="t in tasks" :key="t.id" class="bsc__task">
            <div class="bsc__task-main">
              <RouterLink :to="`/nodes/${t.node_id}/network`" class="lb-ellipsis">{{ t.node_name || `节点 #${t.node_id}` }}</RouterLink>
              <b class="lb-ellipsis">{{ t.name }}</b>
              <LbStatusTag :meta="taskMeta(t)" />
              <LbStatusTag v-if="t.last_run" :meta="runStateMeta(t.last_run.state)" />
            </div>
            <div class="bsc__task-meta lb-tabular">
              <span>{{ t.vantage_ids.length }} 个拨测点</span>
              <span>{{ scheduleText(t) }}</span>
              <span>本月 {{ points(t.month_points) }} · 预计 {{ points(t.plan_month_points) }}</span>
              <span v-if="t.next_run_at">下次 {{ formatTime(t.next_run_at) }}</span>
              <span v-else-if="t.auto_reason" class="bsc__muted">{{ t.auto_reason }}</span>
            </div>
          </li>
        </ul>
      </div>
    </template>
    <a-skeleton v-else-if="loading" active :paragraph="{ rows: 4 }" />
  </div>
</template>

<style scoped>
.bsc {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.bsc__card {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.bsc__status {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.bsc__notice {
  margin: 0;
}
.bsc__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
  gap: var(--sp-2);
}
.bsc__grid > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-sm);
  background: var(--surface2);
}
.bsc__grid span {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.bsc__grid b {
  font-size: var(--fs-sm);
  color: var(--text);
}
.bsc__row {
  grid-template-columns: 170px minmax(0, 1fr);
}
.bsc__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.bsc__inline {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.bsc__key {
  width: 320px;
  max-width: 100%;
}
.bsc__price {
  width: 190px;
}
.bsc__tz {
  width: 200px;
}
.bsc__sub {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text2);
}
.bsc__actions {
  display: flex;
  justify-content: flex-end;
  align-items: center;
  gap: var(--sp-2);
}
.bsc__dirty {
  font-size: var(--fs-xs);
  color: var(--warn);
}
.bsc__tasks-head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--sep2);
}
.bsc__list {
  list-style: none;
  margin: 0;
  padding: 0;
}
.bsc__task {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: var(--sp-2) var(--sp-4);
  border-bottom: 1px solid var(--sep2);
}
.bsc__task:last-child {
  border-bottom: none;
}
.bsc__task-main {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}
.bsc__task-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 2px var(--sp-3);
  font-size: var(--fs-xs);
  color: var(--text2);
}
.bsc__warn {
  font-size: var(--fs-xs);
  color: var(--warn);
}
.bsc__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
.bsc__empty {
  padding: var(--sp-3) var(--sp-4);
}
</style>
