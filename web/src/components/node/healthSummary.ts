import type { CloudNodeView, Node, NodeHealth, ServiceState } from '@/api/client'
import { metaOf, serviceStateMeta, type LbStatusMeta } from '@/components/lb/statusMeta'
import { isPreChange } from '@/components/lb/derive'

/**
 * 节点列表「服务巡检」列的摘要(V20)。
 *
 * 一格只放一句结论,细节在展开里。四种情形必须分得开,因为要人做的事不同:
 *
 *   服务未运行  去救那个服务(通常巡检已经在救)
 *   无法连接    SSH 都不通,服务是死是活并不知道 —— 可能只是机器在重启
 *   数据过期    巡检很久没跑到这台,结论不作数
 *   不适用      这台机器上没有面板托管的服务要查
 *
 * 多个 Mieru 实例:有一个没在跑就不算正常 —— 它们各自独立地跑与崩,
 * 「有一个在跑」算成正常的话,挂掉的那个再也不会被发现。
 */
export interface HealthItem {
  name: string
  state: ServiceState
  detail: string
}

export interface HealthSummary {
  meta: LbStatusMeta
  items: HealthItem[]
  /** 额外的异常徽标:云实例已停机、流量同步失败、自动恢复失败 / 已自动拉起 */
  badges: LbStatusMeta[]
  /** 巡检结果是什么时候的 */
  checkedAt: string | null
  /** 为真表示巡检结果说的是改管理地址之前的那台机器 */
  preChange: boolean
}

/** 巡检结果超过这个时长没更新就算过期(巡检默认两分钟一轮)。 */
export const HEALTH_STALE_MS = 15 * 60 * 1000

export function summarizeHealth(
  n: Node,
  h: NodeHealth | null | undefined,
  opts: { syncError?: string; cloud?: CloudNodeView | null; now?: number } = {},
): HealthSummary {
  const badges: LbStatusMeta[] = []
  if (opts.cloud && opts.cloud.instance_status === 'Stopped') badges.push(metaOf.mute('云实例已停机', 'square'))
  if (opts.syncError) badges.push(metaOf.bad('流量同步失败', 'triangle'))

  if (!h) {
    return { meta: metaOf.mute('—', 'ring'), items: [], badges, checkedAt: null, preChange: false }
  }
  const items: HealthItem[] = []
  if (n.role !== 'RELAY' && h.singbox !== 'NOT_APPLICABLE') {
    items.push({ name: 'sing-box', state: h.singbox, detail: h.singbox_detail })
  }
  if (h.nginx !== 'NOT_APPLICABLE') items.push({ name: 'nginx', state: h.nginx, detail: h.nginx_detail })
  if (h.realm && h.realm !== 'NOT_APPLICABLE') items.push({ name: 'realm', state: h.realm, detail: h.realm_detail })
  for (const m of h.mieru ?? []) {
    items.push({ name: `Mieru · ${m.display_name}`, state: m.state, detail: m.detail })
  }
  if (h.recover_error) badges.push(metaOf.bad('自动恢复失败', 'triangle'))
  else if (h.recovered) badges.push(metaOf.ok('已自动拉起', 'check'))

  const preChange = isPreChange(n, h.checked_at)
  const now = opts.now ?? Date.now()
  const stale = !!h.checked_at && now - new Date(h.checked_at).getTime() > HEALTH_STALE_MS

  let meta: LbStatusMeta
  const unreachable = items.some((i) => i.state === 'UNREACHABLE')
  const stopped = items.filter((i) => i.state === 'STOPPED')
  if (preChange) meta = { text: '变更前数据', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
  else if (unreachable) meta = serviceStateMeta.UNREACHABLE
  else if (stopped.length) meta = metaOf.bad(`${stopped.length} 项未运行`, 'triangle')
  else if (stale) meta = { text: '数据过期', shape: 'dashRing', fg: 'var(--text3)', bg: 'var(--fill)' }
  else if (!items.length) meta = serviceStateMeta.NOT_APPLICABLE
  else meta = metaOf.ok(items.length > 1 ? `正常 · ${items.length} 项` : '正常')
  return { meta, items, badges, checkedAt: h.checked_at, preChange }
}
