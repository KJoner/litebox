const GIB = 1024 ** 3
const TIB = 1024 ** 4

export type LbQuotaUnit = 'GB' | 'TB'

/**
 * 额度以「数值 + 单位」两个控件输入,提交时换算成字节。
 * 与节点表单对齐 —— 原来用户表单只有 GB,节点表单有 GB/TB,
 * 500 GB 与 4 TB 摆在一起看是两种语言。
 */
export function toBytes(value: number | null, unit: LbQuotaUnit): number {
  if (!value || value <= 0) return 0
  return Math.round(value * (unit === 'TB' ? TIB : GIB))
}

/** 回填:整数 TB 才用 TB 显示,否则用 GB,避免 0.49 TB 这种不好读的小数。 */
export function fromBytes(bytes: number): { value: number | null; unit: LbQuotaUnit } {
  if (!bytes || bytes <= 0) return { value: null, unit: 'GB' }
  if (bytes % TIB === 0) return { value: bytes / TIB, unit: 'TB' }
  return { value: Math.round((bytes / GIB) * 100) / 100, unit: 'GB' }
}
