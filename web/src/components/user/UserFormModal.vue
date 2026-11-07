<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message, Modal } from 'ant-design-vue'
import { api, ApiError, type AccessTier, type Node, type ProxyUser, type ResetRulePreview, type UserResetCycle } from '@/api/client'
import { checkLoginUsername, checkPassword } from '@/utils/validate'
import { LbInfoTip } from '@/components/lb'
import { fromBytes, toBytes, type LbQuotaUnit } from './quota'

/**
 * 新建 / 编辑用户。同一个表单两种 mode。
 *
 * 差异只有三处,其余字段共用:
 *   门户登录  新建时整块出现;编辑态隐藏 —— 已有账号在详情抽屉里管,
 *             那里才能同时说清「重设密码会踢掉全部会话」。
 *   标识字段  新建显示「自动分配」;编辑显示真值且只读。
 *   主按钮    「创建用户」/「保存」,后者无改动时禁用。
 *
 * 版式:标准弹窗(UI 2.0 起标题左对齐、取消 / 保存在右下角的底栏,V18 的三栏头已删);
 * 正文是几段分组列表(130px 标签 | 控件),帮助文案收进标签旁的 ⓘ;底部居中一句摘要。
 */
const props = defineProps<{
  open: boolean
  /** null = 新建 */
  user: ProxyUser | null
  tiers: AccessTier[]
  nodes: Node[]
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'saved'): void
}>()

const isEdit = computed(() => props.user !== null)
const submitting = ref(false)
/** 只有服务端知道的错误(重名、外键)放表单顶部,不弹吐司。 */
const serverError = ref('')

const blank = {
  display_name: '',
  remark: '',
  quota_value: null as number | null,
  quota_unit: 'GB' as LbQuotaUnit,
  expires_at: '',
  reset_cycle: 'NONE' as UserResetCycle,
  reset_day: 1,
  reset_period_days: 30,
  reset_anchor_at: '',
  reset_timezone: 'UTC',
  access_tier_id: 1,
  node_ids: [] as number[],
  login_username: '',
  login_password: '',
  must_change_password: true,
}
const form = reactive({ ...blank })
/** 打开时的快照,用于脏检查 —— 不用「碰过就算脏」。 */
let snapshot = JSON.stringify(blank)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    serverError.value = ''
    const u = props.user
    if (!u) {
      Object.assign(form, blank, { access_tier_id: props.tiers[0]?.id ?? 1 })
    } else {
      const q = fromBytes(u.quota_bytes)
      Object.assign(form, {
        ...blank,
        display_name: u.display_name,
        remark: u.remark,
        quota_value: q.value,
        quota_unit: q.unit,
        expires_at: u.expires_at ? u.expires_at.slice(0, 10) : '',
        reset_cycle: u.reset_cycle,
        reset_day: u.reset_day,
        reset_period_days: u.reset_period_days || 30,
        reset_anchor_at: anchorLocalDate(u.reset_anchor_at, u.reset_timezone || 'UTC'),
        reset_timezone: u.reset_timezone || 'UTC',
        access_tier_id: u.access_tier_id,
        // 只回填额外授权:等级继承来的节点不在这里改,
        // 塞进多选框会让管理员一保存就把继承关系固化成手工授权。
        node_ids: [...u.node_ids],
      })
    }
    snapshot = JSON.stringify(form)
  },
)

const dirtyFields = computed(() => {
  const before = JSON.parse(snapshot) as typeof form
  const labels: Record<string, string> = {
    display_name: '用户名称',
    remark: '备注',
    quota_value: '流量额度',
    quota_unit: '流量额度',
    expires_at: '到期时间',
    reset_cycle: '流量重置',
    reset_day: '重置日',
    reset_period_days: '重置周期',
    reset_anchor_at: '对齐日',
    reset_timezone: '重置时区',
    access_tier_id: '访问等级',
    node_ids: '额外授权节点',
  }
  const out = new Set<string>()
  for (const k of Object.keys(labels)) {
    const a = JSON.stringify((before as never)[k])
    const b = JSON.stringify((form as never)[k])
    if (a !== b) out.add(labels[k])
  }
  return [...out]
})

const usernameError = computed(() =>
  form.login_username ? checkLoginUsername(form.login_username) : undefined,
)
const passwordError = computed(() =>
  form.login_password ? checkPassword(form.login_password) : undefined,
)

const nodeOptions = computed(() =>
  props.nodes.map((n) => ({
    // 标出这台机器上入口的等级:否则会给普通用户重复授权他早就继承到的机器。
    // 等级已经降到入口上,所以一台机器可能同时列出几档 —— 那正是要看到的。
    label: `${n.name}(${tiersOf(n)}·${n.host})`,
    value: n.id,
  })),
)

/** 这台机器上入口的等级,去重。没有入口时说清楚 —— 授权它没有任何效果。 */
function tiersOf(n: Node): string {
  if (n.role === 'RELAY') return '转发规则各自设定'
  const names = [...new Set((n.inbounds ?? []).map((i) => i.access_tier_name))]
  return names.length ? names.join('/') : '无入口'
}

function close() {
  emit('update:open', false)
}

/** 确实脏了才拦截,并且列出改了哪几项 —— 只说「有未保存的修改」还得回去找。 */
function tryClose() {
  if (dirtyFields.value.length === 0) return close()
  Modal.confirm({
    title: '放弃未保存的修改?',
    content: `已改动 ${dirtyFields.value.length} 项:${dirtyFields.value.join('、')}。`,
    okText: '放弃',
    okType: 'danger',
    cancelText: '继续编辑',
    autoFocusButton: 'cancel',
    onOk: close,
  })
}

async function submit() {
  if (!form.display_name.trim()) {
    serverError.value = '请填写用户名称'
    return
  }
  // 能在前端判断的(格式、长度、必填)提交前拦住,不让它变成服务端错误。
  if (!isEdit.value && form.login_username && (usernameError.value || passwordError.value)) {
    serverError.value = '登录账号或初始密码格式不正确'
    return
  }

  submitting.value = true
  serverError.value = ''
  const quota_bytes = toBytes(form.quota_value, form.quota_unit)
  // 日期输入只有年月日,补成当天结束的 UTC 时刻。
  const expiresAt = form.expires_at ? `${form.expires_at}T23:59:59Z` : ''

  try {
    if (!isEdit.value) {
      const created = await api.createUser({
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes,
        expires_at: expiresAt || undefined,
        reset_cycle: form.reset_cycle,
        reset_day: form.reset_day,
        ...cycleExtras(),
        access_tier_id: form.access_tier_id,
        node_ids: form.node_ids,
        login_username: form.login_username,
        login_password: form.login_password,
        must_change_password: form.must_change_password,
      })
      // 口令只在这一次请求里用到,立刻从表单状态里抹掉。
      form.login_password = ''

      if (created.portal_account_error) {
        // 用户建好了、登录账号没建成。必须分开说,否则管理员会再建一个用户。
        Modal.warning({
          title: '用户已创建,但登录账号没有建成',
          width: 520,
          content: `${created.portal_account_error}\n\n该用户现在无法登录用户中心 —— 他拿账号去登录只会得到「账号或密码错误」。请在用户详情里单独开通门户登录。不要重新创建用户。`,
          okText: '知道了',
        })
      } else if (created.portal_account) {
        // 这两项管理员必须转发给用户,用一条 3 秒吐司交付等于让他回详情页翻。
        Modal.success({
          title: '用户已创建,门户登录已开通',
          width: 520,
          content: `请把下面两项发给用户:\n\n登录地址:${window.location.origin}\n登录账号:${created.portal_account.username}\n\n受影响节点将在数秒内自动部署。`,
          okText: '知道了',
        })
      } else {
        message.success('用户已创建,受影响节点将在数秒内自动部署')
      }
    } else {
      await api.updateUser(props.user!.id, {
        display_name: form.display_name,
        remark: form.remark,
        quota_bytes,
        ...(expiresAt ? { expires_at: expiresAt } : { clear_expiry: true }),
        // 重置规则只在那一栏真的改过时才发(F01):表单回显的是当前规则,原样发回去会被当成
        // 「改回当前规则」,顺手把已经排好的下一周期规则撤销掉 —— 而管理员改的只是备注。
        ...(cycleDirty.value || cancelPending.value
          ? { reset_cycle: form.reset_cycle, reset_day: form.reset_day, ...cycleExtras(), reset_apply: resetApply.value }
          : {}),
        access_tier_id: form.access_tier_id,
        node_ids: form.node_ids,
      })
      message.success('已保存')
    }
    close()
    emit('saved')
  } catch (err) {
    serverError.value = err instanceof ApiError ? err.message : '保存失败'
  } finally {
    submitting.value = false
  }
}

/** 周期边界的时区(TRAFFIC08):存量用户都是 UTC。只列常用的几个,别的 IANA 名可以直接输入搜索。 */
const timezoneOptions = ['UTC', 'Asia/Shanghai', 'Asia/Hong_Kong', 'Asia/Tokyo', 'Asia/Singapore',
  'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles'].map((v) => ({ value: v, label: v }))

/**
 * 周期规则的另外三项。对齐日发**当地日期**(YYYY-MM-DD),由后端按 reset_timezone 的零点解释(F01)。
 * 原来拼成 …T00:00:00Z 发上去,后端再换到用户时区 —— 纽约、洛杉矶的起点会早一天。
 */
function cycleExtras() {
  const anchor = form.reset_anchor_at.trim()
  return {
    reset_period_days: form.reset_cycle === 'DAYS' ? form.reset_period_days : 0,
    reset_anchor_at: form.reset_cycle === 'DAYS' && anchor ? anchor : '',
    reset_timezone: form.reset_timezone || 'UTC',
  }
}

/** 库里的对齐日换成当地日期显示:新数据本来就是 YYYY-MM-DD;老的 RFC3339 时刻按所选时区取那一天。 */
function anchorLocalDate(anchor: string, tz: string): string {
  if (!anchor) return ''
  if (/^\d{4}-\d{2}-\d{2}$/.test(anchor)) return anchor
  const d = new Date(anchor)
  if (Number.isNaN(d.getTime())) return anchor.slice(0, 10)
  try {
    return new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit' }).format(d)
  } catch {
    return anchor.slice(0, 10)
  }
}

// ---- 改重置规则的生效方式与预览(F01)----
const CYCLE_KEYS = ['reset_cycle', 'reset_day', 'reset_period_days', 'reset_anchor_at', 'reset_timezone'] as const
const cycleDirty = computed(() => {
  if (!isEdit.value) return false
  const before = JSON.parse(snapshot) as typeof form
  return CYCLE_KEYS.some((k) => JSON.stringify(before[k]) !== JSON.stringify(form[k]))
})
/** 默认下一周期生效;立即生效是单独的显式选择,保存之前看得到它的后果。 */
const resetApply = ref<'NEXT' | 'NOW'>('NEXT')
const cancelPending = ref(false)
const resetPreview = ref<ResetRulePreview | null>(null)
const resetPreviewError = ref('')
let previewSeq = 0
watch(
  () => [cycleDirty.value, resetApply.value, ...CYCLE_KEYS.map((k) => form[k])],
  () => {
    resetPreview.value = null
    resetPreviewError.value = ''
    if (!isEdit.value || !cycleDirty.value || !props.user) return
    const seq = ++previewSeq
    const id = props.user.id
    setTimeout(async () => {
      if (seq !== previewSeq) return
      try {
        const p = await api.previewResetRule(id, {
          reset_cycle: form.reset_cycle, reset_day: form.reset_day, ...cycleExtras(), reset_apply: resetApply.value,
        })
        if (seq === previewSeq) resetPreview.value = p
      } catch (e) {
        if (seq === previewSeq) resetPreviewError.value = e instanceof ApiError ? e.message : '预览失败'
      }
    }, 300)
  },
)
const pendingRule = computed(() => {
  const u = props.user
  if (!u || !u.reset_next_cycle) return ''
  return describeRule(u.reset_next_cycle, u.reset_next_day ?? 0, u.reset_next_period_days ?? 0, u.reset_next_anchor_at ?? '', u.reset_next_timezone ?? '')
})
function describeRule(cycle: string, day: number, period: number, anchor: string, tz: string): string {
  if (cycle === 'NONE') return '不重置'
  if (cycle === 'MONTHLY') return `每月 ${day} 日(${tz || 'UTC'})`
  return `每 ${period} 天(对齐 ${anchor || '创建那天'},${tz || 'UTC'})`
}
function fmtUTC(s: string): string {
  return s ? s.replace('T', ' ').replace(/:00Z$/, ' UTC').replace(/Z$/, ' UTC') : ''
}
/** 撤销已经排好的下一周期规则:把当前规则原样发回去(后端把「改回与当前相同」当成撤销)。 */
function undoPending() {
  const u = props.user
  if (!u) return
  Object.assign(form, {
    reset_cycle: u.reset_cycle, reset_day: u.reset_day, reset_period_days: u.reset_period_days || 30,
    reset_anchor_at: anchorLocalDate(u.reset_anchor_at, u.reset_timezone || 'UTC'), reset_timezone: u.reset_timezone || 'UTC',
  })
  cancelPending.value = true
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="isEdit ? `编辑用户 · ${props.user?.display_name}` : '新增用户'"
    :width="560"
    :confirm-loading="submitting"
    :ok-text="isEdit ? '保存' : '创建用户'"
    :ok-button-props="{ disabled: isEdit && dirtyFields.length === 0 }"
    cancel-text="取消"
    :mask-closable="false"
    @cancel="tryClose"
    @ok="submit"
  >
    <div class="uf">
      <div v-if="serverError" class="lb-error-strip">
        <div>
          <b>{{ serverError }}</b>
          <span class="uf__err-note">表单内容已保留。</span>
        </div>
      </div>

      <div class="lb-group">
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            <span class="uf__req">*</span> 用户名称
            <LbInfoTip text="只在管理后台出现,用户看不到。" :width="220" />
          </span>
          <a-input v-model:value="form.display_name" placeholder="用于识别的显示名" />
        </div>
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">备注</span>
          <a-input v-model:value="form.remark" placeholder="例如:2026 年 8 月起" />
        </div>
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            访问等级
            <LbInfoTip text="等级不高于该等级的节点会自动可用,不必逐个勾选。" :width="260" />
          </span>
          <a-select v-model:value="form.access_tier_id">
            <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
              {{ t.name }} —— {{ t.description }}
            </a-select-option>
          </a-select>
        </div>
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            流量额度
            <LbInfoTip text="留空或填 0 表示不限量。单位只影响输入,提交时换算成字节发给接口。" :width="260" />
          </span>
          <div class="uf__quota">
            <a-input-number
              v-model:value="form.quota_value"
              :min="0"
              :precision="2"
              placeholder="不限量"
              class="uf__quota-num"
            />
            <a-select v-model:value="form.quota_unit" class="uf__quota-unit">
              <a-select-option value="GB">GiB</a-select-option>
              <a-select-option value="TB">TiB</a-select-option>
            </a-select>
          </div>
        </div>
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            到期时间
            <LbInfoTip text="留空表示不过期。边界统一取 UTC。" :width="220" />
          </span>
          <a-input v-model:value="form.expires_at" type="date" />
        </div>
      </div>

      <div class="lb-group">
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            流量重置
            <LbInfoTip
              text="每月 1–31 日,超出当月天数时落到当月最后一天;固定 N 天按所选时区的日历日切,边界都在当地 0 点(夏令时那天照样是 0 点)。改规则只影响以后的边界,本周期已经用掉的不追溯。"
              :width="320"
            />
          </span>
          <a-radio-group v-model:value="form.reset_cycle" class="uf__seg">
            <a-radio-button value="NONE">不重置</a-radio-button>
            <a-radio-button value="MONTHLY">每月</a-radio-button>
            <a-radio-button value="DAYS">固定天数</a-radio-button>
          </a-radio-group>
        </div>
        <div v-if="form.reset_cycle === 'MONTHLY'" class="lb-group__row uf__row">
          <span class="lb-group__label">重置日</span>
          <div class="uf__day">
            每月
            <a-input-number v-model:value="form.reset_day" :min="1" :max="31" class="uf__day-num" />
            日
          </div>
        </div>
        <template v-if="form.reset_cycle === 'DAYS'">
          <div class="lb-group__row uf__row">
            <span class="lb-group__label">周期</span>
            <div class="uf__day">
              每
              <a-input-number v-model:value="form.reset_period_days" :min="1" :max="3650" class="uf__day-num" />
              天
            </div>
          </div>
          <div class="lb-group__row uf__row">
            <span class="lb-group__label">
              对齐日
              <LbInfoTip text="周期以这一天的 0 点为对齐点,往前往后每 N 天一个边界。留空 = 用户创建那天。" :width="260" />
            </span>
            <a-input v-model:value="form.reset_anchor_at" placeholder="YYYY-MM-DD,留空 = 创建那天" class="uf__tz" />
          </div>
        </template>
        <div v-if="form.reset_cycle !== 'NONE'" class="lb-group__row uf__row">
          <span class="lb-group__label">时区</span>
          <a-select v-model:value="form.reset_timezone" :options="timezoneOptions" show-search class="uf__tz" />
        </div>
        <div v-if="isEdit && pendingRule && !cycleDirty" class="lb-group__row uf__row uf__pending">
          <span class="lb-group__label">待生效</span>
          <div>
            下一周期起改为 {{ pendingRule }}(当前周期结束时换,本周期用量不受影响)
            <button v-if="!cancelPending" type="button" class="lb-linkbtn" @click="undoPending">撤销</button>
            <span v-else>· 保存后撤销</span>
          </div>
        </div>
        <template v-if="isEdit && cycleDirty">
          <div v-if="props.user && props.user.reset_cycle !== 'NONE'" class="lb-group__row uf__row">
            <span class="lb-group__label">生效方式</span>
            <a-radio-group v-model:value="resetApply" class="uf__seg">
              <a-radio-button value="NEXT">下一周期生效</a-radio-button>
              <a-radio-button value="NOW">立即生效</a-radio-button>
            </a-radio-group>
          </div>
          <div class="lb-group__row uf__row uf__pending">
            <span class="lb-group__label">改动后果</span>
            <div v-if="resetPreview">
              <div>{{ resetPreview.note }}</div>
              <div v-if="resetPreview.next_resets.length">
                之后的重置:{{ resetPreview.next_resets.map(fmtUTC).join('、') }}
              </div>
              <div>改规则本身不清零当前已用的流量,清零只在重置点发生。</div>
            </div>
            <div v-else-if="resetPreviewError" class="uf__err">{{ resetPreviewError }}</div>
            <div v-else>正在计算…</div>
          </div>
        </template>
      </div>

      <template v-if="!isEdit">
        <div class="uf__sub-head">门户登录(可选)</div>
        <div class="lb-group">
          <div class="lb-group__row uf__row">
            <span class="lb-group__label">
              登录账号
              <LbInfoTip text="字母、数字、下划线、连字符与点,3~32 位。留空表示该用户只用订阅,不登录用户中心。" :width="280" />
            </span>
            <div class="uf__field">
              <a-input
                v-model:value="form.login_username"
                placeholder="留空则不开通登录"
                autocomplete="off"
                :status="usernameError ? 'error' : undefined"
              />
              <div v-if="usernameError" class="uf__field-err">{{ usernameError }}</div>
            </div>
          </div>
          <template v-if="form.login_username">
            <div class="lb-group__row uf__row">
              <span class="lb-group__label">
                <span class="uf__req">*</span> 初始密码
                <LbInfoTip
                  warn
                  :width="280"
                  text="至少 8 位。提交后不再回显,也不写进审计日志。请通过安全渠道发给用户。"
                />
              </span>
              <div class="uf__field">
                <a-input-password
                  v-model:value="form.login_password"
                  placeholder="不会回显"
                  autocomplete="new-password"
                  :status="passwordError ? 'error' : undefined"
                />
                <div v-if="passwordError" class="uf__field-err">{{ passwordError }}</div>
              </div>
            </div>
            <div class="lb-group__row uf__row uf__row--switch">
              <span class="lb-group__label">首次登录后须改密码</span>
              <a-switch v-model:checked="form.must_change_password" />
            </div>
          </template>
        </div>
      </template>

      <div class="lb-group">
        <div class="lb-group__row uf__row">
          <span class="lb-group__label">
            额外授权节点
            <LbInfoTip
              :width="300"
              text="在访问等级之外单独追加。等级已经覆盖的节点不必勾选 —— 勾了会把继承关系固化成手工授权,之后调等级就不再自动生效。"
            />
          </span>
          <a-select
            v-model:value="form.node_ids"
            mode="multiple"
            :options="nodeOptions"
            placeholder="通常留空"
          />
        </div>
        <div v-if="isEdit" class="lb-group__row uf__row">
          <span class="lb-group__label">
            用户编号
            <LbInfoTip text="重新生成 UUID 不在这里 —— 它是危险操作,走详情页的确认弹窗。" :width="260" />
          </span>
          <div class="lb-group__value lb-group__value--mono">{{ props.user?.user_code }}</div>
        </div>
      </div>

      <div v-if="!isEdit" class="uf__foot">保存后受影响节点将在数秒内自动部署</div>
      <div v-else-if="dirtyFields.length" class="uf__foot">
        已改动 {{ dirtyFields.length }} 项 · 保存后受影响节点将在数秒内自动部署
      </div>
      <div v-else class="uf__foot">没有改动</div>
    </div>
  </a-modal>
</template>

<style scoped>
.uf {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.uf__err-note {
  color: var(--text2);
  margin-left: 4px;
}
.uf__pending {
  font-size: var(--fs-sm);
  color: var(--text2);
}
.uf__err {
  color: var(--bad);
}

.uf__row {
  grid-template-columns: 130px minmax(0, 1fr);
}
.uf__row--switch {
  grid-template-columns: minmax(0, 1fr) auto;
}
.uf__row :deep(.ant-input),
.uf__row :deep(.ant-input-affix-wrapper),
.uf__row :deep(.ant-select),
.uf__row :deep(.ant-input-number) {
  width: 100%;
}

.uf__req {
  color: var(--bad);
}

.uf__quota {
  display: flex;
  gap: 8px;
  min-width: 0;
}
.uf__quota-num {
  flex: 1;
  min-width: 0;
}
.uf__quota :deep(.uf__quota-unit) {
  width: 90px;
  flex: none;
}

.uf__day {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: var(--fs-base);
}
.uf__day :deep(.uf__day-num) {
  width: 90px;
}
.uf__tz {
  width: 220px;
  max-width: 100%;
}

.uf__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.uf__field-err {
  font-size: var(--fs-xs);
  color: var(--bad);
}

.uf__sub-head {
  font-size: var(--fs-base);
  font-weight: 600;
  padding: 0 4px;
  margin-bottom: -8px;
}

.uf__foot {
  font-size: var(--fs-sm);
  color: var(--text3);
  text-align: center;
}

@media (max-width: 767px) {
  .uf__row {
    grid-template-columns: 1fr;
  }
}
</style>
