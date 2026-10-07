<script setup lang="ts">
import { computed } from 'vue'
import { LbInfoTip } from '@/components/lb'
import { fromLocalInput, toLocalInput, type ExpiryFormValue } from './expiryMeta'

/**
 * 供应商到期档案的那几个字段。节点 / 外部代理 / 代理源三个编辑表单与「续费」弹窗
 * 共用这一份 —— 文案、校验、「留空 = 未设置」的含义只写一遍。
 *
 * expires_at 对外是 RFC3339 UTC,输入框按浏览器本地时间显示;
 * lead_days 对外是数组,输入框是逗号分隔的字符串(空 = 继承系统规则)。
 */
const props = defineProps<{
  modelValue: ExpiryFormValue
  /** 全局的提前天数,占位符里显示「继承:7,3,1」 */
  defaultLeadDays?: number[]
  disabled?: boolean
  /** 紧凑布局(放在别的表单里) */
  compact?: boolean
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: ExpiryFormValue): void }>()

function set<K extends keyof ExpiryFormValue>(key: K, value: ExpiryFormValue[K]) {
  emit('update:modelValue', { ...props.modelValue, [key]: value })
}

const localExpires = computed({
  get: () => toLocalInput(props.modelValue.expires_at),
  set: (v: string) => set('expires_at', fromLocalInput(v)),
})

const leadPlaceholder = computed(
  () => `留空继承系统规则(${(props.defaultLeadDays ?? [7, 3, 1]).join(',')})`,
)
</script>

<template>
  <div class="xf" :class="{ 'xf--compact': compact }">
    <a-form-item label="商家到期时间">
      <a-input v-model:value="localExpires" type="datetime-local" :disabled="disabled" />
      <div class="xf__help">
        商家账单上的到期时间,按本机时区填写。留空表示<strong>未设置</strong>(不是永不过期)。
        只用于提醒:到期不会停服务、不会删节点、不会关订阅。
      </div>
    </a-form-item>

    <a-row :gutter="12">
      <a-col :span="12">
        <a-form-item label="到期提醒">
          <a-switch
            :checked="modelValue.reminder_enabled"
            :disabled="disabled"
            @update:checked="(v: boolean) => set('reminder_enabled', v)"
          />
          <span class="xf__inline">{{ modelValue.reminder_enabled ? '开启' : '关闭' }}</span>
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item>
          <template #label>
            已在商家开启自动续费
            <LbInfoTip
              :width="300"
              text="这是你在商家那边的登记状态,面板不代为扣款、不执行续费。开着仍然提醒,只是文案变成「请确认余额及扣费结果」。到期后没登记新的到期时间,面板不会假定续费成功。"
            />
          </template>
          <a-switch
            :checked="modelValue.auto_renew"
            :disabled="disabled"
            @update:checked="(v: boolean) => set('auto_renew', v)"
          />
          <span class="xf__inline">{{ modelValue.auto_renew ? '已开启' : '手动续费' }}</span>
        </a-form-item>
      </a-col>
    </a-row>

    <a-row :gutter="12">
      <a-col :span="12">
        <a-form-item label="商家名称">
          <a-input
            :value="modelValue.vendor_name"
            :maxlength="64"
            :disabled="disabled"
            placeholder="例如:Vultr"
            @update:value="(v: string) => set('vendor_name', v)"
          />
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item label="续费页面">
          <a-input
            :value="modelValue.vendor_url"
            :disabled="disabled"
            placeholder="https://…(可选,进提醒正文)"
            @update:value="(v: string) => set('vendor_url', v)"
          />
        </a-form-item>
      </a-col>
    </a-row>

    <a-row :gutter="12">
      <a-col :span="12">
        <a-form-item label="提前天数(覆盖)">
          <a-input
            :value="modelValue.lead_days_text"
            :disabled="disabled"
            :placeholder="leadPlaceholder"
            @update:value="(v: string) => set('lead_days_text', v)"
          />
        </a-form-item>
      </a-col>
      <a-col :span="12">
        <a-form-item label="内部备注">
          <a-input
            :value="modelValue.note"
            :maxlength="512"
            :disabled="disabled"
            placeholder="只在管理后台出现"
            @update:value="(v: string) => set('note', v)"
          />
        </a-form-item>
      </a-col>
    </a-row>
  </div>
</template>

<style scoped>
.xf__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--text3);
}
.xf__inline {
  margin-left: 10px;
  font-size: 12.5px;
  color: var(--text2);
}
.xf--compact :deep(.ant-form-item) {
  margin-bottom: 12px;
}
</style>
