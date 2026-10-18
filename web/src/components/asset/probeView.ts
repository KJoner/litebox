import type { ProbeISP, ProbeMetric, ProbeOrigin, ProbeState, ProbeStats, ProbeSummary, ProbeTCPMode } from '@/api/client'

/**
 * 探测一行的显示规则,卡片、节点详情与目标库共用 —— 几处各写一份的话,同一个目标在卡片上叫「丢包」、
 * 在详情里叫「失败率」,而管理员会以为是两个数。
 *
 * 一律按后端给的 metric 起名,不按协议自己推:
 *   - LOSS(ICMP):真实丢包率 = Σ(发 − 收) ÷ Σ发;
 *   - CONNECT(TCP 建连):连接失败率 = 失败的建连 ÷ 发起的建连;
 *   - SYN(TCP 握手近似):握手超时无应答率 = Σ(发出的 SYN − 有应答的) ÷ Σ发出的 SYN。
 *     它基于系统的 TCP 建连,不是原始套接字的 SYN 探测,**不能叫丢包率**;RST 也是应答。
 * 采集失败、工具不可用、DNS 失败、目标受限都没有发起,不进分母 —— 分母为 0 时显示「—」,不是 0%。
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
export function rateOf(s: ProbeStats, metric: ProbeMetric): number | null {
  return metric === 'CONNECT' ? s.failure_rate : s.loss_rate
}

/** 比例的短名(行内用)。 */
export function rateName(metric: ProbeMetric): string {
  switch (metric) {
    case 'LOSS':
      return '丢包'
    case 'SYN':
      return '无应答'
  }
  return '失败率'
}

/** 比例的全名。 */
export function rateFullName(metric: ProbeMetric): string {
  switch (metric) {
    case 'LOSS':
      return 'ICMP 丢包率'
    case 'SYN':
      return '握手超时无应答率'
  }
  return 'TCP 连接失败率'
}

export function rateTitle(s: ProbeStats, metric: ProbeMetric, span: string): string {
  let head: string
  switch (metric) {
    case 'LOSS':
      head = `${span}:ICMP 发 ${s.sent} 个包,收到 ${s.received} 个`
      break
    case 'SYN':
      head = `${span}:发出 ${s.sent} 个 SYN,有应答(SYN-ACK 或 RST)${s.received} 个 —— 这是握手超时无应答率,不是丢包率`
      break
    default:
      head = `${span}:TCP 连接 ${s.attempts} 次,失败 ${s.failures} 次`
  }
  const parts = [head]
  const notSent: string[] = []
  if (s.dns_errors) notSent.push(`DNS 失败 ${s.dns_errors}`)
  if (s.ssh_errors) notSent.push(`采集失败 ${s.ssh_errors}`)
  if (s.tool_errors) notSent.push(`工具不可用 ${s.tool_errors}`)
  if (s.blocked) notSent.push(`目标受限 ${s.blocked}`)
  if (notSent.length) parts.push(`未发起(不计入):${notSent.join('、')}`)
  return parts.join(';')
}

/** 方式的名字(表格、表单用)。 */
export function metricLabel(metric: ProbeMetric): string {
  switch (metric) {
    case 'LOSS':
      return 'ICMP'
    case 'SYN':
      return 'TCP 握手(近似 SYN)'
  }
  return 'TCP 建连'
}

/** 协议标签:ICMP / TCP 443 / SYN 443。 */
export function protoChip(p: { metric: ProbeMetric; resolved_port?: number; port?: number; direction?: string }): string {
  if (p.metric === 'LOSS') return 'ICMP'
  const port = p.port ?? (p.direction === 'OUTBOUND' ? p.resolved_port : 0)
  const head = p.metric === 'SYN' ? 'SYN' : 'TCP'
  return port ? `${head} ${port}` : head
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

export function ispText(isp: ProbeISP | string): string {
  return ({ CT: '电信', CU: '联通', CM: '移动', CERNET: '教育网', OTHER: '其他' } as Record<string, string>)[isp] ?? ''
}

export function originText(o: ProbeOrigin | string): string {
  return ({ MANUAL: '自定义', ZSTATIC: 'Zstatic', TCPQUALITY: 'TcpQuality' } as Record<string, string>)[o] ?? o
}

export function tcpModeOf(protocol: string, mode: ProbeTCPMode | undefined): ProbeMetric {
  if (protocol === 'ICMP') return 'LOSS'
  return mode === 'SYN' ? 'SYN' : 'CONNECT'
}
