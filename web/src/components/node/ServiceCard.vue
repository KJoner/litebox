<script setup lang="ts">
import { LbInfoTip, LbStatusTag, type LbStatusMeta } from '@/components/lb'

/**
 * 「入口」Tab 上的一张服务卡片:sing-box / Mieru / nginx / realm 各一张。
 *
 * 三层结构是固定的,四张卡片一模一样,管理员看一眼就知道每一颗按钮在哪:
 *
 *   名称 ⓘ ………………… [状态]      ← 状态只认 serviceState.ts 算出来的那一个
 *   版本 · 一句现状
 *   ┌──────── 安装 / 启动 / 停止 ────────┐   ← 随状态变化;在跑时是「停止」+「重启」两半
 *   [下发配置]   [检查]   [卸载]            ← 三颗固定
 *
 * 这个组件只管长相,不管做什么:每颗按钮的动作、确认档次与"为什么现在
 * 不能点"都由 NodeEntriesPanel 给 —— 那里才知道这台机器上有几个入口、
 * 停掉会断多少人。按钮不能点时把原因放进 title,而不是把按钮藏掉:
 * 四张卡片少一颗按钮,管理员会以为那是别的一种服务。
 */
export interface ServiceCardAction {
  label: string
  onClick: () => void
  /** 非空表示现在不能点,这句话就是原因(进 title) */
  disabled?: string
  danger?: boolean
  primary?: boolean
  /** 有菜单时渲染成下拉按钮:主体点 onClick,箭头展开这些项 */
  menu?: { label: string; onClick: () => void }[]
}

export interface ServiceCardModel {
  key: string
  title: string
  /** ⓘ 里的帮助文案:这类服务的下发摩擦、停止的代价 */
  tip: string
  status: LbStatusMeta
  /** 状态下面那一行:版本与一句现状 */
  detail: string
  /** 大按钮,一到两颗,平分一行 */
  primary: ServiceCardAction[]
  /** 底排,固定三颗 */
  actions: ServiceCardAction[]
}

defineProps<{ card: ServiceCardModel; busy: boolean }>()
</script>

<template>
  <section class="sc" :data-service="card.key">
    <div class="sc__head">
      <span class="sc__title">
        {{ card.title }}
        <LbInfoTip :text="card.tip" :width="300" />
      </span>
      <LbStatusTag :meta="card.status" />
    </div>
    <div class="sc__detail" :title="card.detail">{{ card.detail || ' ' }}</div>

    <div class="sc__primary">
      <template v-for="a in card.primary" :key="a.label">
        <a-dropdown-button
          v-if="a.menu"
          :type="a.primary ? 'primary' : 'default'"
          :danger="a.danger"
          :disabled="busy || !!a.disabled"
          :title="a.disabled"
          class="sc__big"
          @click="a.onClick"
        >
          {{ a.label }}
          <template #overlay>
            <a-menu>
              <a-menu-item v-for="m in a.menu" :key="m.label" @click="m.onClick">
                {{ m.label }}
              </a-menu-item>
            </a-menu>
          </template>
        </a-dropdown-button>
        <a-button
          v-else
          :type="a.primary ? 'primary' : 'default'"
          :danger="a.danger"
          :disabled="busy || !!a.disabled"
          :title="a.disabled"
          class="sc__big"
          @click="a.onClick"
        >
          {{ a.label }}
        </a-button>
      </template>
    </div>

    <div class="sc__actions">
      <a-button
        v-for="a in card.actions"
        :key="a.label"
        size="small"
        :danger="a.danger"
        :disabled="busy || !!a.disabled"
        :title="a.disabled"
        @click="a.onClick"
      >
        {{ a.label }}
      </a-button>
    </div>
  </section>
</template>

<style scoped>
.sc {
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
  padding: 14px 16px 16px;
  background: var(--surface);
  border-radius: var(--r-group);
  box-shadow: var(--shadow);
}

.sc__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.sc__title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 15px;
  font-weight: 600;
  white-space: nowrap;
}

/* 版本与现状:一行,超出省略 —— init 的原话可以很长,完整的放 title 里。 */
.sc__detail {
  font-size: 12px;
  line-height: 1.5;
  color: var(--text3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 大按钮:一颗撑满,两颗平分。40 高比行内按钮高一档 ——
   它是这张卡片上最重的一个动作,要与底排那三颗分得开。 */
.sc__primary {
  display: flex;
  gap: 8px;
}
.sc__primary > :deep(.ant-btn),
.sc__primary > :deep(.ant-dropdown-button) {
  flex: 1;
  min-width: 0;
}
.sc__primary :deep(.ant-btn) {
  height: 40px;
  font-size: 14px;
}
/* 下拉按钮是两颗拼起来的,主体那颗要撑满剩余宽度。 */
.sc__primary :deep(.ant-dropdown-button > .ant-btn:first-child) {
  flex: 1;
}

/* 底排三颗等宽。字号压到 12 是因为四张卡并排时每张只有 260 左右,
   「下发配置」四个字在 13px 下会把第三颗挤到下一行。 */
.sc__actions {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 6px;
}
.sc__actions :deep(.ant-btn) {
  padding-inline: 4px;
  font-size: 12px;
}
</style>
