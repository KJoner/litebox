import type { ProbeProtocol, ProbeState, ProbeStats, ProbeSummary } from '@/api/client'

/**
 * 探测一行的显示规则,卡片与节点详情共用 —— 两处各写一份的话,同一个目标在卡片上叫「丢包」、
 * 在详情里叫「失败率」,而管理员会以为是两个数。
 *
 * ICMP 看丢包率(按包:Σ(发 − 收) ÷ Σ发),TCP 看连接失败率(按建连)。采集失败、工具不可用、
 * DNS 失败、目标受限都没有发起,不进分母 —— 分母为 0 时显示「—」,不是 0%。
 */

/** 状态的色调。采集失败不按坏画:那是面板连不上机器,目标那头怎样并不知道。 */
export function probeTone(state: ProbeState): 'bad' | 'warn' | 'muted' | '' {
  switch (state) {
    case 'DOWN':
      return 'bad'
    case 'FAILING':
    case 'LOSSY':
    case 'TOOL_ERROR':
    case 'BLOCKED':
      return 'warn'
    case 'OK':
      return ''
    default:
      return 'muted'
  }
}

/** 整行降灰:暂停、停用、关机、失效、过期时,历史点阵仍然画,但不再暗示「此刻」。 */
export function probeMuted(state: ProbeState): boolean {
  return state === 'PAUSED' || state === 'INVALID' || state === 'DISABLED' || state === 'STALE' || state === 'STOPPED'
}

/** 一行右侧的数字:最近一次可达时是 RTT,否则是结果名。 */
export function probeValue(p: ProbeSummary): string {
  const l = p.last
  if (!l) return '—'
  if ((l.status === 'OK' || l.status === 'LOSS') && l.rtt_ms != null) return rttText(l.rtt_ms)
  return l.label
}

/** 这一行看哪个比例。 */
export function rateOf(s: ProbeStats, protocol: ProbeProtocol): number | null {
  return protocol === 'ICMP' ? s.loss_rate : s.failure_rate
}

export function rateName(protocol: ProbeProtocol): string {
  return protocol === 'ICMP' ? '丢包' : '失败率'
}

export function rateTitle(s: ProbeStats, protocol: ProbeProtocol, span: string): string {
  const parts =
    protocol === 'ICMP'
      ? [`${span}:ICMP 发 ${s.sent} 个包,收到 ${s.received} 个`]
      : [`${span}:TCP 连接 ${s.attempts} 次,失败 ${s.failures} 次`]
  const notSent: string[] = []
  if (s.dns_errors) notSent.push(`DNS 失败 ${s.dns_errors}`)
  if (s.ssh_errors) notSent.push(`采集失败 ${s.ssh_errors}`)
  if (s.tool_errors) notSent.push(`工具不可用 ${s.tool_errors}`)
  if (s.blocked) notSent.push(`目标受限 ${s.blocked}`)
  if (notSent.length) parts.push(`未发起(不计入):${notSent.join('、')}`)
  return parts.join(';')
}

/** 协议标签:ICMP / TCP 443。 */
export function protoChip(p: Pick<ProbeSummary, 'protocol' | 'resolved_port' | 'direction'>): string {
  if (p.protocol === 'ICMP') return 'ICMP'
  return p.direction === 'OUTBOUND' && p.resolved_port ? `TCP ${p.resolved_port}` : 'TCP'
}

export function targetTitle(p: ProbeSummary): string {
  const where = p.protocol === 'ICMP' ? p.resolved_host : `${p.resolved_host}:${p.resolved_port}`
  const peer = p.last?.peer && p.last.peer !== p.resolved_host ? `,最近一次连的是 ${p.last.peer}` : ''
  return `${p.label}(${where}${peer})`
}

/** 比例的文字:分母为 0 时是「—」,不是 0%。 */
export function rateText(rate: number | null | undefined): string {
  if (rate == null) return '—'
  const v = rate * 100
  if (v === 0) return '0%'
  return v < 1 ? `${v.toFixed(1)}%` : `${Math.round(v)}%`
}

export function rttText(ms: number | null | undefined): string {
  if (ms == null) return '—'
  return ms < 10 ? `${ms.toFixed(1)} ms` : `${Math.round(ms)} ms`
}

export function familyText(f: string): string {
  return f === 'V6' ? 'IPv6' : 'IPv4'
}
