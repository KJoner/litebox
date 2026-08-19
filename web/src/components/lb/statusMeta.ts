import { color } from '@/theme/tokens'

/**
 * 状态的三重编码:形状 + 文案 + 颜色。
 *
 * 形状本身可区分,这样打印、投屏、色觉障碍下也读得出来:
 *   dot      实心圆 = 正常运行
 *   cross    叉     = 不可达 / 失败
 *   triangle 三角   = 需要人工动作
 *   spinner  虚线环 = 进行中
 *   pause    双竖条 = 人为暂停(节点停发订阅)
 *   square   方框   = 已禁用
 *   ring     空心环 = 未知 / 待初始化
 *   dashRing 灰虚线环 = 采样过期(不是离线)
 *   check    对勾   = 已同步 / 成功
 *   minus    短横   = 已跳过
 */
export type LbShape =
  | 'dot' | 'cross' | 'triangle' | 'spinner' | 'pause'
  | 'square' | 'ring' | 'dashRing' | 'check' | 'minus'

export interface LbStatusMeta {
  text: string
  shape: LbShape
  fg: string
  bg: string
  bd: string
}

const ok = (text: string, shape: LbShape = 'dot'): LbStatusMeta =>
  ({ text, shape, fg: color.success, bg: color.successBg, bd: color.successBorder })
const warn = (text: string, shape: LbShape = 'triangle'): LbStatusMeta =>
  ({ text, shape, fg: color.warning, bg: color.warningBg, bd: color.warningBorder })
const bad = (text: string, shape: LbShape = 'cross'): LbStatusMeta =>
  ({ text, shape, fg: color.danger, bg: color.dangerBg, bd: color.dangerBorder })
const info = (text: string, shape: LbShape = 'spinner'): LbStatusMeta =>
  ({ text, shape, fg: color.brand, bg: color.brandBg, bd: color.brandBorder })
const mute = (text: string, shape: LbShape = 'ring'): LbStatusMeta =>
  ({ text, shape, fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder })
const paused = (text: string): LbStatusMeta =>
  ({ text, shape: 'pause', fg: color.maintenance, bg: color.maintenanceBg, bd: color.maintenanceBorder })

/**
 * 用户状态。DEPLOY_PENDING / DEPLOY_FAILED 也要显示 ——
 * 档案改好了但节点还没下发,用户其实连不上。
 */
export const userStatusMeta: Record<string, LbStatusMeta> = {
  ACTIVE: ok('正常'),
  DISABLED: mute('已停用', 'square'),
  EXPIRED: bad('已到期'),
  QUOTA_EXCEEDED: bad('流量用尽', 'triangle'),
  DEPLOY_PENDING: info('待部署'),
  DEPLOY_FAILED: bad('部署失败'),
}

/**
 * 节点状态。离线升红:原来 OFFLINE 是 orange 而 DEPLOY_FAILED 才是 red,
 * 管理员会以为橙色的离线不要紧。两者都红,靠形状区分(叉 / 叉)——
 * 所以 DEPLOY_FAILED 用三角:它需要人工动作,离线是机器不可达。
 */
export const nodeStatusMeta: Record<string, LbStatusMeta> = {
  ONLINE: ok('运行中'),
  OFFLINE: bad('离线'),
  DEPLOY_FAILED: bad('部署失败', 'triangle'),
  // 「待初始化」与「已禁用」原来同为 default 灰,一个是还没装、一个是人为关的。
  PENDING: mute('待初始化', 'ring'),
  DISABLED: mute('已禁用', 'square'),
}

export const deployStatusMeta: Record<string, LbStatusMeta> = {
  SUCCESS: ok('成功', 'check'),
  FAILED: bad('失败'),
  ROLLED_BACK: warn('已回滚'),
  RUNNING: info('进行中'),
  // 配置无变化、什么都没做。不能标绿 —— 那会让人以为部署过了。
  SKIPPED: mute('已跳过', 'minus'),
}

/**
 * 配置状态。取后端 config_state,前端不推导 —— 见 derive.ts 里的说明。
 * 字段上线前一律落在 UNKNOWN。
 */
export const configStatusMeta: Record<string, LbStatusMeta> = {
  IN_SYNC: ok('已同步', 'check'),
  PENDING: warn('待部署'),
  DEPLOY_FAILED: bad('部署失败', 'triangle'),
  NEVER_DEPLOYED: mute('未部署', 'ring'),
  UNKNOWN: mute('未知', 'ring'),
  // 中转机上没有 sing-box,这个问题在它身上没有主语。
  // 与「未知」分开:那一档的意思是"本该知道但算不出来",会催着人去查。
  NOT_APPLICABLE: mute('不适用', 'minus'),
}

/** 门户只用这三种。用户看不懂 rev 与部署。 */
export const portalNodeStatusMeta: Record<string, LbStatusMeta> = {
  normal: ok('正常'),
  maintenance: paused('维护中'),
  disabled: mute('已停用', 'square'),
}

/**
 * 外部代理条目状态。
 *
 * 「上游已消失」与「已到期」都是红的,靠形状分开 —— 前者要去问机场,
 * 后者要去续费,两件完全不同的事。只靠颜色的话打印与投屏下分不出来。
 */
export const externalProxyStatusMeta: Record<string, LbStatusMeta> = {
  ACTIVE: ok('正常'),
  DISABLED: mute('已停用', 'square'),
  // 「上游有但我不要」—— 导入时没勾选的。不是故障,是刻意排除。
  EXCLUDED: mute('已排除', 'minus'),
}

/** 上游连续未出现。达到阈值会自动退出订阅,但永不自动删除。 */
export const missingMeta = (rounds: number, threshold: number): LbStatusMeta =>
  rounds >= threshold
    ? bad('上游已消失 ' + rounds + ' 轮 · 已退出订阅', 'triangle')
    : warn('上游第 ' + rounds + ' 次未出现')

/**
 * 条目或其所属源已到期 —— 要去续费,与「上游消失」是两件事。
 *
 * 文案里带上后果:到期与「下发订阅」开关是两个维度,开关开着但已到期时
 * 那一条其实不在订阅里。只写「已到期」的话,管理员看到开关是开的,
 * 会以为它还在发。
 */
export const expiredMeta: LbStatusMeta = bad('已到期 · 已退出订阅')

/** 代理源同步失败。连续多次才报 —— 偶发失败是常态。 */
export const syncFailedMeta = (times: number): LbStatusMeta =>
  bad('同步连续失败 ' + times + ' 次', 'triangle')

/** 采样过期。只挂在「最后同步」列上,不污染运行状态。 */
export const staleMeta: LbStatusMeta = {
  text: '数据过期',
  shape: 'dashRing',
  fg: color.text3,
  bg: color.bgPage,
  bd: color.neutralBorder,
}

/** 节点停发订阅 —— subscription_enabled=false,与 DISABLED 是两回事。 */
export const subscriptionOffMeta: LbStatusMeta = paused('停发订阅')

export type LbStatusKind =
  | 'user' | 'node' | 'deploy' | 'config' | 'portalNode' | 'externalProxy'

const tables: Record<LbStatusKind, Record<string, LbStatusMeta>> = {
  user: userStatusMeta,
  node: nodeStatusMeta,
  deploy: deployStatusMeta,
  config: configStatusMeta,
  portalNode: portalNodeStatusMeta,
  externalProxy: externalProxyStatusMeta,
}

export function statusMeta(kind: LbStatusKind, status: string): LbStatusMeta {
  return tables[kind][status] ?? mute(status, 'ring')
}
