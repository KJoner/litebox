<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import {
  api,
  ApiError,
  type AccessTier,
  type AdjustAction,
  type AdjustPayload,
  type ProxyUser,
} from '@/api/client'
import { formatBytes, formatQuota } from '@/utils/format'
import { LbResultList, type LbResultItem } from '@/components/lb'
import { toBytes, type LbQuotaUnit } from './quota'
import { daysUntil } from '@/components/lb/derive'

/**
 * 续期 / 额度调整。**一个弹窗承载 8 种 action,不是两个弹窗** ——
 * 代码里它们是同一个 adjustUser 接口下的 8 个 action,批量走的也是它。
 * 入口不同则预选不同的 action:列表的「续期」进来预选 EXTEND_EXPIRY,
 * 「加流量」进来预选 ADD_QUOTA。
 *
 * 这个弹窗的核心是「执行后」预览。八种 action 的语义差别很细:
 * 延长对已过期用户从今天起算、对未过期用户从原到期日起算;增加流量可填负数;
 * 设置额度填 0 变不限量;重置流量会让因超额被停的用户自动恢复。
 * 这些规则写在 extra 帮助文字里没人会逐条读 —— 直接把结果算出来摆在按钮上方,
 * 并把按钮文案写成具体动作(「延长 90 天」而不是「执行」)。
 */
const props = defineProps<{
  open: boolean
  /** 单个用户;null 表示对 targets 批量调整 */
  user: ProxyUser | null
  targets: ProxyUser[]
  tiers: AccessTier[]
  initialAction?: AdjustAction
}>()

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'done'): void
}>()

const batch = computed(() => props.user === null)
const list = computed(() => (props.user ? [props.user] : props.targets))

const actions: { value: AdjustAction; label: string }[] = [
  { value: 'EXTEND_EXPIRY', label: '延长有效期' },
  { value: 'ADD_QUOTA', label: '增加流量' },
  { value: 'SET_EXPIRY', label: '设置到期时间' },
  { value: 'SET_QUOTA', label: '设置额度' },
  { value: 'RESET_TRAFFIC', label: '重置已用流量' },
  { value: 'CHANGE_TIER', label: '调整等级' },
  { value: 'ENABLE_USER', label: '启用账号' },
  { value: 'DISABLE_USER', label: '停用账号' },
]

const form = reactive({
  action: 'EXTEND_EXPIRY' as AdjustAction,
  expiry_days: 30,
  expires_at: '',
  quota_value: 10 as number | null,
  quota_unit: 'GB' as LbQuotaUnit,
  access_tier_id: 1,
  remark: '',
})

const submitting = ref(false)
/** 批量的逐条结果。非空即进入结果态,弹窗不自动关。 */
const results = ref<LbResultItem[] | null>(null)

watch(
  () => props.open,
  (open) => {
    if (!open) return
    results.value = null
    form.action = props.initialAction ?? 'EXTEND_EXPIRY'
    form.expiry_days = 30
    form.expires_at = ''
    form.quota_value = 10
    form.quota_unit = 'GB'
    form.access_tier_id = props.user?.access_tier_id ?? props.tiers[0]?.id ?? 1
    form.remark = ''
  },
)

/** 单个用户时算出「执行后」的具体结果。批量时只说规则。 */
const preview = computed(() => {
  const u = props.user
  switch (form.action) {
    case 'EXTEND_EXPIRY': {
      if (!u) return { note: `已过期的用户从今天起算,未到期的从原到期日起算。` }
      const base = u.expires_at && (daysUntil(u.expires_at) ?? -1) >= 0
        ? new Date(u.expires_at)
        : new Date()
      const after = new Date(base.getTime() + form.expiry_days * 86400000)
      return {
        from: u.expires_at ? u.expires_at.slice(0, 10) : '不过期',
        to: after.toISOString().slice(0, 10),
        note:
          u.status === 'EXPIRED'
            ? '该用户已过期,延长从今天起算而不是从原到期日。状态会回到「正常」,并触发受影响节点重新部署。'
            : '从原到期日起算。',
      }
    }
    case 'SET_EXPIRY':
      return {
        from: u?.expires_at ? u.expires_at.slice(0, 10) : '不过期',
        to: form.expires_at || '不过期',
        note: form.expires_at ? '' : '留空表示改为不过期。',
      }
    case 'ADD_QUOTA': {
      const delta = toBytes(Math.abs(form.quota_value ?? 0), form.quota_unit) * (Number(form.quota_value) < 0 ? -1 : 1)
      if (!u) return { note: '填负数表示扣减。不限量的用户请改用「设置额度」。' }
      if (u.quota_bytes <= 0) return { note: '该用户当前不限量,增加流量无意义。请改用「设置额度」。' }
      return {
        from: formatQuota(u.quota_bytes),
        to: formatBytes(Math.max(0, u.quota_bytes + delta)),
        note: '填负数表示扣减。',
      }
    }
    case 'SET_QUOTA': {
      const v = toBytes(form.quota_value, form.quota_unit)
      return {
        from: u ? formatQuota(u.quota_bytes) : undefined,
        to: v > 0 ? formatBytes(v) : '不限量',
        note: '填 0 表示不限量。',
      }
    }
    case 'RESET_TRAFFIC':
      return {
        from: u ? formatBytes(u.used_total) : undefined,
        to: '0 B',
        note: '历史流水与节点计数器基线保持不变。此前因超额被停的用户会自动恢复。',
      }
    case 'CHANGE_TIER':
      return {
        from: u?.access_tier_name,
        to: props.tiers.find((t) => t.id === form.access_tier_id)?.name,
        note: '等级变更会自动标脏受影响节点并排入重新部署 —— 拖着不部署等于被移出的用户还能继续用。',
      }
    case 'DISABLE_USER':
      return { note: '凭据在受影响节点重新部署后失效。门户仍可登录,但看不到订阅地址。随时可恢复。' }
    default:
      return { note: '恢复后凭据会重新下发到受影响节点。' }
  }
})

const okText = computed(() => {
  const n = list.value.length
  const many = batch.value ? `为 ${n} 个用户` : ''
  switch (form.action) {
    case 'EXTEND_EXPIRY':
      return `${many}延长 ${form.expiry_days} 天`
    case 'ADD_QUOTA':
      return `${many}调整流量`
    case 'SET_QUOTA':
      return `${many}设置额度`
    case 'SET_EXPIRY':
      return `${many}设置到期时间`
    case 'RESET_TRAFFIC':
      return `${many}重置流量`
    case 'CHANGE_TIER':
      return `${many}调整等级`
    case 'DISABLE_USER':
      return `${many}停用`
    default:
      return `${many}启用`
  }
})

/** 批量时挑出「这次操作对它没意义」的对象,提前提示而不是执行完才说。 */
const mismatched = computed(() => {
  if (!batch.value) return []
  if (form.action === 'EXTEND_EXPIRY' || form.action === 'SET_EXPIRY') {
    return props.targets.filter((u) => !u.expires_at).map((u) => u.display_name)
  }
  if (form.action === 'ADD_QUOTA') {
    return props.targets.filter((u) => u.quota_bytes <= 0).map((u) => u.display_name)
  }
  return []
})

function payload(): AdjustPayload {
  const body: AdjustPayload = { action: form.action, remark: form.remark }
  switch (form.action) {
    case 'ADD_QUOTA':
      body.quota_delta_bytes =
        toBytes(Math.abs(form.quota_value ?? 0), form.quota_unit) *
        (Number(form.quota_value) < 0 ? -1 : 1)
      break
    case 'SET_QUOTA':
      body.quota_bytes = toBytes(form.quota_value, form.quota_unit)
      break
    case 'EXTEND_EXPIRY':
      body.expiry_delta_days = form.expiry_days
      break
    case 'SET_EXPIRY':
      body.expires_at = form.expires_at ? `${form.expires_at}T23:59:59Z` : ''
      break
    case 'CHANGE_TIER':
      body.access_tier_id = form.access_tier_id
      break
  }
  return body
}

async function submit() {
  submitting.value = true
  try {
    if (!batch.value) {
      await api.adjustUser(props.user!.id, payload())
      message.success('已调整')
      emit('update:open', false)
      emit('done')
      return
    }

    // 逐条推进,中途失败不中止剩余的。已成功的不会回滚 —— 批量不是事务。
    results.value = props.targets.map((u) => ({ id: u.id, name: u.display_name }))
    const body = payload()
    const r = await api.batchAdjust(props.targets.map((u) => u.id), body)
    const byId = new Map(r.items.map((i) => [i.user_id, i]))
    results.value = props.targets.map((u) => {
      const it = byId.get(u.id)
      return {
        id: u.id,
        name: u.display_name,
        ok: it?.ok ?? false,
        detail: it?.ok ? '已处理' : (it?.error ?? '未知原因'),
      }
    })
    if (r.succeeded === r.total) {
      message.success(`已处理 ${r.succeeded} 个用户`)
      emit('update:open', false)
      results.value = null
    }
    emit('done')
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '调整失败')
    results.value = null
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <a-modal
    :open="props.open"
    :title="batch ? `批量调整 ${list.length} 个用户` : `续期 / 调整 · ${props.user?.display_name}`"
    :width="560"
    :confirm-loading="submitting"
    :ok-text="okText"
    cancel-text="取消"
    :mask-closable="!submitting"
    :closable="!submitting"
    :footer="results ? null : undefined"
    @cancel="emit('update:open', false)"
    @ok="submit"
  >
    <!-- 结果态:失败行可单独重试,弹窗不自动关。 -->
    <LbResultList v-if="results" :items="results" />

    <template v-else>
      <a-form layout="vertical">
        <a-form-item label="操作">
          <div class="ua__actions">
            <a-button
              v-for="a in actions"
              :key="a.value"
              size="small"
              :type="form.action === a.value ? 'primary' : 'default'"
              @click="form.action = a.value"
            >
              {{ a.label }}
            </a-button>
          </div>
        </a-form-item>

        <a-form-item v-if="form.action === 'EXTEND_EXPIRY'" label="延长天数">
          <div class="ua__row">
            <a-button
              v-for="d in [30, 90, 180, 365]"
              :key="d"
              size="small"
              :type="form.expiry_days === d ? 'primary' : 'default'"
              @click="form.expiry_days = d"
            >
              +{{ d }}
            </a-button>
            <a-input-number v-model:value="form.expiry_days" :step="30" style="flex: 1" />
          </div>
        </a-form-item>

        <a-form-item v-else-if="form.action === 'SET_EXPIRY'" label="到期时间">
          <a-input v-model:value="form.expires_at" type="date" style="width: 100%" />
        </a-form-item>

        <a-row v-else-if="form.action === 'ADD_QUOTA' || form.action === 'SET_QUOTA'" :gutter="12">
          <a-col :span="14">
            <a-form-item :label="form.action === 'ADD_QUOTA' ? '增加流量' : '流量额度'">
              <a-input-number v-model:value="form.quota_value" :precision="2" style="width: 100%" />
            </a-form-item>
          </a-col>
          <a-col :span="10">
            <a-form-item label="单位">
              <a-select v-model:value="form.quota_unit">
                <a-select-option value="GB">GB</a-select-option>
                <a-select-option value="TB">TB</a-select-option>
              </a-select>
            </a-form-item>
          </a-col>
        </a-row>

        <a-form-item v-else-if="form.action === 'CHANGE_TIER'" label="访问等级">
          <a-select v-model:value="form.access_tier_id">
            <a-select-option v-for="t in props.tiers" :key="t.id" :value="t.id">
              {{ t.name }}
            </a-select-option>
          </a-select>
        </a-form-item>

        <!-- 批量时先列出对象,并标出其中不适合本次操作的 -->
        <div v-if="batch" class="ua__targets">
          <div class="ua__targets-head">将被调整的用户</div>
          <div v-for="u in list" :key="u.id" class="ua__target">
            <span class="lb-ellipsis">{{ u.display_name }}</span>
            <span class="ua__target-meta lb-mono">
              {{ u.expires_at ? u.expires_at.slice(0, 10) : '不过期' }}
            </span>
          </div>
        </div>
        <a-alert
          v-if="mismatched.length"
          type="warning"
          show-icon
          class="ua__warn"
          :message="`${mismatched.join('、')} 不适用本次操作`"
          description="这多半不是你想要的。请从选择中移除,或换一个操作。"
        />

        <!-- 「执行后」预览:把结果算出来摆在按钮上方,而不是写在帮助文字里 -->
        <div class="ua__preview">
          <div class="ua__preview-title">执行后</div>
          <div v-if="preview.to" class="ua__preview-diff lb-mono">
            <span v-if="preview.from" class="ua__from">{{ preview.from }}</span>
            <span v-if="preview.from">→</span>
            <strong>{{ preview.to }}</strong>
          </div>
          <div v-if="preview.note" class="ua__preview-note">{{ preview.note }}</div>
        </div>

        <a-form-item>
          <template #label>
            备注 <span class="ua__label-warn">用户可见</span>
          </template>
          <a-input v-model:value="form.remark" :maxlength="128" placeholder="例如:2026 年 8 月续费" />
          <div class="ua__help">
            这句话会出现在用户门户的「最近调整」里。不要写内部说明。
            <template v-if="batch">这 {{ list.length }} 个用户会看到同一句话。</template>
          </div>
        </a-form-item>
      </a-form>

      <div v-if="batch" class="ua__foot">逐个执行,失败不影响其余。已成功的不会回滚。</div>
    </template>
  </a-modal>
</template>

<style scoped>
.ua__actions,
.ua__row {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
  align-items: center;
}

.ua__targets {
  margin-bottom: 16px;
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.ua__targets-head {
  padding: 8px 11px;
  background: #f6f7f9;
  border-bottom: 1px solid #edeff2;
  font-size: 11.5px;
  font-weight: 600;
  color: #576070;
}

.ua__target {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 11px;
  font-size: 12px;
}

.ua__target + .ua__target {
  border-top: 1px solid #edeff2;
}

.ua__target-meta {
  margin-left: auto;
  font-size: 11px;
  color: #6b7480;
}

.ua__warn {
  margin-bottom: 16px;
}

.ua__preview {
  display: flex;
  flex-direction: column;
  gap: 7px;
  margin-bottom: 20px;
  padding: 11px 13px;
  background: #eef4fc;
  border: 1px solid #c9dcf3;
  border-radius: 6px;
}

.ua__preview-title {
  font-size: 11.5px;
  font-weight: 600;
  color: #1d4f96;
}

.ua__preview-diff {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12.5px;
  color: #1d4f96;
}

.ua__from {
  color: #7fa8da;
  text-decoration: line-through;
}

.ua__preview-note {
  font-size: 11.5px;
  line-height: 1.65;
  color: #4a7bbe;
}

.ua__label-warn {
  font-size: 11.5px;
  font-weight: 400;
  color: #b4291d;
}

.ua__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}

.ua__foot {
  font-size: 11.5px;
  color: #6b7480;
}
</style>
