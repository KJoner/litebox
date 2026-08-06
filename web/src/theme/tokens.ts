/**
 * LiteBox 设计 Token —— 唯一的颜色与尺寸来源。
 *
 * 不要在组件的 scoped CSS 里再写十六进制色值。原来 #cf1322 / #d46b08 / #389e0d
 * 散在四个文件里各写一遍,改一次要改四处,而且已经开始不一致了。
 */

export const color = {
  // 表面
  bgPage: '#F6F7F9',
  bgSurface: '#FFFFFF',
  bgSubtle: '#F1F3F5',
  border: '#E3E6EA',
  borderSubtle: '#EDEFF2',

  /**
   * 文字四级。三级 #6B7480 是 4.7:1(AA),任意字号可用,元数据一律用它。
   * 四级 #8A93A0 只有 3.1:1,仅限 >=24px 的大字占位(指标卡的「—」)。
   */
  text1: '#15181C',
  text2: '#576070',
  text3: '#6B7480',
  text4: '#8A93A0',
  /** 输入框占位与禁用态。不承载信息,所以不受对比度约束。 */
  placeholder: '#7C8492',
  /** 纯装饰性分隔符(面包屑的斜杠等)。 */
  divider: '#A9B1BB',

  brand: '#2563B8',
  brandHover: '#1D4F96',
  brandBg: '#EEF4FC',
  brandBorder: '#C9DCF3',

  success: '#1B7A4B',
  successBg: '#E9F5EE',
  successBorder: '#C3E3D0',

  warning: '#92610A',
  warningBg: '#FCF3E3',
  warningBorder: '#EFDCB4',

  danger: '#B4291D',
  dangerBg: '#FDECEA',
  dangerBorder: '#F3CFC9',

  /** 人为暂停:节点停发订阅。与「已禁用」是两回事。 */
  maintenance: '#5F52A0',
  maintenanceBg: '#F0EEF9',
  maintenanceBorder: '#D6D0EE',

  neutral: '#5C6672',
  neutralBg: '#F1F3F5',
  neutralBorder: '#DFE3E8',
} as const

export const radius = { control: 4, inner: 6, card: 8 } as const

export const space = [0, 4, 8, 12, 16, 20, 24, 32, 48] as const

export const font = {
  sans: "'IBM Plex Sans', 'Noto Sans SC', -apple-system, BlinkMacSystemFont, 'Segoe UI', 'PingFang SC', sans-serif",
  mono: "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace",
} as const

/** 只给浮层。卡片一律用 1px 边框,不用投影。 */
export const shadowOverlay = '0 4px 12px rgba(20, 24, 28, 0.08)'

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
  /** 超过这个行数才分页 */
  paginateOver: 50,
} as const

export function usageColor(percent: number): string {
  if (percent >= threshold.usageDanger) return color.danger
  if (percent >= threshold.usageWarn) return color.warning
  return color.success
}
