import type { TopoEdge, TopoEndpoint, TopoEntry } from '@/api/client'

/**
 * 画布与统一目标选择器共用的说法(v23.3)。两处对「这是什么入口、连哪个地址、出口在哪」
 * 必须一个说法 —— 入口管理、选择器、画布各写一份的话,迟早出现一处叫「转发」、一处叫「中转」、
 * 一处把公网地址写成「出口 IP」。
 */

export const KIND_LABEL: Record<string, string> = {
  SINGBOX: 'sing-box',
  MIERU: 'Mieru',
  NGINX: 'nginx 转发',
  REALM: 'realm 转发',
  EXTERNAL: '外部代理',
  ADDRESS: '指定地址',
}

const PROTOCOL_LABEL: Record<string, string> = {
  VLESS_REALITY: 'VLESS',
  SHADOWSOCKS: 'Shadowsocks',
  SNELL: 'Snell',
  TROJAN: 'Trojan',
  MIERU: 'Mieru',
  VMESS: 'VMess',
  VLESS: 'VLESS',
  HYSTERIA2: 'Hysteria2',
  TUIC: 'TUIC',
}

export function kindLabel(kind: string): string {
  return KIND_LABEL[kind] ?? kind
}

export function protocolLabel(p?: string): string {
  if (!p) return ''
  return PROTOCOL_LABEL[p] ?? p
}

/** 主机:端口。IPv6 字面量加方括号(库里存无方括号的形式);Mieru 的端口段完整写出,不截成首个端口。 */
export function hostPortText(address: string, port: number, portEnd?: number): string {
  const host = address.includes(':') ? `[${address}]` : address
  if (!port) return host
  if (portEnd && portEnd > port) return `${host}:${port}-${portEnd}`
  return `${host}:${port}`
}

export function endpointText(ep: TopoEndpoint): string {
  return hostPortText(ep.address, ep.port, ep.port_end)
}

export function familyText(f: string): string {
  return f === 'V6' ? 'IPv6' : 'IPv4'
}

export const ADDR_MODE_TEXT: Record<string, string> = {
  ENDPOINT: '选定的地址条目',
  DEFAULT: '默认地址(管理地址 + 跟随端口)',
  LEGACY: '待确认:升级前留下的兼容地址',
}

/** 卡片上的去向标记。只是配置推导,不是实测的出口 IP。 */
export function egressText(e: TopoEntry): string {
  switch (e.egress) {
    case 'DIRECT':
      return '本机直连出网'
    case 'EXTERNAL':
      return '之后由外部服务决定'
    case 'NONE':
      return '没有去向'
  }
  return ''
}

export const PENDING_TEXT: Record<string, string> = {
  PENDING: '已保存待下发',
  RETRY: '下发失败、等待重试',
  BLOCKED: '下发失败并回滚过,等人处理',
}

export const EDGE_STATE_TEXT: Record<TopoEdge['state'], string> = {
  OK: '配置关系',
  LEGACY: '待确认地址',
  BROKEN: '目标缺失',
  LOOP: '成环',
  UNKNOWN_BACKEND: '后端未知',
}

export function edgeKindText(k: TopoEdge['kind']): string {
  return k === 'FORWARD' ? '透明转发' : '代理连接'
}

export function entryTitle(e: Pick<TopoEntry, 'node_name' | 'name'>): string {
  return e.node_name ? `${e.node_name} / ${e.name}` : e.name
}

/** 入口在「入口管理」页里的位置:画布 ↔ 入口管理双向定位用。 */
export function inboundsLink(e: TopoEntry): string {
  return `/inbounds?focus=${encodeURIComponent(e.key)}`
}

export function topologyLink(key: string): string {
  return `/topology?focus=${encodeURIComponent(key)}`
}
