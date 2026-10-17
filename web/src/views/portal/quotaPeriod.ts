import type { PortalDashboard } from '@/api/client'
import { formatUTCDay } from '@/utils/format'

/**
 * 门户里三种流量数字的时间范围,说法只在这里(UI 2.0.1)。
 *
 *   额度已用(概览、我的流量的额度卡)  used_total:额度周期内的用量 —— 从上次重置
 *                                      (按月自动重置,或管理员手动重置)起算,
 *                                      从没重置过就是开通以来。额度判断用的就是它。
 *   本月(我的节点)                    month_bytes:UTC 自然月,1 日 00:00 起。
 *   累计(我的节点)                    total_bytes:开通以来的全部记录,重置不清零。
 *
 * 三者统计的是同一批字节,只是时间范围不同 —— 重置日不是 1 号、或者管理员手动
 * 重置过,数字就对不上,那是正常的。概览原来把额度周期内的用量标成「本月流量」,
 * 与「我的节点」的「本月」撞名,用户一加就以为面板算错了。
 * **不要为了让数字一致去改统计**,只把范围写在数字旁边。
 */
export function quotaPeriod(d: Pick<PortalDashboard, 'last_reset_at' | 'next_reset_at'>): string {
  const from = d.last_reset_at ? `${formatUTCDay(d.last_reset_at)}起` : '开通起'
  const to = d.next_reset_at ? `${formatUTCDay(d.next_reset_at)}重置` : '不自动重置'
  return `${from} · ${to}`
}

export const QUOTA_PERIOD_TIP =
  '额度周期内已用的流量:从上次重置(每月自动重置,或管理员手动重置)起算,从未重置过则从开通起算,按 UTC 计;不计流量节点的流量不算在内。「我的节点」里的「本月」按 UTC 自然月统计,时间范围不同,两个数字不一定相同。'

export const NODE_TRAFFIC_NOTE =
  '「今日」「本月」按 UTC 日与 UTC 自然月(1 日 00:00 起)统计,「累计」是开通以来的全部记录,与概览里的「额度已用」(按额度周期,重置后从零开始)不是同一个时间范围。'
