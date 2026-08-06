<script setup lang="ts">
import { ref } from 'vue'
import { message } from 'ant-design-vue'

/**
 * 只读可复制字段:订阅地址、UUID、面板公钥。
 *
 * 剪贴板降级路径是重点。非 HTTPS 或浏览器不支持时 clipboard API 不可用,
 * 不能只说「复制失败」就完 —— 用户还是需要拿到这段内容。所以:
 *   1. 提示他手动复制,并给出快捷键;
 *   2. **真的把文本选中**,让下一个动作就能完成。
 */
const props = withDefaults(
  defineProps<{
    value: string
    label?: string
    /** 提示语,例如「等同于密码,勿转发」 */
    caution?: string
    /** hash / token 用中段省略:7f3a…c91d 还能人工比对,7f3a2b1c… 不能。 */
    middleEllipsis?: boolean
    buttonText?: string
    /** 主按钮样式 —— 门户的推荐格式用 primary,其余用默认 */
    primary?: boolean
  }>(),
  { buttonText: '复制', primary: false },
)

const box = ref<HTMLElement | null>(null)
const copied = ref(false)

/**
 * 中段省略。只省略最后一个「/」之后的那一段 ——
 * 订阅地址要的是「前缀完整 + token 缩短」(https://box.example.com/sub/a41e…7b02),
 * 把前缀一起吃掉会得到 https://127…bnAfuI,连是哪台面板都看不出来。
 * UUID 这类没有斜杠的串则整体中段省略,与 7f3a…c91d 一致。
 */
function shown(v: string): string {
  if (!props.middleEllipsis) return v
  const cut = v.lastIndexOf('/') + 1
  const head = v.slice(0, cut)
  const tail = v.slice(cut)
  if (tail.length <= 24) return v
  return head + tail.slice(0, 10) + '…' + tail.slice(-6)
}

function selectAll() {
  const el = box.value
  if (!el) return
  const range = document.createRange()
  range.selectNodeContents(el)
  const sel = window.getSelection()
  sel?.removeAllRanges()
  sel?.addRange(range)
}

async function copy() {
  try {
    await navigator.clipboard.writeText(props.value)
    // 就地变绿 1.5s。不弹吐司 —— 反馈应该出现在动作发生的地方。
    copied.value = true
    setTimeout(() => (copied.value = false), 1500)
  } catch {
    selectAll()
    const key = /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘C' : 'Ctrl+C'
    message.warning(`浏览器不允许自动复制。内容已选中,请按 ${key}`)
  }
}
</script>

<template>
  <div class="lb-copy">
    <div v-if="props.label || props.caution" class="lb-copy__head">
      <span v-if="props.label" class="lb-copy__label">{{ props.label }}</span>
      <span v-if="props.caution" class="lb-copy__caution">{{ props.caution }}</span>
    </div>
    <div class="lb-copy__row">
      <div ref="box" class="lb-copy__box lb-mono" :title="props.value">{{ shown(props.value) }}</div>
      <a-button
        :type="props.primary ? 'primary' : 'default'"
        class="lb-copy__btn"
        :class="{ 'lb-copy__btn--ok': copied }"
        :aria-label="`复制${props.label ?? ''}`"
        @click="copy"
      >
        {{ copied ? '已复制' : props.buttonText }}
      </a-button>
    </div>
  </div>
</template>

<style scoped>
.lb-copy {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
}

.lb-copy__head {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.lb-copy__label {
  font-size: 13px;
  font-weight: 500;
}

.lb-copy__caution {
  font-size: 11.5px;
  color: #92610a;
}

.lb-copy__row {
  display: flex;
  gap: 8px;
  min-width: 0;
}

.lb-copy__box {
  flex: 1;
  min-width: 0;
  height: 30px;
  display: flex;
  align-items: center;
  padding: 0 10px;
  background: #f6f7f9;
  border: 1px solid #edeff2;
  border-radius: 4px;
  font-size: 11.5px;
  color: #576070;
  /* 不折行:地址折成三行会把主按钮推到屏幕外。 */
  overflow: hidden;
  white-space: nowrap;
  user-select: all;
}

.lb-copy__btn {
  flex: none;
}

.lb-copy__btn--ok {
  color: #14603b !important;
  background: #e9f5ee !important;
  border-color: #c3e3d0 !important;
}
</style>
