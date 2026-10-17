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
 * UI 2.0:12px 圆角 + 细边框,不再有左侧色条 —— 危险态靠头部的状态标签说话,
 * 外框描边换成 --bad-border。
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
  border: 1px solid var(--sep);
  border-radius: var(--r-card);
  box-shadow: var(--shadow);
  min-width: 0;
}

.lb-rowcard--danger {
  border-color: var(--bad-border);
}

.lb-rowcard__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 14px var(--sp-4) 0;
  font-size: var(--fs-base);
  font-weight: 600;
}

.lb-rowcard__body {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px var(--sp-4) 14px;
  font-size: var(--fs-sm);
}

.lb-rowcard__foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px var(--sp-4) 14px;
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
