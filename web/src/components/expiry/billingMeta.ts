import type { BillingCycle, BillingPayload, BillingView, ExpiryRenewMethod } from '@/api/client'

/**
 * 套餐计费表单的取值规则,只写在这一处。金额一律是十进制字符串 —— 不经浮点,
 * 后端按币种的小数位解析成最小单位;多一位小数直接报错,不悄悄四舍五入。
 */

export const cycleOptions: { value: BillingCycle; label: string }[] = [
  { value: '', label: '未设置' },
  { value: 'MONTHLY', label: '月付' },
  { value: 'QUARTERLY', label: '季付' },
  { value: 'SEMIANNUAL', label: '半年付' },
  { value: 'YEARLY', label: '年付' },
  { value: 'CUSTOM', label: '自定义周期' },
  { value: 'ONE_TIME', label: '一次性' },
  { value: 'TRAFFIC_PACK', label: '流量包' },
  { value: 'PERMANENT', label: '永久 / 无到期' },
]

/** 常见币种。只是输入提示 —— 任何三位字母代码都收。 */
export const commonCurrencies = ['CNY', 'USD', 'HKD', 'EUR', 'GBP', 'JPY', 'SGD', 'TWD', 'KRW', 'CAD', 'AUD']

export interface BillingFormValue {
  plan_name: string
  billing_cycle: BillingCycle
  cycle_count: number | null
  cycle_unit: '' | 'MONTH' | 'DAY'
  currency: string
  price: string
  purchased_at: string
  period_start: string
  period_paid: string
  traffic_unlimited: boolean
}

export const blankBillingForm = (): BillingFormValue => ({
  plan_name: '',
  billing_cycle: '',
  cycle_count: null,
  cycle_unit: '',
  currency: '',
  price: '',
  purchased_at: '',
  period_start: '',
  period_paid: '',
  traffic_unlimited: false,
})

export function billingFormOf(b: BillingView | null | undefined): BillingFormValue {
  if (!b) return blankBillingForm()
  return {
    plan_name: b.plan_name,
    billing_cycle: b.billing_cycle,
    cycle_count: b.billing_cycle === 'CUSTOM' ? b.cycle_count : null,
    cycle_unit: b.billing_cycle === 'CUSTOM' ? b.cycle_unit : '',
    currency: b.currency,
    price: b.price,
    purchased_at: b.purchased_at,
    period_start: b.period_start,
    period_paid: b.period_paid,
    traffic_unlimited: b.traffic_unlimited,
  }
}

export function billingPayload(f: BillingFormValue): BillingPayload {
  const custom = f.billing_cycle === 'CUSTOM'
  return {
    plan_name: f.plan_name.trim(),
    billing_cycle: f.billing_cycle,
    cycle_count: custom ? Number(f.cycle_count ?? 0) : 0,
    cycle_unit: custom ? f.cycle_unit || 'MONTH' : '',
    currency: f.currency.trim().toUpperCase(),
    price: f.price.trim(),
    purchased_at: f.purchased_at,
    period_start: f.period_start,
    period_paid: f.period_paid.trim(),
    traffic_unlimited: f.traffic_unlimited,
  }
}

/**
 * 「续一期」对应的续费方式。只有固定周期的套餐有;按日历月 / 年推进,不是 30 / 365 天。
 */
export function oneCycleRenewal(b: BillingView | null | undefined): { method: ExpiryRenewMethod; count: number; label: string } | null {
  if (!b) return null
  switch (b.billing_cycle) {
    case 'MONTHLY':
      return { method: 'MONTHS', count: 1, label: '续一期(月付)' }
    case 'QUARTERLY':
      return { method: 'MONTHS', count: 3, label: '续一期(季付)' }
    case 'SEMIANNUAL':
      return { method: 'MONTHS', count: 6, label: '续一期(半年付)' }
    case 'YEARLY':
      return { method: 'YEARS', count: 1, label: '续一期(年付)' }
    case 'CUSTOM':
      if (b.cycle_count <= 0) return null
      return b.cycle_unit === 'DAY'
        ? { method: 'DAYS', count: b.cycle_count, label: `续一期(${b.cycle_count} 天)` }
        : { method: 'MONTHS', count: b.cycle_count, label: `续一期(${b.cycle_count} 个月)` }
  }
  return null
}
