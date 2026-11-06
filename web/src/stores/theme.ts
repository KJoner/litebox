import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import type { ThemeMode } from '@/theme/tokens'

const STORAGE_KEY = 'litebox.theme'

/** 用户的选择:浅色 / 深色 / 跟随系统。 */
export type ThemePreference = ThemeMode | 'system'

/**
 * 主题(UI 2.0 起三档)。
 *
 * 存的是**偏好**,生效的是 `mode`(解析后的 light / dark)。CSS 变量(<html data-theme>)
 * 与 AntD 的 ConfigProvider(App.vue)都读 `mode` —— 两边读同一个值,
 * 才不会出现 AntD 控件与页面底色一明一暗。
 *
 * 「跟随系统」要**持续**跟随:监听 prefers-color-scheme 的变化,系统在傍晚切到深色时
 * 面板跟着切。V18 只在首次进入时读一次系统设置,之后就再也不跟了。
 * localStorage 里没有值时默认深色(VNext);老版本存过的 'light' / 'dark' / 'system' 原样沿用。
 *
 * 根节点加 lb-theme-switching 类 400ms,让 background / color 有过渡;
 * 平时不挂它:一个常驻的 transition 会让每次路由切换都闪一下。
 */
function readPreference(): ThemePreference {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'light' || saved === 'dark' || saved === 'system') return saved
  } catch {
    /* 隐私模式下 localStorage 可能抛错,那就跟随系统 */
  }
  // VNext UI01:没选过就是暖暗色(Nexus 系列的默认),不再默认跟随系统。
  return 'dark'
}

const MOTION_KEY = 'litebox.motion'

/** 动效档位(VNext UI03):正常 / 减弱(只留淡入淡出)/ 关闭。根节点上的 data-motion 由 base.css 解释。 */
export type MotionLevel = 'normal' | 'reduced' | 'off'

function readMotion(): MotionLevel {
  try {
    const saved = localStorage.getItem(MOTION_KEY)
    if (saved === 'normal' || saved === 'reduced' || saved === 'off') return saved
  } catch {
    /* 同上 */
  }
  return 'normal'
}

export const useThemeStore = defineStore('theme', () => {
  const media = window.matchMedia?.('(prefers-color-scheme: dark)')
  const systemDark = ref(media?.matches ?? false)
  media?.addEventListener?.('change', (e) => {
    systemDark.value = e.matches
  })

  const preference = ref<ThemePreference>(readPreference())
  const mode = computed<ThemeMode>(() =>
    preference.value === 'system' ? (systemDark.value ? 'dark' : 'light') : preference.value,
  )

  function apply(m: ThemeMode, animate: boolean) {
    const root = document.documentElement
    if (animate) {
      root.classList.add('lb-theme-switching')
      window.setTimeout(() => root.classList.remove('lb-theme-switching'), 400)
    }
    root.setAttribute('data-theme', m)
  }

  apply(mode.value, false)
  watch(mode, (m) => apply(m, true))
  watch(preference, (p) => {
    try {
      localStorage.setItem(STORAGE_KEY, p)
    } catch {
      /* 存不下就下次再跟随系统 */
    }
  })

  function setPreference(p: ThemePreference) {
    preference.value = p
  }

  /** 在浅 / 深之间切(门户顶栏那颗按钮用)。切完就不再跟随系统。 */
  function toggle() {
    preference.value = mode.value === 'dark' ? 'light' : 'dark'
  }

  const motion = ref<MotionLevel>(readMotion())
  const applyMotion = (m: MotionLevel) => document.documentElement.setAttribute('data-motion', m)
  applyMotion(motion.value)
  watch(motion, (m) => {
    applyMotion(m)
    try {
      localStorage.setItem(MOTION_KEY, m)
    } catch {
      /* 存不下就只在本次生效 */
    }
  })
  function setMotion(m: MotionLevel) {
    motion.value = m
  }

  return { mode, preference, setPreference, toggle, motion, setMotion }
})
