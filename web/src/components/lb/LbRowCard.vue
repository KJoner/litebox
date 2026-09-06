<script setup lang="ts">
/**
 * 窄屏下表格行换成的卡片外壳。
 *
 * 只提供外框与三段插槽,内容各页自己填 —— 用户行要额度条、节点行要两种状态、
 * 审计行要夹断的详情,抽成「通用行卡片」只会得到一堆条件分支。
 *
 * 底栏是动作区:桌面上的 28px 行内按钮在手指下全部不合格,
 * 这里一律 36px 起(成组)或 44px(单独)。缩小间距可以,缩小命中区不行。
 *
 * V18:20px 圆角 + --shadow,不再有左侧色条 —— 危险态靠头部的状态标签说话,
 * 外框只加一圈极淡的 --bad-bg 描边。
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
  background: var(--surface);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
}

.lb-rowcard--danger {
  box-shadow: var(--shadow), 0 0 0 1px var(--bad-bg);
}

.lb-rowcard__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 16px 18px 0;
  font-size: 14px;
  font-weight: 600;
}

.lb-rowcard__body {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px 18px 14px;
  font-size: 13px;
}

.lb-rowcard__foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 18px 16px;
  border-top: 1px solid var(--sep2);
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
