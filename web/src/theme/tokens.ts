/**
 * LiteBox 设计 Token 的 TS 侧(UI 2.0)。唯一来源是 styles/tokens.css,这里只放两样:
 *
 *   palette   浅 / 深两组颜色【实值】,只给 theme/antd.ts。ConfigProvider 要按实值
 *             推导派生色(hover、disabled、边框浅一档……),认不得 var()。
 *             键名与 tokens.css 的变量一一对应,改一边必须同步另一边。
 *   color     组件里用的颜色,全是 **CSS 变量引用**(`var(--ok)`),
 *             跟着 <html data-theme> 一起切换,组件不必知道当前是哪个主题。
 *             SVG 的 fill / stroke 属性同样收 var() —— 呈现属性按 CSS 值解析。
 *
 * scoped CSS 里一律直接写 `var(--xxx)`。`color.*` 的键名保持与 V3 一致,
 * 老调用点(`color.successBg`、`color.text3`)不用改。
 */

export type ThemeMode = 'light' | 'dark'

export interface Palette {
  bg: string
  surface: string
  surface2: string
  surface3: string
  fill: string
  fill2: string
  sep: string
  sep2: string
  track: string
  text: string
  text2: string
  text3: string
  tooltipBg: string
  textInverse: string
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
  overlay: string
  shadowXs: string
  shadow: string
  shadowHover: string
  shadowLg: string
}

const light: Palette = {
  bg: '#F8FAFC',
  surface: '#FFFFFF',
  surface2: '#F8FAFC',
  surface3: '#F1F5F9',
  fill: 'rgba(15,23,42,.05)',
  fill2: 'rgba(15,23,42,.09)',
  sep: '#E2E8F0',
  sep2: '#EEF2F6',
  track: 'rgba(15,23,42,.07)',
  text: '#0F172A',
  text2: '#475569',
  text3: '#64748B',
  tooltipBg: '#0F172A',
  textInverse: '#F8FAFC',
  brand: '#2563B8',
  brandHover: '#1D4F96',
  brandBg: '#EEF4FC',
  ok: '#1B7A4B',
  okBg: 'rgba(27,122,75,.10)',
  warn: '#92610A',
  warnBg: 'rgba(180,120,10,.11)',
  bad: '#B4291D',
  badBg: 'rgba(180,41,29,.09)',
  purple: '#5F52A0',
  purpleBg: 'rgba(95,82,160,.10)',
  overlay: 'rgba(15,23,42,.45)',
  shadowXs: '0 1px 2px rgba(15,23,42,.04)',
  shadow: '0 1px 2px rgba(15,23,42,.04), 0 1px 3px rgba(15,23,42,.03)',
  shadowHover: '0 6px 16px -4px rgba(15,23,42,.10), 0 2px 4px -2px rgba(15,23,42,.05)',
  shadowLg: '0 16px 40px -12px rgba(15,23,42,.22), 0 4px 12px -4px rgba(15,23,42,.08)',
}

const dark: Palette = {
  bg: '#0B0F14',
  surface: '#121923',
  surface2: '#161F2B',
  surface3: '#1B2533',
  fill: 'rgba(148,163,184,.10)',
  fill2: 'rgba(148,163,184,.16)',
  sep: '#263241',
  sep2: '#1C2633',
  track: 'rgba(148,163,184,.14)',
  text: '#F1F5F9',
  text2: '#CBD5E1',
  text3: '#94A3B8',
  tooltipBg: '#E2E8F0',
  textInverse: '#0F172A',
  brand: '#5B9AF0',
  brandHover: '#7DB0F5',
  brandBg: 'rgba(91,154,240,.14)',
  ok: '#4CC38A',
  okBg: 'rgba(76,195,138,.13)',
  warn: '#E0A526',
  warnBg: 'rgba(224,165,38,.13)',
  bad: '#F0665C',
  badBg: 'rgba(240,102,92,.13)',
  purple: '#B3A6F0',
  purpleBg: 'rgba(179,166,240,.13)',
  overlay: 'rgba(2,6,12,.62)',
  shadowXs: '0 1px 2px rgba(0,0,0,.3)',
  shadow: '0 1px 2px rgba(0,0,0,.35)',
  shadowHover: '0 8px 20px -6px rgba(0,0,0,.6)',
  shadowLg: '0 18px 48px -12px rgba(0,0,0,.75), 0 0 0 1px rgba(148,163,184,.08)',
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
  /** 纯装饰性分隔符(面包屑的 / 等)。 */
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

/** 圆角阶梯(px),与 tokens.css 的 --r-* 一致。只给要在 TS 里拿数的地方(AntD 主题)。 */
export const radius = {
  xs: 4,
  sm: 6,
  btn: 8,
  input: 8,
  tile: 8,
  seg: 8,
  group: 10,
  card: 12,
  sheet: 14,
  pill: 999,
} as const

/** 字号阶梯(px),与 tokens.css 的 --fs-* 一致。 */
export const fontSize = {
  '2xs': 11,
  xs: 12,
  sm: 13,
  base: 14,
  md: 15,
  lg: 16,
  xl: 18,
  '2xl': 24,
  metric: 28,
} as const

export const space = [0, 4, 8, 12, 16, 20, 24, 32, 48] as const

/** 控件高度,与 tokens.css 的 --h-control* 一致。 */
export const controlHeight = { sm: 28, base: 36, lg: 40 } as const

/** 系统字体栈。中文回落到自托管的 Noto Sans SC(Windows / Linux 上没有 PingFang)。 */
export const font = {
  sans: "-apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', 'Helvetica Neue', 'Noto Sans SC', 'Microsoft YaHei', sans-serif",
  mono: "'SF Mono', ui-monospace, 'JetBrains Mono', Menlo, Consolas, 'Cascadia Mono', monospace",
} as const

/** 卡片以细边框为主、阴影为辅;浮层重。 */
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
