<script setup lang="ts">
import LbInfoTip from './LbInfoTip.vue'

/**
 * 卡片外置的分组标题:左 19px 700 标题(+ 可选 ⓘ),右侧动作链接或 12.5px 计数。
 *
 * 列表 / 表格类卡片的标题不放卡片内 —— 卡片上方 `padding: 0 4px 12px`,
 * 与设计稿「全部节点 · 5 / 5 台」那一行一致。
 */
withDefaults(
  defineProps<{
    title: string
    /** ⓘ 里的文案。 */
    tip?: string
    /** 右侧计数,如「12 / 12 人」;与 #extra 二选一。 */
    count?: string
    /** 生效方式徽标(设置页)。 */
    effect?: 'now' | 'deploy' | 'sessions' | 'readonly'
    /** 用 h3 而不是 h2(卡内子分组)。 */
    level?: 2 | 3
  }>(),
  { level: 2 },
)

const effectLabel = {
  now: '改完立即生效',
  deploy: '改动会触发重新部署',
  sessions: '会撤销其他会话',
  readonly: '只读',
}
</script>

<template>
  <div class="lb-section" :class="{ 'lb-section--sub': level === 3 }">
    <component :is="level === 3 ? 'h3' : 'h2'" class="lb-section__title">
      <span>{{ title }}</span>
      <LbInfoTip v-if="tip" :text="tip" />
      <slot name="tip" />
      <span v-if="effect" class="lb-effect" :class="`lb-effect--${effect}`">{{ effectLabel[effect] }}</span>
      <slot name="badge" />
    </component>
    <div v-if="count || $slots.extra" class="lb-section__extra">
      <slot name="extra">
        <span class="lb-section__count lb-tabular">{{ count }}</span>
      </slot>
    </div>
  </div>
</template>

<style scoped>
.lb-section {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  padding: 0 4px 12px;
  flex-wrap: wrap;
}
.lb-section__title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-size: 19px;
  font-weight: 700;
  letter-spacing: -0.02em;
  line-height: 1.3;
  min-width: 0;
}
.lb-section--sub .lb-section__title {
  font-size: 14px;
  font-weight: 600;
  letter-spacing: 0;
}
.lb-section--sub {
  padding: 0 0 10px;
}
.lb-section__extra {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 13px;
  font-weight: 500;
}
.lb-section__count {
  font-size: 12.5px;
  font-weight: 400;
  color: var(--text3);
}
</style>
