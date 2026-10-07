import type { ExpiryKind, ExpiryView } from '@/api/client'
import { metaOf, type LbStatusMeta } from '@/components/lb/statusMeta'
import { formatTime } from '@/utils/format'

/**
 * 供应商到期的展示规则,只写在这一处。
 *
 * 四种状态都要显示出来 —— 「未设置」不是「永不过期」:商家那边没有永不过期这回事,
 * 空只说明还没登记;「已到期」显示成「待确认续费结果」而不是「已过期」:
 * 面板只是没拿到新的到期时间,不知道商家那边续没续。
 */
export function expiryStatusMeta(v: ExpiryView | null | undefined): LbStatusMeta {
  if (!v || v.state === 'UNSET') return metaOf.mute('未设置', 'ring')
  switch (v.state) {
    case 'OVERDUE':
      return metaOf.bad('已到期 · 待确认')
    case 'SOON':
      return metaOf.warn(v.days_left != null ? `${Math.max(v.days_left, 0)} 天后到期` : '即将到期')
    default:
      return metaOf.ok(v.days_left != null ? `${v.days_left} 天` : '正常', 'check')
  }
}

/** 列表里那一格的文字:日期 + 状态。 */
export function expiryText(v: ExpiryView | null | undefined): string {
  if (!v || !v.expires_at) return '未设置'
  return formatTime(v.expires_at)
}

export const expiryKindLabel: Record<ExpiryKind, string> = {
  NODE: '自建节点',
  EXTERNAL_PROXY: '外部代理',
  PROXY_SOURCE: '代理源',
}

/** 编辑表单里的到期字段(expires_at 为 RFC3339 UTC;lead_days_text 逗号分隔,空 = 继承)。 */
export interface ExpiryFormValue {
  expires_at: string
  reminder_enabled: boolean
  auto_renew: boolean
  vendor_name: string
  vendor_url: string
  note: string
  lead_days_text: string
}

export const blankExpiryForm = (): ExpiryFormValue => ({
  expires_at: '',
  reminder_enabled: true,
  auto_renew: false,
  vendor_name: '',
  vendor_url: '',
  note: '',
  lead_days_text: '',
})

/** 把逗号分隔的提前天数转成数组;非数字由后端拒绝。 */
export function parseLeadDaysText(text: string): number[] {
  return text
    .split(/[,，;\s]+/)
    .filter(Boolean)
    .map((x: string) => Number(x))
}

/** 列表筛选用的四档。 */
export type ExpiryFilter = 'ALL' | 'SOON' | 'OVERDUE' | 'AUTO_RENEW' | 'UNSET'

export function matchExpiryFilter(v: ExpiryView | null | undefined, f: ExpiryFilter): boolean {
  switch (f) {
    case 'SOON':
      return v?.state === 'SOON'
    case 'OVERDUE':
      return v?.state === 'OVERDUE'
    case 'AUTO_RENEW':
      return !!v?.auto_renew && !!v.expires_at
    case 'UNSET':
      return !v || v.state === 'UNSET'
    default:
      return true
  }
}

export const expiryFilterOptions: { value: ExpiryFilter; label: string }[] = [
  { value: 'ALL', label: '全部到期状态' },
  { value: 'SOON', label: '即将到期' },
  { value: 'OVERDUE', label: '已到期' },
  { value: 'AUTO_RENEW', label: '商家自动续费' },
  { value: 'UNSET', label: '未设置到期时间' },
]

/** RFC3339(UTC)→ 浏览器本地时间的 datetime-local 输入值。 */
export function toLocalInput(value: string): string {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** datetime-local 输入值 → RFC3339 UTC;空串原样返回。 */
export function fromLocalInput(value: string): string {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

/** 生成一次续费请求的幂等键。 */
export function newRequestID(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}
