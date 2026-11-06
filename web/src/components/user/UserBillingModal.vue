<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { message } from 'ant-design-vue'
import { api, ApiError, type ProxyUser } from '@/api/client'
import { formatBytes } from '@/utils/format'

/**
 * 用户计费规则(VNext TRAFFIC02 / 08)。
 *
 * 计费流量 = 倍率 × f(本周期上行合计, 下行合计),f 是只算上行 / 只算下行 / 双向合计 / 双向取大。
 * 取大是对**整个周期的合计**取,不是逐次采样、逐天或逐跳取大再相加。
 * 默认下一周期生效(这一周期已经用掉的不被追溯重算);立即生效时旧段按旧规则算好封存、新段从此刻起算,
 * 结果明确是分段合计。原始上行 / 下行字节一个都不动。预览由后端算,前端不推导。
 */
const props = defineProps<{ open: boolean; user: ProxyUser | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const mode = ref<'SUM' | 'UP' | 'DOWN' | 'MAX'>('SUM')
const multiplier = ref(1)
const immediate = ref(false)
const preview = ref<{ current_label: string; next_label: string; billed_before: number; billed_after: number; exceeded_after: boolean } | null>(null)
const previewError = ref('')
const saving = ref(false)

const noCycle = computed(() => props.user?.reset_cycle === 'NONE')

watch(
  () => props.open,
  (open) => {
    if (!open || !props.user) return
    mode.value = props.user.billing_mode || 'SUM'
    multiplier.value = (props.user.billing_multiplier_pct || 100) / 100
    // 不按周期重置的用户没有「下一周期」,只能立即生效。
    immediate.value = noCycle.value
    preview.value = null
  },
  { immediate: true },
)

function body(previewOnly: boolean) {
  return {
    mode: mode.value,
    multiplier_pct: Math.round((multiplier.value || 0) * 100),
    immediate: immediate.value,
    preview: previewOnly,
  }
}

let seq = 0
watch([mode, multiplier, immediate, () => props.open], async () => {
  if (!props.open || !props.user) return
  const mine = ++seq
  try {
    const p = await api.setUserBilling(props.user.id, body(true))
    if (mine === seq) {
      preview.value = p
      previewError.value = ''
    }
  } catch (e) {
    if (mine === seq) previewError.value = e instanceof ApiError ? e.message : '预览失败'
  }
})

async function save() {
  if (!props.user) return
  saving.value = true
  try {
    await api.setUserBilling(props.user.id, body(false))
    message.success(immediate.value ? '已改,立即生效(旧段已封存)' : '已改,下一周期生效')
    emit('saved')
    emit('update:open', false)
  } catch (e) {
    message.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <a-modal
    :open="open"
    title="计费规则"
    width="520px"
    ok-text="保存"
    cancel-text="取消"
    :confirm-loading="saving"
    :ok-button-props="{ disabled: !!previewError }"
    @ok="save"
    @cancel="emit('update:open', false)"
  >
    <div class="ub">
      <div class="ub__hint">
        计费流量 = 倍率 × 方向函数(本周期上行合计,下行合计)。取大是对整个周期的合计取,不是逐次采样取大再相加。
        额度(quota)比较的是计费流量;上行、下行的原始字节一个都不改。
      </div>
      <a-form layout="vertical">
        <a-form-item label="方向">
          <a-radio-group v-model:value="mode" button-style="solid" size="small">
            <a-radio-button value="SUM">双向合计</a-radio-button>
            <a-radio-button value="DOWN">只算下行</a-radio-button>
            <a-radio-button value="UP">只算上行</a-radio-button>
            <a-radio-button value="MAX">双向取大</a-radio-button>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="倍率">
          <a-input-number v-model:value="multiplier" :min="0.01" :max="100" :step="0.1" :precision="2" addon-before="×" />
        </a-form-item>
        <a-form-item label="何时生效">
          <a-radio-group v-model:value="immediate">
            <a-radio :value="false" :disabled="noCycle">下一周期生效(默认)</a-radio>
            <a-radio :value="true">立即生效(旧段封存)</a-radio>
          </a-radio-group>
          <div v-if="noCycle" class="ub__hint">这个用户的额度不按周期重置,没有「下一周期」,只能立即生效。</div>
        </a-form-item>
      </a-form>
      <div v-if="previewError" class="ub__bad">{{ previewError }}</div>
      <div v-else-if="preview" class="ub__preview">
        <div>{{ preview.current_label }} → <b>{{ preview.next_label }}</b></div>
        <div v-if="immediate">
          本周期计费流量 {{ formatBytes(preview.billed_before) }} → {{ formatBytes(preview.billed_after) }}(分段合计,切换那一刻不跳变,之后按新规则累加)
        </div>
        <div v-else>本周期仍按当前规则计费,下次重置时换成新规则。</div>
        <div v-if="preview.exceeded_after" class="ub__bad">按新规则已经达到额度:保存后这个用户会被停用,并触发受影响节点重新下发。</div>
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.ub {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.ub__hint,
.ub__preview {
  font-size: var(--fs-xs);
  color: var(--text2);
}
.ub__preview {
  background: var(--surface2);
  border-radius: var(--r-group);
  padding: var(--sp-2) var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.ub__bad {
  color: var(--bad);
  font-size: var(--fs-xs);
}
</style>
