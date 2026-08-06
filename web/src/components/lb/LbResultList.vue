<script setup lang="ts">
import { computed } from 'vue'

/**
 * 批量操作的逐条结果。
 *
 * 现有实现是 failed.map(...).join('\n') 塞进 Modal.warning 的 content。
 * 字符串里放不下「重试这一条」的按钮,而部分失败时管理员最需要的恰恰是这个。
 *
 * 同一个组件也用于「执行中」的逐条进度:pending 项显示虚线环。
 */
export interface LbResultItem {
  id: number | string
  name: string
  /** undefined = 还没执行到 */
  ok?: boolean
  /** 成功时可放一句结果摘要,失败时放原因 */
  detail?: string
}

const props = defineProps<{ items: LbResultItem[]; retryable?: boolean }>()
defineEmits<{ (e: 'retry', item: LbResultItem): void }>()

const done = computed(() => props.items.filter((i) => i.ok !== undefined).length)
const failed = computed(() => props.items.filter((i) => i.ok === false).length)
const running = computed(() => done.value < props.items.length)

defineExpose({ done, failed, running })
</script>

<template>
  <div class="lb-result">
    <div v-if="running" class="lb-result__bar">
      <div class="lb-result__fill" :style="{ width: (done / items.length) * 100 + '%' }" />
    </div>

    <div class="lb-result__table">
      <div class="lb-result__head">
        <span>对象</span><span>结果</span><span>说明</span><span />
      </div>
      <div
        v-for="it in items"
        :key="it.id"
        class="lb-result__row"
        :class="{ 'lb-result__row--bad': it.ok === false }"
      >
        <span class="lb-ellipsis">{{ it.name }}</span>
        <span>
          <span v-if="it.ok === undefined" class="lb-result__pending">等待</span>
          <span v-else-if="it.ok" class="lb-result__ok">成功</span>
          <span v-else class="lb-result__bad">失败</span>
        </span>
        <span class="lb-result__detail lb-clamp-2">{{ it.detail ?? '—' }}</span>
        <span>
          <a v-if="retryable && it.ok === false" class="lb-result__retry" @click="$emit('retry', it)">
            重试
          </a>
        </span>
      </div>
    </div>

    <!-- 已成功的不会回滚:批量调整不是事务。这句话必须写出来。 -->
    <div v-if="!running && failed > 0" class="lb-result__foot">
      {{ items.length }} 个中 {{ items.length - failed }} 个成功。已成功的不会回滚 —— 批量操作不是事务。
    </div>
  </div>
</template>

<style scoped>
.lb-result {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.lb-result__bar {
  height: 5px;
  background: #edeff2;
  border-radius: 3px;
  overflow: hidden;
}

.lb-result__fill {
  height: 5px;
  background: #2563b8;
  transition: width 0.2s;
}

.lb-result__table {
  border: 1px solid #edeff2;
  border-radius: 6px;
  overflow: hidden;
}

.lb-result__head,
.lb-result__row {
  display: grid;
  grid-template-columns: 1.3fr 0.7fr 1.6fr 0.6fr;
  align-items: center;
  gap: 8px;
  padding: 8px 11px;
  font-size: 12px;
}

.lb-result__head {
  background: #f6f7f9;
  border-bottom: 1px solid #edeff2;
  font-size: 11px;
  font-weight: 600;
  color: #576070;
}

.lb-result__row + .lb-result__row {
  border-top: 1px solid #edeff2;
}

.lb-result__row--bad {
  background: #fefafa;
}

.lb-result__ok {
  color: #1b7a4b;
}

.lb-result__bad {
  color: #b4291d;
}

.lb-result__pending,
.lb-result__detail {
  color: #6b7480;
}

.lb-result__retry {
  font-size: 11.5px;
}

.lb-result__foot {
  font-size: 12px;
  line-height: 1.7;
  color: #5c4405;
  background: #fcf3e3;
  border: 1px solid #efdcb4;
  border-radius: 6px;
  padding: 10px 12px;
}
</style>
