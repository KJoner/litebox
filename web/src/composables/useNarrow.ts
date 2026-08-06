import { onMounted, onUnmounted, ref } from 'vue'

/**
 * 是否处于窄屏(<768)。
 *
 * 后台的四张长表在这个断点下要整体换成卡片列表,而不是让 AntD Table 横向滚动 ——
 * 横向滚动会把最右边的「操作」列推到屏幕外,手机上找不到它。
 * 断点值与 styles 里的 @media (max-width: 767px) 保持一致,改要一起改。
 */
export function useNarrow() {
  const narrow = ref(false)
  let mql: MediaQueryList | null = null
  const sync = () => (narrow.value = mql!.matches)

  onMounted(() => {
    mql = window.matchMedia('(max-width: 767px)')
    sync()
    mql.addEventListener('change', sync)
  })
  onUnmounted(() => mql?.removeEventListener('change', sync))

  return narrow
}
