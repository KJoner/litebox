import type { ServiceStatus } from '@/api/client'
import { metaOf, type LbStatusMeta } from '@/components/lb'

/**
 * 服务卡片的状态判定。
 *
 * 四张卡片(sing-box / Mieru / nginx / realm)那颗随状态变化的大按钮
 * 只认这里算出来的 phase:没装 → 「安装」;装了没跑 → 「启动」;
 * 在跑 → 「停止」。判据只有这一处,四张卡片各写一遍的话,某天改了
 * "没配置算不算已安装",四处迟早分叉。
 */
export type ServicePhase =
  /** 还没探测过、或正在探测 */
  | 'UNKNOWN'
  /** 探测失败(SSH 不通):服务是死是活我们并不知道 */
  | 'UNREACHABLE'
  | 'NOT_INSTALLED'
  /** 装了但没在跑 */
  | 'STOPPED'
  | 'RUNNING'

export function servicePhase(status: ServiceStatus | null, error: string): ServicePhase {
  if (error) return 'UNREACHABLE'
  if (!status) return 'UNKNOWN'
  if (!status.installed) return 'NOT_INSTALLED'
  if (status.state === 'RUNNING') return 'RUNNING'
  return 'STOPPED'
}

/**
 * 状态标签。三重编码(形状 + 文案 + 颜色),与全站一致。
 *
 * 「已安装」分两档:配置在而没在跑,是一个本该在服务却停了的东西,给警告色;
 * 从没下发过配置的,停着是正常的,给中性色。混成一档的话,一台刚装好
 * 还没下发的机器会常年顶着一个黄三角,几次之后管理员就不看它了。
 *
 * suffix 给 Mieru 用:「运行中 2/3」里多出的那一截正是要人去救的那一个。
 */
export function servicePhaseMeta(
  phase: ServicePhase,
  status: ServiceStatus | null,
  opts: { checking?: boolean; suffix?: string; partial?: boolean } = {},
): LbStatusMeta {
  if (opts.checking) return metaOf.info('检查中', 'spinner')
  switch (phase) {
    case 'UNKNOWN':
      return metaOf.mute('未检查', 'ring')
    case 'UNREACHABLE':
      return metaOf.bad('连不上', 'cross')
    case 'NOT_INSTALLED':
      return metaOf.mute('未安装', 'ring')
    case 'RUNNING':
      return opts.partial
        ? metaOf.warn(`运行中${opts.suffix ?? ''}`, 'triangle')
        : metaOf.ok(`运行中${opts.suffix ?? ''}`, 'dot')
    case 'STOPPED':
      return status?.config_present
        ? metaOf.warn('已安装 · 没在跑', 'triangle')
        : metaOf.mute('已安装', 'square')
  }
}
