/**
 * LiteBox 设计 Token —— 唯一的颜色与尺寸来源(V18 iOS 风格改版)。
 *
 * 两份东西,分别给两个消费者:
 *
 *   palette   浅 / 深两组【实值】,只给 theme/antd.ts。ConfigProvider 要按实值
 *             推导派生色(hover、disabled、边框浅一档……),认不得 var()。
 *   color     组件里用的颜色,全是 **CSS 变量引用**(`var(--ok)`),
 *             跟着 <html data-theme> 一起切换,组件不必知道当前是哪个主题。
 *             SVG 的 fill / stroke 属性同样收 var() —— 呈现属性按 CSS 值解析。
 *
 * 不要在组件的 scoped CSS 里再写十六进制色值:scoped CSS 里一律直接写
 * `var(--xxx)`(与 styles/tokens.css 同名)。这里的键名保持与 V3 一致,
 * 老调用点(`color.successBg`、`color.text3`)不用改;删掉的是 `text4`
 * (三级之外不再有第四级)与全部 `*Border`(语义标签不再有边框)。
 */

export type ThemeMode = 'light' | 'dark'

export interface Palette {
  bg: string
  surface: string
  surface2: string
  fill: string
  fill2: string
  sep: string
  sep2: string
  glass: string
  track: string
  text: string
  text2: string
  text3: string
  brand: string
  brandHover: string
  brandBg: string
  ok: string
  okBg: string
  warn: string
  warnBg: string
  bad: string
  badBg: string
  purple: string
  purpleBg: string
  shadow: string
  shadowHover: string
  shadowLg: string
  shadowBrand: string
}

const light: Palette = {
  bg: '#F5F5F7',
  surface: '#FFFFFF',
  surface2: '#F5F5F7',
  fill: 'rgba(0,0,0,.05)',
  fill2: 'rgba(0,0,0,.09)',
  sep: 'rgba(0,0,0,.09)',
  sep2: 'rgba(0,0,0,.05)',
  glass: 'rgba(255,255,255,.78)',
  track: 'rgba(0,0,0,.07)',
  text: '#1D1D1F',
  text2: '#5F5F66',
  text3: '#86868B',
  brand: '#2563B8',
  brandHover: '#1D4F96',
  brandBg: '#EEF4FC',
  ok: '#1B7A4B',
  okBg: 'rgba(27,122,75,.12)',
  warn: '#92610A',
  warnBg: 'rgba(146,97,10,.12)',
  bad: '#B4291D',
  badBg: 'rgba(180,41,29,.12)',
  purple: '#5F52A0',
  purpleBg: 'rgba(95,82,160,.12)',
  shadow: '0 1px 2px rgba(0,0,0,.04), 0 8px 24px rgba(0,0,0,.05)',
  shadowHover: '0 2px 4px rgba(0,0,0,.05), 0 14px 36px rgba(0,0,0,.09)',
  shadowLg: '0 12px 40px rgba(0,0,0,.16)',
  shadowBrand: '0 4px 12px rgba(37,99,184,.25)',
}

const dark: Palette = {
  bg: '#000000',
  surface: '#1C1C1E',
  surface2: '#2C2C2E',
  fill: 'rgba(255,255,255,.08)',
  fill2: 'rgba(255,255,255,.14)',
  sep: 'rgba(255,255,255,.12)',
  sep2: 'rgba(255,255,255,.07)',
  glass: 'rgba(28,28,30,.78)',
  track: 'rgba(255,255,255,.1)',
  text: '#F5F5F7',
  text2: '#A1A1A6',
  text3: '#8E8E93',
  brand: '#5B9AF0',
  brandHover: '#7DB0F5',
  brandBg: 'rgba(91,154,240,.18)',
  ok: '#4CC38A',
  okBg: 'rgba(76,195,138,.18)',
  warn: '#E0A526',
  warnBg: 'rgba(224,165,38,.18)',
  bad: '#F0665C',
  badBg: 'rgba(240,102,92,.18)',
  purple: '#B3A6F0',
  purpleBg: 'rgba(179,166,240,.18)',
  shadow: '0 1px 2px rgba(0,0,0,.5), 0 8px 24px rgba(0,0,0,.4)',
  shadowHover: '0 2px 4px rgba(0,0,0,.5), 0 14px 36px rgba(0,0,0,.6)',
  shadowLg: '0 12px 48px rgba(0,0,0,.7)',
  shadowBrand: '0 4px 12px rgba(91,154,240,.3)',
}

/** 浅 / 深两组实值。键名与 styles/tokens.css 的变量一一对应。 */
export const palette: Record<ThemeMode, Palette> = { light, dark }

export const color = {
  // 表面
  bgPage: 'var(--bg)',
  bgSurface: 'var(--surface)',
  bgSubtle: 'var(--surface2)',
  fill: 'var(--fill)',
  fill2: 'var(--fill2)',
  border: 'var(--sep)',
  borderSubtle: 'var(--sep2)',
  track: 'var(--track)',
  glass: 'var(--glass)',

  /** 文字三级。元数据一律用三级。 */
  text1: 'var(--text)',
  text2: 'var(--text2)',
  text3: 'var(--text3)',
  /** 输入框占位与禁用态。 */
  placeholder: 'var(--text3)',
  /** 纯装饰性分隔符(面包屑的 › 等)。 */
  divider: 'var(--text3)',

  brand: 'var(--brand)',
  brandHover: 'var(--brand-hover)',
  brandBg: 'var(--brand-bg)',

  success: 'var(--ok)',
  successBg: 'var(--ok-bg)',

  warning: 'var(--warn)',
  warningBg: 'var(--warn-bg)',

  danger: 'var(--bad)',
  dangerBg: 'var(--bad-bg)',

  /** 人为暂停:节点停发订阅。与「已禁用」是两回事。 */
  maintenance: 'var(--purple)',
  maintenanceBg: 'var(--purple-bg)',

  neutral: 'var(--neutral)',
  neutralBg: 'var(--neutral-bg)',
} as const

/** 圆角阶梯:输入框 10 / 分组列表 14 / 卡片 20 / 弹窗 22 / 胶囊 999。 */
export const radius = { input: 10, group: 14, card: 20, sheet: 22, pill: 999, tile: 12, seg: 9 } as const

export const space = [0, 4, 8, 12, 16, 20, 24, 32, 48] as const

/** 系统字体栈。中文回落到自托管的 Noto Sans SC(Windows / Linux 上没有 PingFang)。 */
export const font = {
  sans: "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'SF Pro Display', 'PingFang SC', 'Helvetica Neue', 'Noto Sans SC', sans-serif",
  mono: "'SF Mono', ui-monospace, Menlo, Consolas, 'Cascadia Mono', monospace",
} as const

/** 卡片极浅,浮层重。卡片不再有 1px 边框。 */
export const shadow = 'var(--shadow)'
export const shadowHover = 'var(--shadow-hover)'
export const shadowLg = 'var(--shadow-lg)'
export const shadowBrand = 'var(--shadow-brand)'

export const ease = 'cubic-bezier(.2,.8,.2,1)'
export const easeSheet = 'cubic-bezier(.32,.72,0,1)'

/**
 * 阈值集中在这里,但**只放前端自己算的那些**。
 * 额度告警等级取后端 warning_level,不在前端重算 —— 边界只能有一份定义。
 */
export const threshold = {
  /** 采样超过这个时长算过期。取采集周期(5 分钟)的两倍。 */
  metricsStaleMs: 10 * 60 * 1000,
  /** 资源使用率着色。128MB 的小机器内存本来就贴着高位走,定低了天天报警。 */
  usageWarn: 70,
  usageDanger: 90,
  /** 到期预警天数 */
  expiringSoonDays: 7,
  /** 用户额度接近上限的比例 */
  nearQuotaRatio: 0.8,
} as const

export function usageColor(percent: number): string {
  if (percent >= threshold.usageDanger) return color.danger
  if (percent >= threshold.usageWarn) return color.warning
  return color.success
}
