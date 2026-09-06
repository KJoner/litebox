import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import type { ThemeMode } from '@/theme/tokens'

const STORAGE_KEY = 'litebox.theme'

/**
 * 浅色 / 深色。UI 层唯一的新状态。
 *
 * 初始值:localStorage 里存过就用它,没存过跟随 prefers-color-scheme。
 * 切换写到 <html data-theme> 上 —— CSS 变量在 tokens.css 里按它切,
 * AntD 那一侧由 App.vue 读同一个值换 ConfigProvider 的 theme。
 * 根节点加 lb-theme-switching 类 400ms,让 background / color 有过渡;
 * 平时不挂它:一个常驻的 transition 会让每次路由切换都闪一下。
 */
function initial(): ThemeMode {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'light' || saved === 'dark') return saved
  } catch {
    /* 隐私模式下 localStorage 可能抛错,那就跟随系统 */
  }
  return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

export const useThemeStore = defineStore('theme', () => {
  const mode = ref<ThemeMode>(initial())

  function apply(m: ThemeMode, animate: boolean) {
    const root = document.documentElement
    if (animate) {
      root.classList.add('lb-theme-switching')
      window.setTimeout(() => root.classList.remove('lb-theme-switching'), 400)
    }
    root.setAttribute('data-theme', m)
  }

  apply(mode.value, false)
  watch(mode, (m) => {
    apply(m, true)
    try {
      localStorage.setItem(STORAGE_KEY, m)
    } catch {
      /* 存不下就下次再跟随系统 */
    }
  })

  function toggle() {
    mode.value = mode.value === 'dark' ? 'light' : 'dark'
  }

  return { mode, toggle }
})
