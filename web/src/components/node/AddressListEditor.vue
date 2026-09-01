<script setup lang="ts">
import { ref } from 'vue'

/**
 * 地址列表编辑器(V16):上半部分是已加的地址列表,下面一行是输入框 + 添加按钮。
 *
 * 每一行带 id 的是存量地址,就地改值会保住 id —— 后端按 id 做增量,改值不影响
 * 引用它的入口地址条目;删掉一行才会连带把那些条目撤下来。新加的没有 id。
 *
 * 这个组件不认识"族"(V4/V6),只管一串 {id?, address};由父组件给它贴上族。
 */
const props = defineProps<{
  modelValue: { id?: number; address: string }[]
  placeholder: string
  /** 面板一次都不解析这一栏,填错发现不了 —— 这句提示要显示出来。 */
  hint?: string
}>()
const emit = defineEmits<{ 'update:modelValue': [{ id?: number; address: string }[]] }>()

const draft = ref('')

function add() {
  const v = draft.value.trim()
  if (!v) return
  emit('update:modelValue', [...props.modelValue, { address: v }])
  draft.value = ''
}
function removeAt(i: number) {
  const next = props.modelValue.slice()
  next.splice(i, 1)
  emit('update:modelValue', next)
}
function editAt(i: number, address: string) {
  const next = props.modelValue.slice()
  next[i] = { ...next[i], address }
  emit('update:modelValue', next)
}
</script>

<template>
  <div class="ale">
    <div v-if="modelValue.length" class="ale__list">
      <div v-for="(row, i) in modelValue" :key="row.id ?? `new-${i}`" class="ale__row">
        <a-input
          :value="row.address"
          size="small"
          class="ale__row-input lb-mono"
          @update:value="(v: string) => editAt(i, v)"
        />
        <a-button size="small" type="text" class="ale__row-del" @click="removeAt(i)">删除</a-button>
      </div>
    </div>
    <div v-else class="ale__empty">还没有地址</div>

    <div class="ale__add">
      <a-input
        v-model:value="draft"
        size="small"
        :placeholder="placeholder"
        class="lb-mono"
        @press-enter="add"
      />
      <a-button size="small" @click="add">添加</a-button>
    </div>
    <div v-if="hint" class="ale__hint">{{ hint }}</div>
  </div>
</template>

<style scoped>
.ale {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ale__list {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.ale__row {
  display: flex;
  gap: 6px;
  align-items: center;
}
.ale__row-input {
  flex: 1;
}
.ale__row-del {
  color: #b4291d;
  padding: 0 6px;
}
.ale__empty {
  font-size: 12px;
  color: #8a94a6;
  padding: 2px 0;
}
.ale__add {
  display: flex;
  gap: 6px;
}
.ale__add .ant-input {
  flex: 1;
}
.ale__hint {
  font-size: 11.5px;
  line-height: 1.6;
  color: #6b7480;
}
</style>
