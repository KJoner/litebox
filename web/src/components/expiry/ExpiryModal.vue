<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ExpiryDetail,
  type ExpiryKind,
  type ExpiryRenewBase,
  type ExpiryRenewMethod,
  type ExpiryRenewPlan,
  type ExpiryView,
} from '@/api/client'
import { LbStatusTag } from '@/components/lb'
import { formatTime, formatRelative } from '@/utils/format'
import ExpiryFields from './ExpiryFields.vue'
import {
  blankExpiryForm,
  expiryKindLabel,
  expiryStatusMeta,
  fromLocalInput,
  newRequestID,
  parseLeadDaysText,
  toLocalInput,
  type ExpiryFormValue,
} from './expiryMeta'

/**
 * 「续费 / 修改到期时间」。节点、外部代理、代理源三处的菜单都开这一个弹窗,
 * 与编辑表单共用同一套字段(ExpiryFields)与同一组接口。
 *
 * 三块:
 *   上  当前状态 + 上次提醒发没发出去(推送失败不影响面板里的提示,但要让人看得见);
 *   中  续费:延长 1 / 3 / 6 个月、1 年,或直接指定新到期时间;
 *       提交前显示「原到期时间 → 新到期时间」,由后端按自然月算,前端不自己算;
 *   下  档案字段 + 续费历史。
 *
 * request_id 在弹窗打开时生成一次:重复点击、网络重试都带同一个值,后端只延长一次。
 */
const props = defineProps<{
  open: boolean
  kind: ExpiryKind
  objectId: number
  name: string
  /** 外部代理跟随来源时,来源的名字(只用于文案) */
  sourceName?: string
}>()
const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  /** 档案或到期时间变了,调用方据此刷新列表 */
  (e: 'changed', view: ExpiryView): void
}>()

const detail = ref<ExpiryDetail | null>(null)
const loading = ref(false)
const loadError = ref('')

type Option = { key: string; label: string; method: ExpiryRenewMethod; count: number }
const options: Option[] = [
  { key: 'm1', label: '延长 1 个月', method: 'MONTHS', count: 1 },
  { key: 'm3', label: '延长 3 个月', method: 'MONTHS', count: 3 },
  { key: 'm6', label: '延长 6 个月', method: 'MONTHS', count: 6 },
  { key: 'y1', label: '延长 1 年', method: 'YEARS', count: 1 },
  { key: 'abs', label: '指定到期时间', method: 'ABSOLUTE', count: 0 },
  { key: 'clear', label: '取消到期时间', method: 'CLEAR', count: 0 },
]

const renew = reactive({
  option: 'm1',
  base: '' as ExpiryRenewBase,
  absoluteLocal: '',
  note: '',
  requestId: '',
})
const plan = ref<ExpiryRenewPlan | null>(null)
const planError = ref('')
const previewing = ref(false)
const renewing = ref(false)

const profile = ref<ExpiryFormValue>(blankExpiryForm())
const savingProfile = ref(false)
/** 外部代理:是否单独设置(否则跟随来源)。 */
const ownProfile = ref(true)

const view = computed(() => detail.value?.view ?? null)
const chosen = computed(() => options.find((o) => o.key === renew.option) ?? options[0]!)
const hasExpiry = computed(() => !!view.value?.expires_at)
const expired = computed(() => view.value?.state === 'OVERDUE')
const canInherit = computed(() => props.kind === 'EXTERNAL_PROXY' && !!props.sourceName)

function fillProfile(v: ExpiryView) {
  profile.value = {
    expires_at: v.expires_at,
    reminder_enabled: v.reminder_enabled,
    auto_renew: v.auto_renew,
    vendor_name: v.vendor_name,
    vendor_url: v.vendor_url,
    note: v.note,
    lead_days_text: v.lead_days.join(','),
  }
  ownProfile.value = !v.inherited
}

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const d = await api.expiry(props.kind, props.objectId)
    detail.value = d
    fillProfile(d.view)
    // 默认基准按后端规则:未过期从原到期时间起算,已过期从现在起算。留空让后端决定。
    renew.base = ''
    await preview()
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '加载到期信息失败'
  } finally {
    loading.value = false
  }
}

watch(
  () => props.open,
  (open) => {
    if (!open) return
    detail.value = null
    plan.value = null
    planError.value = ''
    renew.option = 'm1'
    renew.absoluteLocal = ''
    renew.note = ''
    renew.requestId = newRequestID()
    void load()
  },
  // 列表页用 v-if + :open="true" 挂载它,没有 false → true 的变化,所以要立即跑一次。
  { immediate: true },
)

let previewSeq = 0
async function preview() {
  const o = chosen.value
  const seq = ++previewSeq
  planError.value = ''
  if (o.method === 'ABSOLUTE' && !renew.absoluteLocal) {
    plan.value = null
    return
  }
  previewing.value = true
  try {
    const r = await api.renewExpiry(props.kind, props.objectId, {
      method: o.method,
      count: o.count,
      base: renew.base,
      expires_at: o.method === 'ABSOLUTE' ? fromLocalInput(renew.absoluteLocal) : '',
      preview: true,
    })
    if (seq === previewSeq) plan.value = r.plan
  } catch (err) {
    if (seq === previewSeq) {
      plan.value = null
      planError.value = err instanceof ApiError ? err.message : '无法计算新到期时间'
    }
  } finally {
    if (seq === previewSeq) previewing.value = false
  }
}

watch(() => [renew.option, renew.base, renew.absoluteLocal], () => void preview())

async function submitRenew() {
  const o = chosen.value
  renewing.value = true
  try {
    const r = await api.renewExpiry(props.kind, props.objectId, {
      method: o.method,
      count: o.count,
      base: renew.base,
      expires_at: o.method === 'ABSOLUTE' ? fromLocalInput(renew.absoluteLocal) : '',
      request_id: renew.requestId,
      note: renew.note,
    })
    if (r.duplicate) {
      message.info('这次续费已经提交过,没有再延长')
    } else if (o.method === 'CLEAR') {
      message.success('已取消到期时间')
    } else {
      message.success(`已续费:${formatTime(r.plan.old_expires_at) === '—' ? '未设置' : formatTime(r.plan.old_expires_at)} → ${formatTime(r.plan.new_expires_at)}`)
    }
    // 一次成功之后换一个新的幂等键:同一个弹窗里再续一次是另一笔。
    renew.requestId = newRequestID()
    renew.note = ''
    await load()
    if (r.view) emit('changed', r.view)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '续费失败')
  } finally {
    renewing.value = false
  }
}

async function saveProfile() {
  savingProfile.value = true
  try {
    let v: ExpiryView
    if (canInherit.value && !ownProfile.value) {
      v = await api.clearExpiry(props.kind, props.objectId)
      message.success(`已改为跟随来源「${props.sourceName}」的到期信息`)
    } else {
      const p = profile.value
      v = await api.saveExpiry(props.kind, props.objectId, {
        expires_at: p.expires_at,
        reminder_enabled: p.reminder_enabled,
        auto_renew: p.auto_renew,
        vendor_name: p.vendor_name,
        vendor_url: p.vendor_url,
        note: p.note,
        lead_days: parseLeadDaysText(p.lead_days_text),
      })
      message.success('到期档案已保存')
    }
    await load()
    emit('changed', v)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingProfile.value = false
  }
}

const title = computed(() => `续费 / 修改到期时间 · ${props.name}`)

const noticeText = computed(() => {
  const n = view.value?.last_notice
  if (!n) return ''
  const stage = n.stage === 'OVERDUE' ? '已到期提醒' : `提前 ${n.stage.slice(1)} 天提醒`
  switch (n.status) {
    case 'SENT':
      return `${stage}已经 ${n.channel} 发送(${formatRelative(n.sent_at)})`
    case 'PENDING':
      return `${stage}在 ${n.channel} 上还没发出去,正在重试${n.last_error ? `:${n.last_error}` : ''}`
    case 'FAILED':
      return `${stage}在 ${n.channel} 上发送失败,已放弃重试${n.last_error ? `:${n.last_error}` : ''}`
    default:
      return ''
  }
})

const noticeFailed = computed(() => {
  const s = view.value?.last_notice?.status
  return s === 'FAILED' || s === 'PENDING'
})

const absoluteInputFromCurrent = computed({
  get: () => renew.absoluteLocal || toLocalInput(view.value?.expires_at ?? ''),
  set: (v: string) => (renew.absoluteLocal = v),
})
</script>

<template>
  <a-modal
    :open="props.open"
    :title="title"
    :width="640"
    :footer="null"
    @cancel="emit('update:open', false)"
  >
    <div v-if="loadError" class="lb-error-strip">{{ loadError }}</div>
    <div v-else-if="!detail" class="xm__loading">正在读取到期信息…</div>
    <template v-else-if="view">
      <!-- 当前状态 -->
      <div class="lb-group xm__group">
        <div class="lb-group__row">
          <span class="lb-group__label">对象</span>
          <span class="lb-group__value">{{ expiryKindLabel[kind] }} · {{ name }}</span>
        </div>
        <div class="lb-group__row">
          <span class="lb-group__label">到期状态</span>
          <span class="lb-group__value xm__status">
            <LbStatusTag :meta="expiryStatusMeta(view)" size="md" />
            <span v-if="view.expires_at">{{ formatTime(view.expires_at) }}</span>
            <span v-if="view.auto_renew" class="lb-chip lb-chip--brand">已登记商家自动续费</span>
            <span v-if="view.inherited" class="lb-chip">跟随来源「{{ sourceName }}」</span>
          </span>
        </div>
        <div v-if="expired" class="lb-group__row">
          <span class="lb-group__label">说明</span>
          <span class="lb-group__value xm__warn">
            已到期,面板没有拿到新的到期时间。商家那边续没续费面板不知道 ——
            续费之后在下面登记新的到期时间,旧周期的提醒才会停。
          </span>
        </div>
        <div v-if="noticeText" class="lb-group__row">
          <span class="lb-group__label">上次提醒</span>
          <span class="lb-group__value" :class="{ xm__warn: noticeFailed }">{{ noticeText }}</span>
        </div>
      </div>

      <!-- 续费 -->
      <div class="xm__title">续费</div>
      <div class="xm__options">
        <button
          v-for="o in options"
          :key="o.key"
          type="button"
          class="xm__opt"
          :class="{ 'xm__opt--on': renew.option === o.key, 'xm__opt--danger': o.method === 'CLEAR' }"
          :disabled="o.method === 'CLEAR' && !hasExpiry"
          @click="renew.option = o.key"
        >
          {{ o.label }}
        </button>
      </div>
      <div v-if="chosen.method === 'ABSOLUTE'" class="xm__row">
        <span class="xm__row-label">新到期时间</span>
        <a-input v-model:value="absoluteInputFromCurrent" type="datetime-local" style="max-width: 260px" />
      </div>
      <div v-if="(chosen.method === 'MONTHS' || chosen.method === 'YEARS') && hasExpiry" class="xm__row">
        <span class="xm__row-label">从哪里起算</span>
        <a-radio-group v-model:value="renew.base" size="small">
          <a-radio-button value="">{{ expired ? '默认(从现在起)' : '默认(从原到期时间起)' }}</a-radio-button>
          <a-radio-button value="CURRENT">从原到期时间起</a-radio-button>
          <a-radio-button value="NOW">从现在起</a-radio-button>
        </a-radio-group>
      </div>
      <div class="xm__preview">
        <template v-if="planError">
          <span class="xm__warn">{{ planError }}</span>
        </template>
        <template v-else-if="plan">
          <span class="xm__from">{{ plan.old_expires_at ? formatTime(plan.old_expires_at) : '未设置' }}</span>
          <span class="xm__arrow">→</span>
          <b class="xm__to">{{ plan.new_expires_at ? formatTime(plan.new_expires_at) : '未设置' }}</b>
          <span class="xm__how">{{ plan.method_text }}</span>
        </template>
        <span v-else-if="previewing" class="xm__how">正在计算…</span>
        <span v-else class="xm__how">选一个延长方式,这里会显示新的到期时间</span>
      </div>
      <div class="xm__help">
        按自然月、自然年计算(1 月 31 日 + 1 个月 = 2 月末),不是固定 30 天。
        <strong>最终以商家实际给出的新到期时间为准</strong> —— 对不上就用「指定到期时间」。
        续费不改变流量周期、用户额度、节点凭据与服务运行状态。
      </div>
      <div class="xm__row">
        <span class="xm__row-label">备注</span>
        <a-input v-model:value="renew.note" :maxlength="256" placeholder="可选,只在管理后台出现(例如:支付宝付款 ¥60)" />
      </div>
      <div class="xm__actions">
        <a-button
          type="primary"
          :danger="chosen.method === 'CLEAR'"
          :loading="renewing"
          :disabled="!plan || !!planError"
          @click="submitRenew"
        >
          {{ chosen.method === 'CLEAR' ? '确认取消到期时间' : '确认续费' }}
        </a-button>
      </div>

      <!-- 档案 -->
      <div class="xm__title">到期档案</div>
      <div v-if="canInherit" class="xm__row xm__row--inherit">
        <a-switch v-model:checked="ownProfile" size="small" />
        <span>单独设置这条线路的到期信息(关掉则跟随来源「{{ sourceName }}」)</span>
      </div>
      <a-form layout="vertical">
        <ExpiryFields
          v-model="profile"
          :default-lead-days="detail.lead_days"
          :disabled="canInherit && !ownProfile"
          compact
        />
      </a-form>
      <div class="xm__help">
        提醒按设置里的规则发(提前 {{ detail.lead_days.join(' / ') }} 天,每天 {{ detail.send_time }},{{ detail.timezone }}),
        这里的「提前天数」只覆盖这一个对象。
      </div>
      <div class="xm__actions">
        <a-button :loading="savingProfile" @click="saveProfile">保存档案</a-button>
      </div>

      <!-- 续费历史 -->
      <div class="xm__title">续费历史</div>
      <div v-if="!detail.renewals.length" class="xm__help">还没有续费记录。</div>
      <div v-else class="xm__hist">
        <div v-for="r in detail.renewals" :key="r.id" class="xm__hist-row">
          <span class="xm__hist-time">{{ formatTime(r.created_at) }}</span>
          <span class="xm__hist-body">
            {{ r.method_text }}:
            {{ r.old_expires_at ? formatTime(r.old_expires_at) : '未设置' }} →
            <b>{{ r.new_expires_at ? formatTime(r.new_expires_at) : '未设置' }}</b>
            <span v-if="r.admin_name" class="xm__hist-who">· {{ r.admin_name }}</span>
            <span v-if="r.note" class="xm__hist-note">· {{ r.note }}</span>
          </span>
        </div>
      </div>
    </template>
  </a-modal>
</template>

<style scoped>
.xm__loading {
  padding: 24px 0;
  text-align: center;
  color: var(--text3);
  font-size: 13px;
}
.xm__group {
  margin-bottom: 18px;
}
.xm__status {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.xm__warn {
  color: var(--warn);
}
.xm__title {
  margin: 18px 0 8px;
  font-size: 13px;
  font-weight: 700;
  color: var(--text2);
}
.xm__options {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.xm__opt {
  border: none;
  border-radius: var(--r-pill);
  padding: 5px 13px;
  background: var(--fill);
  color: var(--text2);
  font-size: 12.5px;
  font-weight: 500;
  cursor: pointer;
}
.xm__opt--on {
  background: var(--brand-bg);
  color: var(--brand);
}
.xm__opt--danger.xm__opt--on {
  background: var(--bad-bg);
  color: var(--bad);
}
.xm__opt:disabled {
  opacity: 0.45;
  cursor: not-allowed;
}
.xm__row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 8px 0;
}
.xm__row--inherit {
  font-size: 12.5px;
  color: var(--text2);
}
.xm__row-label {
  flex: 0 0 90px;
  font-size: 12.5px;
  color: var(--text3);
}
.xm__preview {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 8px;
  margin: 10px 0 6px;
  padding: 10px 14px;
  border-radius: var(--r-group);
  background: var(--surface2);
  font-size: 14px;
  font-variant-numeric: tabular-nums;
}
.xm__from {
  color: var(--text3);
}
.xm__arrow {
  color: var(--text3);
}
.xm__to {
  color: var(--text);
}
.xm__how {
  font-size: 12px;
  color: var(--text3);
}
.xm__help {
  font-size: 12px;
  line-height: 1.6;
  color: var(--text3);
}
.xm__actions {
  display: flex;
  justify-content: flex-end;
  margin: 10px 0 4px;
}
.xm__hist {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12.5px;
}
.xm__hist-row {
  display: grid;
  grid-template-columns: 128px minmax(0, 1fr);
  gap: 10px;
}
.xm__hist-time {
  color: var(--text3);
  font-variant-numeric: tabular-nums;
}
.xm__hist-who,
.xm__hist-note {
  color: var(--text3);
}
</style>
