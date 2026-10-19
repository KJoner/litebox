import type { BoceRunState, BoceScheduleSpec, BoceTaskInput, BoceVantage } from '@/api/client'
import { metaOf, type LbStatusMeta } from '@/components/lb'

/**
 * Boce 外部入站拨测在前端的几条显示规则,设置页、节点详情、批量应用共用。
 *
 * 数字一律由后端算(轮数、波点、费用、可运行天数都走 /api/boce/estimate),这里只负责把它们说清楚 ——
 * 前端自己数时间槽的话,页面上写每天 16 轮,而引擎跑了 17 轮,多出来的那一轮正好是 24:00。
 */

const WEEKDAY_NAMES = ['一', '二', '三', '四', '五', '六', '日']

export function weekdaysText(mask: number): string {
  if (!mask || mask === 127) return '每天'
  if (mask === 0b0011111) return '工作日'
  if (mask === 0b1100000) return '周末'
  return '周' + WEEKDAY_NAMES.filter((_, i) => mask & (1 << i)).join('')
}

export function scheduleText(s: BoceScheduleSpec): string {
  if (s.schedule_mode === 'TIMES') return `${weekdaysText(s.weekdays)} ${s.times.join('、')}`
  return `${weekdaysText(s.weekdays)} ${s.window_start}–${s.window_end} 每 ${s.interval_min} 分钟`
}

export function runStateMeta(state: BoceRunState | string): LbStatusMeta {
  switch (state) {
    case 'DONE':
      return metaOf.ok('已完成')
    case 'POLLING':
    case 'CREATING':
      return metaOf.mute(state === 'POLLING' ? '等待结果' : '正在创建', 'ring')
    case 'TIMEOUT':
      return metaOf.warn('超时(部分结果)')
    case 'EXPIRED':
      return metaOf.warn('结果已失效')
    case 'UNKNOWN':
      return metaOf.warn('结果不确定', 'triangle')
    case 'FAILED':
      return metaOf.bad('创建失败(未扣费)')
    case 'SKIPPED':
      return metaOf.paused('未执行')
    case 'INVALID':
      return metaOf.bad('目标已失效', 'triangle')
  }
  return metaOf.mute(state || '还没有执行', 'ring')
}

export function yuan(v: number | null | undefined): string {
  if (v == null) return '—'
  if (v === 0) return '¥0'
  return v < 0.01 ? `¥${v.toFixed(4)}` : `¥${v.toFixed(2)}`
}

export function points(v: number | null | undefined): string {
  if (v == null) return '—'
  return `${Number.isInteger(v) ? v : v.toFixed(1)} 波点`
}

/** 拨测点给人看的名字:节点名 + 运营商(节点名里已经带了运营商就不重复);只用接口原样给的说法。 */
export function vantageLabel(v: Pick<BoceVantage, 'node_name' | 'isp_name'> & { area?: string }): string {
  // 海外拨测点的运营商一律是「海外」(大区另有标记),名字里不重复。
  if (!v.isp_name || v.node_name.includes(v.isp_name) || (v.area === 'OVERSEA' && v.isp_name === '海外')) return v.node_name
  return `${v.node_name} ${v.isp_name}`
}

/** 三网快捷筛选:按接口返回的运营商名字里有没有这两个字认,认不出的归「其他」。不臆造运营商。 */
export const ISP_QUICK = [
  { key: 'CT', label: '电信', match: (s: string) => s.includes('电信') },
  { key: 'CU', label: '联通', match: (s: string) => s.includes('联通') },
  { key: 'CM', label: '移动', match: (s: string) => s.includes('移动') },
] as const

export function ispGroup(name: string): string {
  for (const q of ISP_QUICK) if (q.match(name)) return q.key
  return 'OTHER'
}

/** 幂等的请求 ID:一个弹窗 / 一次确认生成一个,重复点击与刷新重发只算一次。 */
export function newRequestID(): string {
  const c = globalThis.crypto
  if (c && 'randomUUID' in c) return c.randomUUID().replace(/-/g, '')
  return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 12)}`
}

export function emptyTaskInput(spec: BoceScheduleSpec): BoceTaskInput {
  return {
    name: '',
    ref: undefined,
    vantage_ids: [],
    month_points_limit: 0,
    enabled: false,
    note: '',
    // 不用 structuredClone:传进来的常常是响应式代理,克隆它会直接抛 DataCloneError。
    ...specOf(spec),
  }
}

export function specOf(t: BoceScheduleSpec): BoceScheduleSpec {
  return {
    schedule_mode: t.schedule_mode,
    interval_min: t.interval_min,
    times: [...(t.times ?? [])],
    window_start: t.window_start,
    window_end: t.window_end,
    weekdays: t.weekdays,
    timezone: t.timezone,
  }
}

export const COMMON_TIMEZONES = [
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Taipei',
  'Asia/Tokyo',
  'Asia/Singapore',
  'UTC',
  'Europe/London',
  'America/Los_Angeles',
]
