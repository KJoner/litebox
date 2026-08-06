import type { Node, NodeConfigState, ProxyUser } from '@/api/client'
import { threshold } from '@/theme/tokens'
import { staleMeta, subscriptionOffMeta, type LbStatusMeta } from './statusMeta'

/**
 * 派生态。全部由已有字段算出,不新增任何请求。
 *
 * 这些东西**不是** status:节点只有一个 status 枚举,用户也只有一个。
 * 「即将到期」「接近额度」「无可用节点」「待改初始密码」都与 status 正交,
 * 在界面上占状态列的第二行或独立标记,不挤进同一个标签。
 */

export function daysUntil(iso: string | null): number | null {
  if (!iso) return null
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return null
  return Math.ceil((t - Date.now()) / 86400000)
}

export function isExpiringSoon(u: ProxyUser): boolean {
  const d = daysUntil(u.expires_at)
  return d !== null && d >= 0 && d <= threshold.expiringSoonDays
}

/** 不限量的用户永远不算「接近上限」:没有上限可接近。 */
export function isNearQuota(u: ProxyUser): boolean {
  if (u.quota_bytes <= 0) return false
  return u.used_total / u.quota_bytes >= threshold.nearQuotaRatio
}

/** 等级继承 + 额外授权合并后一个都没有。这是最容易被忽略的「配好了但用不了」。 */
export function hasNoUsableNode(u: ProxyUser): boolean {
  return u.effective_node_ids.length === 0
}

export function mustChangePassword(u: ProxyUser): boolean {
  return u.portal_account?.must_change_password === true
}

/** login_enabled=false 全站统称「门户登录已关闭」,不叫「已停用」。 */
export function portalLoginOff(u: ProxyUser): boolean {
  return u.portal_account?.login_enabled === false
}

/**
 * 行主操作:随状态变,只留一个。
 * 管理员打开列表永远是为了处理某个人的某件事,让他先读完六个等重的文字链
 * 再自己判断该点哪个,是把分诊工作推给了他。
 */
export type LbUserAction = 'renew' | 'addQuota' | 'enable' | 'assignNode' | 'detail'

export function primaryUserAction(u: ProxyUser): LbUserAction {
  if (u.status === 'DISABLED') return 'enable'
  if (u.status === 'EXPIRED' || isExpiringSoon(u)) return 'renew'
  if (u.status === 'QUOTA_EXCEEDED' || isNearQuota(u)) return 'addQuota'
  if (hasNoUsableNode(u)) return 'assignNode'
  return 'detail'
}

export const userActionLabel: Record<LbUserAction, string> = {
  renew: '续期',
  addQuota: '加流量',
  enable: '恢复',
  assignNode: '分配节点',
  detail: '详情',
}

/**
 * 节点配置状态。**只读后端给的业务字段,前端不推导。**
 *
 * 这个值回答的是「库里当前的配置是否已经在节点上生效」。前端手边的三样东西
 * 都答不了它:
 *   - 在线状态  只说 sing-box 在跑,不说跑的是哪一版;
 *   - config_revision  是库里的目标版本,没有「节点上是第几版」与它对比;
 *   - 最近部署记录  成功过不等于此后没再改过库。
 * 拿这三样猜出来的「已同步」是会骗人的 —— 管理员据此不去部署,
 * 被移出的用户就一直还能用。
 *
 * 后端按 deployed_config_sha256 与此刻重新渲染出的哈希比对得出,不连 SSH。
 * 创建 / 更新节点的响应里没有这两个字段(调用方随后都会重拉列表),
 * 所以取不到时落在 UNKNOWN 与「不催」上 —— 宁可显示「未知」。
 */
export function configState(n: Node): NodeConfigState {
  return n.config_state ?? 'UNKNOWN'
}

/** 是否该提示部署。字段缺失时返回 false —— 不猜,也不催。 */
export function needsDeploy(n: Node): boolean {
  return n.needs_deploy === true
}

/**
 * 采样是否过期。取采集周期(5 分钟)的两倍 —— 只错过一次采集很常见
 * (节点忙、网络抖),报「过期」会天天误报。
 *
 * **过期只是 warning,不得渲染成离线。** 采集走独立 SSH 通道,
 * 取不到不代表代理服务停了。
 */
export function isMetricsStale(collectedAt: string | null | undefined): boolean {
  if (!collectedAt) return false
  const t = new Date(collectedAt).getTime()
  return Number.isNaN(t) || Date.now() - t > threshold.metricsStaleMs
}

/** 节点身上的附加标记,与运行状态并列显示,不互相覆盖。 */
export function nodeBadges(n: Node, collectedAt?: string | null): LbStatusMeta[] {
  const out: LbStatusMeta[] = []
  if (!n.subscription_enabled) out.push(subscriptionOffMeta)
  if (isMetricsStale(collectedAt)) out.push(staleMeta)
  return out
}
