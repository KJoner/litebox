<script setup lang="ts">
import { computed } from 'vue'
import { LbInfoTip } from '@/components/lb'
import { fromLocalInput, toLocalInput } from './expiryMeta'
import { commonCurrencies, cycleOptions, type BillingFormValue } from './billingMeta'

/**
 * 套餐计费字段:周期、币种、金额、本期起点与实际支付。只在「套餐与续费」弹窗里出现 ——
 * 节点 / 外部代理 / 代理源的编辑表单仍然只有到期那几栏,保存时不带它们(后端按「保持原值」处理)。
 */
const props = defineProps<{
  modelValue: BillingFormValue
  disabled?: boolean
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: BillingFormValue): void }>()

function set<K extends keyof BillingFormValue>(key: K, value: BillingFormValue[K]) {
  emit('update:modelValue', { ...props.modelValue, [key]: value })
}

const custom = computed(() => props.modelValue.billing_cycle === 'CUSTOM')
const needsStart = computed(() => ['ONE_TIME', 'TRAFFIC_PACK'].includes(props.modelValue.billing_cycle))

const localStart = computed({
  get: () => toLocalInput(props.modelValue.period_start),
  set: (v: string) => set('period_start', fromLocalInput(v)),
})
const localPurchased = computed({
  get: () => (props.modelValue.purchased_at ? toLocalInput(props.modelValue.purchased_at).slice(0, 10) : ''),
  set: (v: string) => set('purchased_at', v ? fromLocalInput(`${v}T00:00`) : ''),
})
const currencyOptions = commonCurrencies.map((c) => ({ value: c }))
</script>

<template>
  <div class="bf">
    <a-row :gutter="12">
      <a-col :xs="24" :sm="12">
        <a-form-item label="套餐名称">
          <a-input
            :value="modelValue.plan_name"
            :maxlength="64"
            :disabled="disabled"
            placeholder="例如:1C1G-香港 CN2"
            @update:value="(v: string) => set('plan_name', v)"
          />
        </a-form-item>
      </a-col>
      <a-col :xs="24" :sm="12">
        <a-form-item label="计费周期">
          <div class="bf__cycle">
            <a-select
              :value="modelValue.billing_cycle"
              :options="cycleOptions"
              :disabled="disabled"
              @update:value="(v: BillingFormValue['billing_cycle']) => set('billing_cycle', v)"
            />
            <template v-if="custom">
              <a-input-number
                :value="modelValue.cycle_count"
                :min="1"
                :max="modelValue.cycle_unit === 'DAY' ? 3650 : 120"
                :precision="0"
                :disabled="disabled"
                placeholder="N"
                class="bf__count"
                @update:value="(v: number | null) => set('cycle_count', v)"
              />
              <a-select
                :value="modelValue.cycle_unit || 'MONTH'"
                :disabled="disabled"
                class="bf__unit"
                :options="[{ value: 'MONTH', label: '个月' }, { value: 'DAY', label: '天' }]"
                @update:value="(v: 'MONTH' | 'DAY') => set('cycle_unit', v)"
              />
            </template>
          </div>
        </a-form-item>
      </a-col>
    </a-row>

    <a-row :gutter="12">
      <a-col :xs="12" :sm="6">
        <a-form-item label="币种">
          <a-auto-complete
            :value="modelValue.currency"
            :options="currencyOptions"
            :disabled="disabled"
            placeholder="CNY"
            @update:value="(v: string) => set('currency', (v ?? '').toUpperCase())"
          />
        </a-form-item>
      </a-col>
      <a-col :xs="12" :sm="9">
        <a-form-item>
          <template #label>
            套餐价格
            <LbInfoTip :width="260" text="每个计费周期的标价,填十进制金额(如 5 或 12.99)。0 表示免费;留空表示没填。" />
          </template>
          <a-input
            :value="modelValue.price"
            :disabled="disabled"
            inputmode="decimal"
            placeholder="每期标价"
            @update:value="(v: string) => set('price', v)"
          />
        </a-form-item>
      </a-col>
      <a-col :xs="24" :sm="9">
        <a-form-item>
          <template #label>
            本次实际支付
            <LbInfoTip :width="280" text="这一期实际付了多少(有优惠时与标价不同)。留空按套餐价格估算。剩余未摊销价值按它来算。" />
          </template>
          <a-input
            :value="modelValue.period_paid"
            :disabled="disabled"
            inputmode="decimal"
            placeholder="留空 = 与套餐价格相同"
            @update:value="(v: string) => set('period_paid', v)"
          />
        </a-form-item>
      </a-col>
    </a-row>

    <a-row :gutter="12">
      <a-col :xs="24" :sm="12">
        <a-form-item>
          <template #label>
            本期起点
            <LbInfoTip
              :width="300"
              text="本次已付费周期从什么时候开始。留空时按「到期时间往前推一个周期」现算(按日历月,1 月 31 日往前一个月是 12 月 31 日)。一次性与流量包没有固定周期,请填它或购买日期。"
            />
          </template>
          <a-input v-model:value="localStart" type="datetime-local" :disabled="disabled" />
          <div v-if="needsStart && !modelValue.period_start && !modelValue.purchased_at" class="bf__help">
            一次性 / 流量包需要本期起点或购买日期,才能按时间估算。
          </div>
        </a-form-item>
      </a-col>
      <a-col :xs="12" :sm="6">
        <a-form-item label="购买日期">
          <a-input v-model:value="localPurchased" type="date" :disabled="disabled" />
        </a-form-item>
      </a-col>
      <a-col :xs="12" :sm="6">
        <a-form-item>
          <template #label>
            不限流量
            <LbInfoTip :width="240" text="商家套餐不限流量。打开后不做「按流量估算」(没有分母)。" />
          </template>
          <a-switch
            :checked="modelValue.traffic_unlimited"
            :disabled="disabled"
            @update:checked="(v: boolean) => set('traffic_unlimited', v)"
          />
        </a-form-item>
      </a-col>
    </a-row>
  </div>
</template>

<style scoped>
.bf__cycle {
  display: flex;
  gap: var(--sp-2);
}
.bf__cycle > :first-child {
  flex: 1;
  min-width: 0;
}
.bf__count {
  width: 76px;
}
.bf__unit {
  width: 84px;
}
.bf__help {
  margin-top: 4px;
  font-size: var(--fs-xs);
  color: var(--warn);
}
.bf :deep(.ant-form-item) {
  margin-bottom: 12px;
}
</style>
