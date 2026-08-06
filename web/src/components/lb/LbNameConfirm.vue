<script setup lang="ts">
import { computed, ref, watch } from 'vue'

/**
 * 输入资源名称确认。只给四个不可逆操作:
 * 删除用户、删除节点、卸载服务、重置主机密钥。
 *
 * 要求输入的是**内部名称**而不是展示名称 —— 内部名称唯一,展示名称可以重复。
 * 名称不匹配时主按钮保持禁用(而不是点了报错)。
 */
const props = withDefaults(
  defineProps<{
    open: boolean
    title: string
    /** 必须原样输入的名称 */
    name: string
    impacts: string[]
    okText?: string
    /** 输入框上方那句话,默认「输入内部名称以确认」 */
    prompt?: string
    loading?: boolean
  }>(),
  { okText: '删除', prompt: '输入内部名称以确认' },
)

const emit = defineEmits<{
  (e: 'update:open', v: boolean): void
  (e: 'confirm'): void
}>()

const typed = ref('')
const matched = computed(() => typed.value.trim() === props.name)

// 每次打开都清空,免得上一次的输入让按钮一开就是可点的。
watch(
  () => props.open,
  (v) => {
    if (v) typed.value = ''
  },
)
</script>

<template>
  <a-modal
    :open="props.open"
    :title="props.title"
    :width="460"
    :confirm-loading="props.loading"
    :ok-button-props="{ disabled: !matched, danger: true }"
    :ok-text="props.okText"
    cancel-text="取消"
    @update:open="(v: boolean) => emit('update:open', v)"
    @ok="emit('confirm')"
  >
    <div class="lb-nc">
      <div class="lb-nc__impacts">
        <div v-for="(t, i) in props.impacts" :key="i">· {{ t }}</div>
      </div>

      <div class="lb-nc__field">
        <div class="lb-nc__prompt">
          {{ props.prompt }}
          <code class="lb-nc__name">{{ props.name }}</code>
        </div>
        <a-input v-model:value="typed" :placeholder="props.name" autocomplete="off" spellcheck="false" />
      </div>
    </div>
  </a-modal>
</template>

<style scoped>
.lb-nc {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.lb-nc__impacts {
  padding: 10px 11px;
  background: #fdecea;
  border: 1px solid #f3cfc9;
  border-radius: 6px;
  font-size: 11.5px;
  line-height: 1.75;
  color: #8e2117;
}

.lb-nc__field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.lb-nc__prompt {
  font-size: 12px;
  color: #576070;
}

.lb-nc__name {
  padding: 1px 5px;
  margin-left: 4px;
  background: #f1f3f5;
  border: 1px solid #e3e6ea;
  border-radius: 3px;
  font-family: 'IBM Plex Mono', ui-monospace, monospace;
  font-size: 12px;
  color: #15181c;
  user-select: all;
}
</style>
