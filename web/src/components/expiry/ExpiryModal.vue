<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type ExpiryDetail,
  type ExpiryKind,
  type ExpiryRenewBase,
  type ExpiryRenewMethod,
  type ExpiryRenewPeriod,
  type ExpiryRenewPlan,
  type ExpiryView,
} from '@/api/client'
import { LbInfoTip, LbStatusTag } from '@/components/lb'
import { formatTime, formatRelative } from '@/utils/format'
import ExpiryFields from './ExpiryFields.vue'
import BillingFields from './BillingFields.vue'
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
import { billingFormOf, billingPayload, blankBillingForm, oneCycleRenewal, type BillingFormValue } from './billingMeta'

/**
 * 「套餐与续费」。节点、外部代理、代理源三处的菜单与仪表盘卡片都开这一个弹窗,
 * 与编辑表单共用同一套到期字段(ExpiryFields)与同一组接口 —— 不另做第二个入口。
 *
 * 四块:
 *   上  当前状态、套餐、剩余未摊销价值(估算,按时间 / 按流量两种口径),上次提醒发没发出去;
 *   中  续费:续一期 / 延长 1·3·6 个月 / 1 年 / 指定新到期时间,可记实际支付;
 *       提交前显示「原到期时间 → 新到期时间」与新的本期,全由后端按日历月算,前端不自己算;
 *   下  到期档案 + 套餐计费 + 地区标签;续费历史(只追加,不覆盖)。
 *
 * request_id 在弹窗打开时生成一次:重复点击、网络重试都带同一个值,后端只延长、只记账一次。
 */
const props = defineProps<{
  open: boolean
  kind: ExpiryKind
  objectId: number
  name: string
  /** 外部代理跟随来源时,来源的名字(只用于文案) */
  sourceName?: string
  /** 打开后先滚到哪一块(仪表盘卡片上的「编辑套餐 / 续费 / 历史」) */
  focus?: 'billing' | 'renew' | 'history'
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
const baseOptions: Option[] = [
  { key: 'm1', label: '延长 1 个月', method: 'MONTHS', count: 1 },
  { key: 'm3', label: '延长 3 个月', method: 'MONTHS', count: 3 },
  { key: 'm6', label: '延长 6 个月', method: 'MONTHS', count: 6 },
  { key: 'y1', label: '延长 1 年', method: 'YEARS', count: 1 },
  { key: 'abs', label: '指定到期时间', method: 'ABSOLUTE', count: 0 },
  { key: 'clear', label: '取消到期时间', method: 'CLEAR', count: 0 },
]
/** 有固定周期的套餐多一个「续一期」,排在最前面并默认选中。 */
const cycleOption = computed<Option | null>(() => {
  const c = oneCycleRenewal(detail.value?.view.billing)
  return c ? { key: 'cycle', label: c.label, method: c.method, count: c.count } : null
})
const options = computed<Option[]>(() => (cycleOption.value ? [cycleOption.value, ...baseOptions] : baseOptions))

const renew = reactive({
  option: 'm1',
  base: '' as ExpiryRenewBase,
  absoluteLocal: '',
  note: '',
  amount: '',
  requestId: '',
})
const plan = ref<ExpiryRenewPlan | null>(null)
const period = ref<ExpiryRenewPeriod | null>(null)
const planError = ref('')
const previewing = ref(false)
const renewing = ref(false)

const profile = ref<ExpiryFormValue>(blankExpiryForm())
const billing = ref<BillingFormValue>(blankBillingForm())
/** 标签用逗号分隔的文本框,与「提前天数」同一种写法 —— 不用下拉的 tags 模式:那种控件把输入框里
 * 还没回车的字在失焦时才提交,点「保存」的那一下读到的是旧值,敲了的标签就悄悄丢了。 */
const labels = reactive({ region: '', tagsText: '' })
const splitTags = (t: string) => t.split(/[,，]/).map((x) => x.trim()).filter(Boolean)
const savingProfile = ref(false)
/** 外部代理:是否单独设置(否则跟随来源)。 */
const ownProfile = ref(true)

const view = computed(() => detail.value?.view ?? null)
const val = computed(() => detail.value?.valuation ?? null)
const chosen = computed(() => options.value.find((o) => o.key === renew.option) ?? options.value[0]!)
const hasExpiry = computed(() => !!view.value?.expires_at)
const expired = computed(() => view.value?.state === 'OVERDUE')
const canInherit = computed(() => props.kind === 'EXTERNAL_PROXY' && !!props.sourceName)
const currency = computed(() => view.value?.billing?.currency || billing.value.currency.toUpperCase())

const renewRef = ref<HTMLElement | null>(null)
const profileRef = ref<HTMLElement | null>(null)
const historyRef = ref<HTMLElement | null>(null)

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
  billing.value = billingFormOf(v.billing)
  ownProfile.value = !v.inherited
}

let focused = false
async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const d = await api.expiry(props.kind, props.objectId)
    detail.value = d
    fillProfile(d.view)
    labels.region = d.labels?.region ?? ''
    labels.tagsText = (d.labels?.tags ?? []).join(', ')
    if (cycleOption.value && renew.option === 'm1' && !focused) renew.option = 'cycle'
    // 默认基准按后端规则:未过期从原到期时间起算,已过期从现在起算。留空让后端决定。
    renew.base = ''
    await preview()
    if (!focused && props.focus) {
      focused = true
      await nextTick()
      const el = props.focus === 'renew' ? renewRef.value : props.focus === 'history' ? historyRef.value : profileRef.value
      el?.scrollIntoView({ block: 'start', behavior: 'smooth' })
    }
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
    period.value = null
    planError.value = ''
    focused = false
    renew.option = 'm1'
    renew.absoluteLocal = ''
    renew.note = ''
    renew.amount = ''
    renew.requestId = newRequestID()
    void load()
  },
  // 列表页用 v-if + :open="true" 挂载它,没有 false → true 的变化,所以要立即跑一次。
  { immediate: true },
)

function renewBody() {
  const o = chosen.value
  return {
    method: o.method,
    count: o.count,
    base: renew.base,
    expires_at: o.method === 'ABSOLUTE' ? fromLocalInput(renew.absoluteLocal) : '',
    amount: o.method === 'CLEAR' ? '' : renew.amount.trim(),
  }
}

let previewSeq = 0
async function preview() {
  const o = chosen.value
  const seq = ++previewSeq
  planError.value = ''
  if (o.method === 'ABSOLUTE' && !renew.absoluteLocal) {
    plan.value = null
    period.value = null
    return
  }
  previewing.value = true
  try {
    const r = await api.renewExpiry(props.kind, props.objectId, { ...renewBody(), preview: true })
    if (seq === previewSeq) {
      plan.value = r.plan
      period.value = r.period
    }
  } catch (err) {
    if (seq === previewSeq) {
      plan.value = null
      period.value = null
      planError.value = err instanceof ApiError ? err.message : '无法计算新到期时间'
    }
  } finally {
    if (seq === previewSeq) previewing.value = false
  }
}

watch(() => [renew.option, renew.base, renew.absoluteLocal, renew.amount], () => void preview())

async function submitRenew() {
  const o = chosen.value
  renewing.value = true
  try {
    const r = await api.renewExpiry(props.kind, props.objectId, {
      ...renewBody(),
      request_id: renew.requestId,
      note: renew.note,
    })
    if (r.duplicate) {
      message.info('这次续费已经提交过,没有再延长,也没有重复记账')
    } else if (o.method === 'CLEAR') {
      message.success('已取消到期时间')
    } else {
      message.success(`已续费:${formatTime(r.plan.old_expires_at) === '—' ? '未设置' : formatTime(r.plan.old_expires_at)} → ${formatTime(r.plan.new_expires_at)}`)
    }
    // 一次成功之后换一个新的幂等键:同一个弹窗里再续一次是另一笔。
    renew.requestId = newRequestID()
    renew.note = ''
    renew.amount = ''
    await load()
    if (r.view) emit('changed', r.view)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '续费失败')
  } finally {
    renewing.value = false
  }
}

const labelsDirty = computed(() => {
  const l = detail.value?.labels
  const tags = splitTags(labels.tagsText)
  if (!l) return labels.region.trim() !== '' || tags.length > 0
  return l.region !== labels.region.trim().toUpperCase() || l.tags.join(',') !== tags.join(',')
})

async function saveProfile() {
  savingProfile.value = true
  try {
    let v: ExpiryView
    let done: string
    if (canInherit.value && !ownProfile.value) {
      v = await api.clearExpiry(props.kind, props.objectId)
      done = `已改为跟随来源「${props.sourceName}」的到期信息`
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
        billing: billingPayload(billing.value),
      })
      done = '套餐与到期档案已保存'
    }
    // 标签不跟到期继承走,不管是否单独设置都可以存。两样都存完才说「已保存」。
    if (labelsDirty.value) {
      await api.saveAssetLabels(props.kind, props.objectId, { region: labels.region, tags: splitTags(labels.tagsText) })
    }
    message.success(done)
    await load()
    emit('changed', v)
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '保存失败')
  } finally {
    savingProfile.value = false
  }
}

const title = computed(() => `套餐与续费 · ${props.name}`)

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

/** 续费预览里「新的本期」那一句。 */
const periodText = computed(() => {
  const p = period.value
  if (!p || !plan.value) return ''
  const cur = p.currency || currency.value
  const parts = [`新的本期从 ${formatTime(p.start)} 起`]
  if (p.paid_minor >= 0) parts.push(`本次支付 ${p.paid} ${cur}`)
  else parts.push(view.value?.billing?.price ? `本次支付没填,按套餐价格 ${view.value.billing.price} ${cur} 估` : '没有记金额')
  if (p.carry_minor > 0) parts.push(`上一期没用完的 ${p.carry} ${cur} 结转进来`)
  return parts.join(',')
})

function fillPrice() {
  renew.amount = view.value?.billing?.price ?? ''
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="title"
    :width="680"
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
        <div v-if="view.billing?.has_billing && val" class="lb-group__row">
          <span class="lb-group__label">套餐</span>
          <span class="lb-group__value xm__status">
            <span>{{ val.cycle_label }}</span>
            <b v-if="val.amount" class="lb-tabular">{{ val.amount }} {{ val.currency }}</b>
            <span v-if="val.carry_minor > 0" class="xm__how">含上期结转 {{ val.carry }} {{ val.currency }}</span>
            <span v-if="val.paid_from_price && val.amount" class="xm__how">本次支付没填,按标价估</span>
          </span>
        </div>
        <div v-if="view.billing?.has_billing && val" class="lb-group__row">
          <span class="lb-group__label">
            剩余未摊销价值
            <LbInfoTip
              :width="320"
              text="已付金额里还对应着没用完的时间 / 流量的部分,按比例估算(估算值)。它不是余额,也不代表商家会退款;按时间与按流量是两种口径,分开列,不相加。"
            />
          </span>
          <span class="lb-group__value xm__value">
            <span>
              按时间:<b class="lb-tabular">{{ val.time.available ? `${val.time.value} ${val.currency}` : '—' }}</b>
              <span class="xm__how">{{ val.time.available ? val.time.basis : val.time.reason }}</span>
            </span>
            <span>
              按流量:<b class="lb-tabular">{{ val.traffic.available ? `${val.traffic.value} ${val.currency}` : '—' }}</b>
              <span class="xm__how">{{ val.traffic.available ? val.traffic.basis : val.traffic.reason }}</span>
            </span>
            <span v-if="val.period_start" class="xm__how">
              本期 {{ formatTime(val.period_start) }} → {{ val.period_end ? formatTime(val.period_end) : '—' }}
              {{ val.period_start_derived ? '(起点按周期推算)' : '' }}
            </span>
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
      <div ref="renewRef" class="xm__title">续费</div>
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
      <div v-if="(chosen.method === 'MONTHS' || chosen.method === 'YEARS' || chosen.method === 'DAYS') && hasExpiry" class="xm__row">
        <span class="xm__row-label">从哪里起算</span>
        <a-radio-group v-model:value="renew.base" size="small">
          <a-radio-button value="">{{ expired ? '默认(从现在起)' : '默认(从原到期时间起)' }}</a-radio-button>
          <a-radio-button value="CURRENT">从原到期时间起</a-radio-button>
          <a-radio-button value="NOW">从现在起</a-radio-button>
        </a-radio-group>
      </div>
      <div v-if="chosen.method !== 'CLEAR'" class="xm__row">
        <span class="xm__row-label">实际支付</span>
        <a-input
          v-model:value="renew.amount"
          inputmode="decimal"
          :placeholder="view.billing?.price ? `留空 = 不记金额(估值按 ${view.billing.price} 算)` : '留空 = 不记金额'"
          style="max-width: 260px"
          :addon-after="currency || '币种未设置'"
        />
        <a-button v-if="view.billing?.price" size="small" type="link" @click="fillPrice">填入套餐价格</a-button>
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
          <span v-if="periodText" class="xm__how xm__period">{{ periodText }}</span>
        </template>
        <span v-else-if="previewing" class="xm__how">正在计算…</span>
        <span v-else class="xm__how">选一个延长方式,这里会显示新的到期时间</span>
      </div>
      <div class="xm__help">
        按自然月、自然年计算(1 月 31 日 + 1 个月 = 2 月末),不是固定 30 天。
        <strong>最终以商家实际给出的新到期时间为准</strong> —— 对不上就用「指定到期时间」。
        每次续费都追加一条历史,不覆盖之前的支付记录;续费不改变流量周期、用户额度、节点凭据与服务运行状态。
      </div>
      <div class="xm__row">
        <span class="xm__row-label">备注</span>
        <a-input v-model:value="renew.note" :maxlength="256" placeholder="可选,只在管理后台出现(例如:支付宝付款)" />
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
      <div ref="profileRef" class="xm__title">套餐与到期档案</div>
      <div v-if="canInherit" class="xm__row xm__row--inherit">
        <a-switch v-model:checked="ownProfile" size="small" />
        <span>单独设置这条线路的到期与套餐(关掉则跟随来源「{{ sourceName }}」,不重复计费)</span>
      </div>
      <a-form layout="vertical">
        <ExpiryFields
          v-model="profile"
          :default-lead-days="detail.lead_days"
          :disabled="canInherit && !ownProfile"
          compact
        />
        <div class="xm__sub">套餐计费</div>
        <BillingFields v-model="billing" :disabled="canInherit && !ownProfile" />
        <div class="xm__sub">
          地区与标签
          <LbInfoTip :width="260" text="只用于仪表盘卡片的筛选与展示,不进节点配置、不进订阅。面板不按名字猜地区。" />
        </div>
        <a-row :gutter="12">
          <a-col :xs="10" :sm="6">
            <a-form-item label="地区代码">
              <a-input v-model:value="labels.region" :maxlength="2" placeholder="HK / JP / US" />
            </a-form-item>
          </a-col>
          <a-col :xs="14" :sm="18">
            <a-form-item label="标签">
              <a-input v-model:value="labels.tagsText" placeholder="逗号分隔,最多 8 个,例如 CN2, IEPL" />
            </a-form-item>
          </a-col>
        </a-row>
      </a-form>
      <div class="xm__help">
        提醒按设置里的规则发(提前 {{ detail.lead_days.join(' / ') }} 天,每天 {{ detail.send_time }},{{ detail.timezone }}),
        这里的「提前天数」只覆盖这一个对象。金额按币种的小数位记,不同币种不会被加在一起。
      </div>
      <div class="xm__actions">
        <a-button :loading="savingProfile" @click="saveProfile">保存档案</a-button>
      </div>

      <!-- 续费历史 -->
      <div ref="historyRef" class="xm__title">续费历史</div>
      <div v-if="!detail.renewals.length" class="xm__help">还没有续费记录。</div>
      <div v-else class="xm__hist">
        <div v-for="r in detail.renewals" :key="r.id" class="xm__hist-row">
          <span class="xm__hist-time">{{ formatTime(r.created_at) }}</span>
          <span class="xm__hist-body">
            {{ r.method_text }}:
            {{ r.old_expires_at ? formatTime(r.old_expires_at) : '未设置' }} →
            <b>{{ r.new_expires_at ? formatTime(r.new_expires_at) : '未设置' }}</b>
            <span v-if="r.amount" class="xm__hist-paid lb-tabular">· 实付 {{ r.amount }} {{ r.currency }}</span>
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
  font-size: var(--fs-sm);
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
.xm__value {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.xm__warn {
  color: var(--warn);
}
.xm__title {
  margin: 18px 0 8px;
  font-size: var(--fs-sm);
  font-weight: 700;
  color: var(--text2);
  scroll-margin-top: 12px;
}
.xm__sub {
  display: flex;
  align-items: center;
  gap: 4px;
  margin: 6px 0 8px;
  padding-top: 10px;
  border-top: 1px solid var(--sep2);
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text3);
}
.xm__options {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 10px;
}
.xm__opt {
  border: none;
  border-radius: var(--r-btn);
  padding: 5px 13px;
  background: var(--fill);
  color: var(--text2);
  font-size: var(--fs-sm);
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
  font-size: var(--fs-sm);
  color: var(--text2);
}
.xm__row-label {
  flex: 0 0 90px;
  font-size: var(--fs-sm);
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
  font-size: var(--fs-base);
  font-variant-numeric: tabular-nums;
}
.xm__period {
  flex-basis: 100%;
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
  font-size: var(--fs-xs);
  color: var(--text3);
}
.xm__help {
  font-size: var(--fs-xs);
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
  font-size: var(--fs-sm);
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
.xm__hist-paid {
  color: var(--text2);
}
.xm__hist-who,
.xm__hist-note {
  color: var(--text3);
}
@media (max-width: 575px) {
  .xm__row {
    flex-wrap: wrap;
  }
  .xm__hist-row {
    grid-template-columns: minmax(0, 1fr);
    gap: 0;
  }
}
</style>
