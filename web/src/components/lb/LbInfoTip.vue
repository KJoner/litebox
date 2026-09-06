<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

/**
 * 感叹号 Tooltip。V18 起每页的帮助文案、脚注、副标题一律收进这里,
 * 不再散落在页面上 —— 这是改版里唯一涉及「内容位置」的改动。
 *
 * 行为:悬停显示;点击钉住;再点一次、或点页面任意空白处取消。
 * **钉住是全局单例**:钉住一个之后,悬停别的 ⓘ 不切换 —— 管理员正在读的那段话
 * 不该因为鼠标滑过旁边的图标就消失。
 *
 * 气泡用 Teleport 到 body 并按 fixed 定位:指标条、表格卡、弹窗内容区都是
 * overflow:hidden,气泡留在原地会被裁掉一半;而弹窗与抽屉自带 transform 动画,
 * 动画期间会把 fixed 元素的参照系抢过去,只有挪到 body 下才稳。
 */
const props = withDefaults(
  defineProps<{
    /** 文案。也可用默认插槽放带链接的内容。 */
    text?: string
    /** 气泡宽度,240–340。 */
    width?: number
    /** 凭据类字段用实心警告图标。 */
    warn?: boolean
    /** 图标尺寸,默认 16。 */
    size?: number
    /** 无障碍名称,默认「说明」。 */
    label?: string
  }>(),
  { width: 280, warn: false, size: 16, label: '说明' },
)

// 全局钉住状态。放在模块级而不是 store:它不参与任何业务逻辑,也不需要持久化。
const pinned = ref<symbol | null>(null)
let listening = false
function onDocClick() {
  pinned.value = null
}
function ensureListener() {
  if (listening) return
  document.addEventListener('click', onDocClick)
  listening = true
}

const id = Symbol('lb-tip')
const hover = ref(false)
const root = ref<HTMLElement | null>(null)
const bubble = ref<HTMLElement | null>(null)

const open = computed(() => pinned.value === id || (pinned.value === null && hover.value))
const isPinned = computed(() => pinned.value === id)

const pos = ref<{ top?: string; bottom?: string; left?: string; right?: string }>({})

/** 按图标位置算气泡落点,靠右或靠底时翻转,免得裁到视口外。 */
function place() {
  const el = root.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const w = props.width
  const next: typeof pos.value = {}
  const spaceBelow = window.innerHeight - r.bottom
  const bubbleH = bubble.value?.offsetHeight ?? 120
  if (spaceBelow < bubbleH + 16 && r.top > bubbleH + 16) {
    next.bottom = `${window.innerHeight - r.top + 6}px`
  } else {
    next.top = `${r.bottom + 6}px`
  }
  if (r.left - 10 + w > window.innerWidth - 12) {
    next.right = `${Math.max(12, window.innerWidth - r.right - 10)}px`
  } else {
    next.left = `${Math.max(12, r.left - 10)}px`
  }
  pos.value = next
}

watch(open, async (v) => {
  if (!v) return
  await nextTick()
  place()
})

function toggle(e: MouseEvent) {
  e.stopPropagation()
  pinned.value = isPinned.value ? null : id
}

function onScroll() {
  if (open.value) place()
}

onMounted(() => {
  ensureListener()
  window.addEventListener('scroll', onScroll, true)
  window.addEventListener('resize', onScroll)
})
onBeforeUnmount(() => {
  if (isPinned.value) pinned.value = null
  window.removeEventListener('scroll', onScroll, true)
  window.removeEventListener('resize', onScroll)
})
</script>

<template>
  <span
    ref="root"
    class="lb-tip"
    :class="{ 'lb-tip--on': open, 'lb-tip--warn': props.warn }"
    role="button"
    tabindex="0"
    :aria-label="props.label"
    :aria-expanded="open"
    @mouseenter="hover = true"
    @mouseleave="hover = false"
    @click="toggle"
    @keydown.enter.prevent="pinned = isPinned ? null : id"
    @keydown.escape="pinned = null"
  >
    <svg v-if="props.warn" :width="props.size" :height="props.size" viewBox="0 0 16 16" aria-hidden="true">
      <circle cx="8" cy="8" r="7.2" fill="var(--warn)" />
      <path d="M8 4.4v4.2" stroke="#fff" stroke-width="1.6" stroke-linecap="round" />
      <circle cx="8" cy="11.4" r="0.9" fill="#fff" />
    </svg>
    <svg v-else :width="props.size" :height="props.size" viewBox="0 0 16 16" aria-hidden="true">
      <circle cx="8" cy="8" r="6.6" fill="none" stroke="currentColor" stroke-width="1.4" />
      <path d="M8 7.2v4" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
      <circle cx="8" cy="5" r="0.85" fill="currentColor" />
    </svg>

    <Teleport to="body">
      <div
        v-if="open"
        ref="bubble"
        class="lb-tip__bubble"
        :style="{ ...pos, width: props.width + 'px' }"
        role="tooltip"
        @click.stop
      >
        <slot>{{ props.text }}</slot>
      </div>
    </Teleport>
  </span>
</template>

<style scoped>
.lb-tip {
  position: relative;
  display: inline-flex;
  align-items: center;
  color: var(--text3);
  cursor: help;
  vertical-align: middle;
  line-height: 0;
  border-radius: 50%;
  transition: color 0.15s;
}
.lb-tip:hover,
.lb-tip--on {
  color: var(--text);
}
.lb-tip--warn,
.lb-tip--warn:hover {
  color: var(--warn);
}
</style>

<style>
/* 气泡 Teleport 到 body,scoped 管不到它。 */
.lb-tip__bubble {
  position: fixed;
  z-index: 1080;
  padding: 12px 14px;
  background: var(--surface);
  border: 1px solid var(--sep);
  border-radius: var(--r-group);
  box-shadow: var(--shadow-lg);
  font-size: 12.5px;
  line-height: 1.65;
  color: var(--text2);
  font-weight: 400;
  text-align: left;
  white-space: normal;
  animation: lb-pop 0.18s var(--ease);
  max-width: calc(100vw - 24px);
}
.lb-tip__bubble p {
  margin: 0 0 6px;
}
.lb-tip__bubble p:last-child {
  margin-bottom: 0;
}
.lb-tip__bubble b {
  color: var(--text);
  font-weight: 600;
}
.lb-tip__bubble code {
  font-family: var(--mono);
  font-size: 12px;
  padding: 0 4px;
  border-radius: 4px;
  background: var(--fill);
}
</style>
