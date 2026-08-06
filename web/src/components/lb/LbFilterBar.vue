<script setup lang="ts">
/**
 * 筛选栏。它存在的理由只有一个:**LbEmptyState 要区分「首次为空」和「筛选为空」,
 * 就必须有人知道当前有没有生效的筛选。** 这个状态散在各页面的 reactive filters
 * 里,每页都要重写一遍判断。
 *
 * 它不负责过滤数据 —— 过滤仍在页面的 computed 里(用户 10 人量级,推到 SQL
 * 只会多出一层查询拼装代码)。它只持有「有几条筛选生效」并渲染清除入口。
 */
withDefaults(
  defineProps<{
    /** 生效中的筛选条数。0 表示未筛选。 */
    activeCount: number
    /** 筛选后条数 / 总条数,显示在右侧 */
    filtered?: number
    total?: number
    unit?: string
  }>(),
  { unit: '条' },
)

defineEmits<{ (e: 'clear'): void }>()
</script>

<template>
  <div class="lb-filter">
    <slot />
    <a v-if="activeCount > 0" class="lb-filter__clear" @click="$emit('clear')">清除全部筛选</a>
    <span v-if="total !== undefined" class="lb-filter__count lb-mono">
      {{ filtered ?? total }} / {{ total }} {{ unit }}
    </span>
  </div>
</template>

<style scoped>
.lb-filter {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 10px 12px;
  border-bottom: 1px solid #edeff2;
}

.lb-filter__clear {
  margin-left: 4px;
  font-size: 12.5px;
}

.lb-filter__count {
  margin-left: auto;
  font-size: 11.5px;
  color: #6b7480;
}
</style>
