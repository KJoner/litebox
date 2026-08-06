<script setup lang="ts">
/**
 * 选中后的批量操作栏,吸附在表头上方。
 *
 * 「当前筛选外的 N 个不在其中」这句话是必须的:表头复选框是「全选当前筛选结果」
 * 而不是全表,批量停用漏掉或多带上几个人,代价完全不对等。
 *
 * 切换筛选时页面应当清空选择 —— 调用方监听筛选变化后 emit clear。
 */
withDefaults(
  defineProps<{
    selectedCount: number
    /** 筛选后的总数,用于算出「筛选外还有几个」 */
    filteredTotal?: number
    total?: number
    unit?: string
  }>(),
  { unit: '个' },
)

defineEmits<{ (e: 'clear'): void }>()
</script>

<template>
  <div v-if="selectedCount > 0" class="lb-batch">
    <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
      <rect x="0.5" y="0.5" width="11" height="11" rx="2" fill="#2563B8" />
      <path d="M3 6.2 5 8.2 9 3.8" fill="none" stroke="#fff" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
    </svg>
    <span class="lb-batch__count">已选 {{ selectedCount }} {{ unit }}</span>
    <span
      v-if="total !== undefined && filteredTotal !== undefined && total > filteredTotal"
      class="lb-batch__note"
    >
      当前筛选外的 {{ total - filteredTotal }} {{ unit }}不在其中
    </span>
    <div class="lb-batch__actions">
      <slot />
      <a-button type="link" size="small" @click="$emit('clear')">取消选择</a-button>
    </div>
  </div>
</template>

<style scoped>
.lb-batch {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 12px;
  background: #eef4fc;
  border-bottom: 1px solid #edeff2;
}

.lb-batch__count {
  font-size: 12.5px;
  font-weight: 500;
  color: #1d4f96;
}

.lb-batch__note {
  font-size: 11.5px;
  color: #4a7bbe;
}

.lb-batch__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-left: auto;
}
</style>
