<script setup lang="ts">
/**
 * 表格卡顶部的工具条。它存在的理由只有一个:**LbEmptyState 要区分「首次为空」和
 * 「筛选为空」,就必须有人知道当前有没有生效的筛选。** 这个状态散在各页面的
 * reactive filters 里,每页都要重写一遍判断。
 *
 * 它不负责过滤数据 —— 过滤仍在页面的 computed 里(用户 10 人量级,推到 SQL
 * 只会多出一层查询拼装代码)。它只持有「有几条筛选生效」并渲染清除入口。
 *
 * V18:搜索框 240×32 --fill 底圆角 10;下拉是 32px --fill chip;布尔筛选用
 * iOS 开关(a-switch size="small")+ 文字,不再用 checkbox。这些都是对
 * 插槽里 AntD 控件的 :deep 覆盖,页面不用改控件类型,换个容器就生效。
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
    <span v-if="total !== undefined" class="lb-filter__count lb-tabular">
      {{ filtered ?? total }} / {{ total }} {{ unit }}
    </span>
  </div>
</template>

<style scoped>
.lb-filter {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  padding: 14px 20px;
  border-bottom: 1px solid var(--sep2);
}

.lb-filter__clear {
  font-size: 12.5px;
  font-weight: 500;
}

.lb-filter__count {
  margin-left: auto;
  font-size: 12.5px;
  color: var(--text3);
}

/* 搜索框 / 下拉 chip / 开关的规格在 styles/base.css 的 .lb-filter 全局规则里:
   有几页直接用 <div class="lb-filter"> 拼工具条,scoped 的 :deep 管不到它们。 */
@media (max-width: 767px) {
  .lb-filter {
    padding: 12px 14px;
  }
}
</style>
