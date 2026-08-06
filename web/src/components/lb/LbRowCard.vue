<script setup lang="ts">
/**
 * 窄屏下表格行换成的卡片外壳。
 *
 * 只提供外框与三段插槽,内容各页自己填 —— 用户行要额度条、节点行要两种状态、
 * 审计行要夹断的详情,抽成「通用行卡片」只会得到一堆条件分支。
 *
 * 底栏是动作区:桌面上的 26px 行内按钮在手指下全部不合格,
 * 这里一律 36px 起(成组)或 44px(单独)。缩小间距可以,缩小命中区不行。
 */
defineProps<{ danger?: boolean }>()
</script>

<template>
  <section class="lb-rowcard" :class="{ 'lb-rowcard--danger': danger }">
    <div class="lb-rowcard__head"><slot name="head" /></div>
    <div v-if="$slots.default" class="lb-rowcard__body"><slot /></div>
    <div v-if="$slots.foot" class="lb-rowcard__foot"><slot name="foot" /></div>
  </section>
</template>

<style scoped>
.lb-rowcard {
  display: flex;
  flex-direction: column;
  background: #fff;
  border: 1px solid #e3e6ea;
  border-radius: 8px;
}

.lb-rowcard--danger {
  border-color: #f3cfc9;
}

.lb-rowcard__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 12px 14px 0;
}

.lb-rowcard__body {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px 14px 12px;
}

.lb-rowcard__foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  border-top: 1px solid #edeff2;
}

/* 成组按钮 36px,单独的主按钮撑满并升到 44px。 */
.lb-rowcard__foot :deep(.ant-btn) {
  min-height: 36px;
}

.lb-rowcard__foot :deep(.ant-btn:only-child) {
  flex: 1;
  min-height: 44px;
}
</style>
