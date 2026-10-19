import type { ProbeISP, ProbeMetric, ProbeOrigin, ProbeState, ProbeStats, ProbeSummary, ProbeTCPMode } from '@/api/client'

/**
 * 探测一行的显示规则,卡片、节点详情与目标库共用 —— 几处各写一份的话,同一个目标在卡片上叫「丢包」、
 * 在详情里叫「失败率」,而管理员会以为是两个数。
 *
 * 一律按后端给的 metric 起名,不按协议自己推:
 *   - LOSS(ICMP):真实丢包率 = Σ(发 − 收) ÷ Σ发;
 *   - CONNECT(TCP 建连):连接失败率 = 失败的建连 ÷ 发起的建连;
 *   - SYN(TCP 握手近似):握手超时无应答率 = Σ(发出的 SYN − 有应答的) ÷ Σ发出的 SYN。
 *     它基于系统的 TCP 建连,不是原始套接字的 SYN 探测,**不能叫丢包率**;RST 也是应答;
 *   - BOCE(Boce 拨测点的 TCPing):TCPing 丢包率 = Σ(发 − 收) ÷ Σ发,按 Boce 报告的包数。
 *     第三方拨测点从外面测的,与 ICMP 丢包率、主控的 TCP 连接失败率名字不同、不相加;
 *   - BOCE_PING(Boce 拨测点的 Ping):Ping 丢包率,同样按 Boce 报告的包数。它与节点本机测公共目标的
 *     ICMP 丢包率(LOSS)方向相反、测点不同,与 Boce TCPing 也不是一回事 —— 很多机器本来就不回 ICMP,
 *     ping 不通不等于入口不可用;
 *     拨测点异常、接口异常、未取得结果都不进分母,也不按失败画;
 *   - HTTP(出站 HTTP(S)):HTTP 请求失败率 = 没取得有效 HTTP 响应的次数 ÷ 实际发出的请求数。
 *     200、404、5xx 都是取得了响应,不算失败;5xx 另外标出来(服务返回异常状态,不冒充正常)。
 *     耗时叫「HTTP 响应耗时」,不叫 RTT。
 * 采集失败、工具不可用、DNS 失败、目标受限都没有发起,不进分母 —— 分母为 0 时显示「—」,不是 0%。
 */

/** 是不是 Boce 拨测点测的(TCPing 或 Ping)。几处只对第三方测点成立的说法(未执行、拨测点异常)看它。 */
export function isBoceMetric(m: ProbeMetric | string): boolean {
  return m === 'BOCE' || m === 'BOCE_PING'
}

/** 状态的色调。采集失败不按坏画:那是面板连不上机器,目标那头怎样并不知道。 */
export function probeTone(state: ProbeState): 'bad' | 'warn' | 'muted' | '' {
  switch (state) {
    case 'DOWN':
      return 'bad'
    case 'FAILING':
    case 'LOSSY':
    case 'TOOL_ERROR':
    case 'BLOCKED':
    case 'HTTP_5XX':
      return 'warn'
    case 'OK':
      return ''
    default:
      return 'muted'
  }
}

/** 整行降灰:暂停、停用、关机、失效、过期时,历史点阵仍然画,但不再暗示「此刻」。 */
export function probeMuted(state: ProbeState): boolean {
  return (
    state === 'PAUSED' || state === 'INVALID' || state === 'DISABLED' || state === 'STALE' || state === 'STOPPED' ||
    state === 'SKIPPED'
  )
}

/** 一行右侧的数字:最近一次可达时是 RTT(HTTP 是响应耗时),否则是结果名。 */
export function probeValue(p: ProbeSummary): string {
  const l = p.last
  if (!l) return '—'
  if ((l.status === 'OK' || l.status === 'LOSS') && l.rtt_ms != null) return rttText(l.rtt_ms)
  return l.label
}

/** HTTP 最近一次的状态码(没取得响应、或不是 HTTP 序列时为空串)。 */
export function httpCodeText(p: { metric: ProbeMetric; last?: { http_status?: number } | null }): string {
  const code = p.last?.http_status
  return p.metric === 'HTTP' && code ? `HTTP ${code}` : ''
}

/** 自建节点拨测(PEER)的一行名字:来源机器 · 测法。 */
export function peerSourceText(p: ProbeSummary): string {
  return p.peer_source ? `${p.peer_source.name} · ${p.peer_source.method}` : '自建节点'
}

/** 一条序列的原始名称(别名旁边要能看到它):PEER 是「来源 · 测法」,Boce 是拨测点,其余是登记的名称。 */
export function seriesName(p: ProbeSummary): string {
  if (p.source === 'PEER') return peerSourceText(p)
  return p.label
}

/** 卡片上显示的名字:有别名用别名,没有用原始名称。别名只在展示层,不改检测。 */
export function displayName(p: ProbeSummary, aliases?: Record<string, string> | null): string {
  const a = aliases?.[String(p.id)]
  return a && a.trim() ? a : seriesName(p)
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
    case 'BOCE':
      return 'TCPing 丢包'
    case 'BOCE_PING':
      return 'Ping 丢包'
    case 'HTTP':
      return '请求失败'
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
    case 'BOCE':
      return 'Boce TCPing 丢包率'
    case 'BOCE_PING':
      return 'Boce Ping 丢包率'
    case 'HTTP':
      return 'HTTP 请求失败率'
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
    case 'BOCE':
      head = `${span}:Boce 拨测点 TCPing ${s.attempts} 轮,发 ${s.sent} 次、成功 ${s.received} 次 —— 这是 Boce 的 TCPing 丢包率,不是 ICMP 丢包率`
      break
    case 'BOCE_PING':
      head = `${span}:Boce 拨测点 Ping ${s.attempts} 轮,发 ${s.sent} 个包、收到 ${s.received} 个 —— 这是 Boce 拨测点的 Ping 丢包率,不是节点本机的 ICMP 丢包率;很多机器本来就不回 ICMP`
      break
    case 'HTTP':
      head = `${span}:HTTP 请求 ${s.attempts} 次,取得响应 ${s.received} 次(${httpCodesText(s)})—— 404、5xx 都算取得了响应,不算请求失败`
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
  if (s.vantage_errors) notSent.push(`拨测点异常 ${s.vantage_errors}`)
  if (s.api_errors) notSent.push(`接口异常 ${s.api_errors}`)
  if (s.expired) notSent.push(`未取得结果 ${s.expired}`)
  if (s.skipped && isBoceMetric(metric)) notSent.push(`未执行 ${s.skipped}`)
  if (notSent.length) parts.push(`未发起(不计入):${notSent.join('、')}`)
  return parts.join(';')
}

/** HTTP 取得的响应按状态码分类的说法。 */
export function httpCodesText(s: { http_2xx: number; http_3xx: number; http_4xx: number; http_5xx: number }): string {
  return `2xx ${s.http_2xx} · 3xx ${s.http_3xx} · 4xx ${s.http_4xx} · 5xx ${s.http_5xx}`
}

/** 方式的名字(表格、表单用)。 */
export function metricLabel(metric: ProbeMetric): string {
  switch (metric) {
    case 'LOSS':
      return 'ICMP'
    case 'SYN':
      return 'TCP 握手(近似 SYN)'
    case 'BOCE':
      return 'Boce TCPing'
    case 'BOCE_PING':
      return 'Boce Ping'
    case 'HTTP':
      return 'HTTP(S)'
  }
  return 'TCP 建连'
}

/** 协议标签:ICMP / TCP 443 / SYN 443。 */
export function protoChip(p: { metric: ProbeMetric; resolved_port?: number; port?: number; direction?: string }): string {
  if (p.metric === 'LOSS') return 'ICMP'
  const port = p.port ?? (p.direction === 'OUTBOUND' ? p.resolved_port : 0)
  if (p.metric === 'BOCE') return 'TCPing'
  if (p.metric === 'BOCE_PING') return 'Ping'
  if (p.metric === 'HTTP') return 'HTTP'
  const head = p.metric === 'SYN' ? 'SYN' : 'TCP'
  return port ? `${head} ${port}` : head
}

export function targetTitle(p: ProbeSummary): string {
  if (p.source === 'PEER') {
    // Ping 只测地址;TCPing 测地址与端口。来源机器在别处,不是主控。
    const where = p.protocol === 'ICMP' ? p.resolved_host : `${p.resolved_host}:${p.resolved_port}`
    const peer = p.last?.peer && p.last.peer !== p.resolved_host ? `,最近一次连的是 ${p.last.peer}` : ''
    return `自建节点「${p.peer_source?.name ?? p.peer_node_id}」${p.peer_source?.method ?? ''} → ${p.label}(${where}${peer})`
  }
  if (p.source === 'BOCE') {
    const peer = p.last?.peer ? `,拨测点解析到 ${p.last.peer}` : ''
    // Ping 只测地址,端口不参与(序列里记的是 0)。
    const where = p.metric === 'BOCE_PING' ? p.resolved_host : `${p.resolved_host}:${p.resolved_port}`
    return `Boce 拨测点「${p.boce?.vantage_name ?? p.label}」${p.metric === 'BOCE_PING' ? ' Ping' : ''} → ${where}${peer}`
  }
  const where = p.protocol === 'ICMP' ? p.resolved_host : `${p.resolved_host}:${p.resolved_port}`
  const peer = p.last?.peer && p.last.peer !== p.resolved_host ? `,最近一次连的是 ${p.last.peer}` : ''
  return `${p.label}(${where}${peer})`
}

/**
 * HTTP 目标在列表里的写法:只写到路径,查询串换成「?…」(常带 token、签名;截图会被转发)。
 * 完整地址只在编辑表单里出现。与后端 netprobe.RedactURL 同一个规则。
 */
export function redactUrl(raw: string): string {
  try {
    const u = new URL(raw.trim())
    const tail = u.search || u.hash || raw.includes('?') ? '?…' : ''
    return `${u.protocol}//${u.host}${u.pathname}${tail}`
  } catch {
    return raw ? '(无法解析的 URL)' : ''
  }
}

/** 公共目标的地址写法:ICMP 是主机,TCP 是主机:端口,HTTP 是去掉查询串的 URL。 */
export function publicAddr(p: { protocol: string; host: string; port: number; url?: string }): string {
  if (p.protocol === 'HTTP') return redactUrl(p.url ?? '')
  return p.protocol === 'TCP' ? `${p.host}:${p.port}` : p.host
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
  if (protocol === 'HTTP') return 'HTTP'
  return mode === 'SYN' ? 'SYN' : 'CONNECT'
}
