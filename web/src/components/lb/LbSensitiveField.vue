<script setup lang="ts">
import { computed } from 'vue'

/**
 * 可写的敏感字段。LbCopyField 是只读展示,这个是可写的,
 * 而且带「留空 = 保持原值」语义 —— 这条语义原来靠每处 extra 文案各写一遍,
 * 节点表单和用户表单的措辞已经不一致了。
 *
 * 两种模式:
 *   create —— 必填,写入后从内存抹掉(调用方在提交成功后置空 v-model)
 *   edit   —— 编辑态永不回显。占位必须写明「留空表示保持不变」,
 *             因为空输入框在编辑页默认读作「这里没有值」,会让人以为密钥丢了。
 */
const props = withDefaults(
  defineProps<{
    value: string
    label: string
    mode?: 'create' | 'edit'
    /** 私钥用 textarea,密码用 input */
    multiline?: boolean
    required?: boolean
    help?: string
    rows?: number
    placeholder?: string
  }>(),
  { mode: 'create', multiline: false, required: false, rows: 5 },
)

const emit = defineEmits<{ (e: 'update:value', v: string): void }>()

const model = computed({
  get: () => props.value,
  set: (v: string) => emit('update:value', v),
})

const ph = computed(
  () => props.placeholder ?? (props.mode === 'edit' ? '留空表示保持不变' : ''),
)
</script>

<template>
  <a-form-item :required="props.required && props.mode === 'create'">
    <template #label>
      <span class="lb-sens__label">
        {{ props.label }}
        <span class="lb-sens__badge">
          <svg width="7" height="8" viewBox="0 0 8 9" aria-hidden="true">
            <path d="M2 4V2.6a2 2 0 0 1 4 0V4" fill="none" stroke="#92610A" stroke-width="1.2" />
            <rect x="1" y="4" width="6" height="4.4" rx="1" fill="#92610A" />
          </svg>
          {{ props.mode === 'edit' ? '留空即不变' : '不会回显' }}
        </span>
      </span>
    </template>

    <a-textarea
      v-if="props.multiline"
      v-model:value="model"
      :rows="props.rows"
      :placeholder="ph"
      autocomplete="off"
      spellcheck="false"
    />
    <!-- new-password 让浏览器不去填管理员自己的密码 -->
    <a-input-password v-else v-model:value="model" :placeholder="ph" autocomplete="new-password" />

    <div v-if="props.help" class="lb-sens__help">{{ props.help }}</div>
  </a-form-item>
</template>

<style scoped>
.lb-sens__label {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.lb-sens__badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 0 5px;
  background: #fcf3e3;
  border: 1px solid #efdcb4;
  border-radius: 3px;
  font-size: 11px;
  font-weight: 500;
  color: #92610a;
}

.lb-sens__help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: #6b7480;
}
</style>
