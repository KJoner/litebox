// 展示层的格式化工具。

/** 把字节数格式化为可读大小。0 视为"不限"由调用方自行判断。 */
export function formatBytes(bytes: number | null | undefined): string {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  const value = bytes / 1024 ** i
  // 小数位随量级递减:1023 B 不需要小数,1.25 GB 需要。
  const digits = i === 0 ? 0 : value >= 100 ? 1 : 2
  return `${value.toFixed(digits)} ${units[i]}`
}

/** 额度为 0 表示不限量。 */
export function formatQuota(bytes: number): string {
  return bytes > 0 ? formatBytes(bytes) : '不限'
}

/** 把 UTC 的 RFC3339 时间转成本地时区的可读串。 */
export function formatTime(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** 只保留日期部分。 */
export function formatDate(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/**
 * 按 UTC 显示日期时间。
 *
 * 节点额度的周期边界统一取 UTC 00:00。用本地时区渲染会让 UTC+8 的管理员
 * 看到"9 月 15 日 08:00 重置",而他填的是 15 日 —— 更糟的是重置日填 31 时
 * 本地会显示成下个月 1 日,看起来像设置根本没生效。
 */
export function formatUTCTime(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`
}

/** 只保留 UTC 日期部分,用于列表里的"8 月 15 日重置"。 */
export function formatUTCDay(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return `${d.getUTCMonth() + 1} 月 ${d.getUTCDate()} 日`
}

/** 相对时间,用于"最后同步 3 分钟前"这类展示。 */
export function formatRelative(value: string | null | undefined): string {
  if (!value) return '从未'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const seconds = Math.floor((Date.now() - d.getTime()) / 1000)
  if (seconds < 0) return formatTime(value)
  if (seconds < 60) return `${seconds} 秒前`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  if (seconds < 30 * 86400) return `${Math.floor(seconds / 86400)} 天前`
  return formatTime(value)
}

/** 剩余天数;已过期返回负数。 */
export function daysUntil(value: string | null | undefined): number | null {
  if (!value) return null
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return null
  return Math.ceil((d.getTime() - Date.now()) / 86400000)
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

/** 截断哈希用于展示,完整值放 title。 */
export function shortHash(hash: string | null | undefined): string {
  if (!hash) return '—'
  return hash.length > 12 ? hash.slice(0, 12) : hash
}
