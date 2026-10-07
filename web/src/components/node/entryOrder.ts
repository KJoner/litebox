import type { OrderScheme } from '@/api/client'

/**
 * 入口在列表里的先后(V14.1 定下「先机器再入口」,V20 加了全局排序值)。
 *
 * ---------- 判据必须与后端订阅那一侧完全一致 ----------
 *
 * 后端是 subscription.EntryOrder:旧方案(LEGACY)先机器、再入口的 sort_order、
 * 平手时按种类与 id 兜底;新方案(GLOBAL)先比全局排序值
 * (自建 = 节点排序号 × 1000 + 入口序号,外部代理自带),同值自建在前、外部在后,
 * 再回到旧方案的那串键。两边分叉的话,管理员在「入口管理」里看到的顺序
 * 与用户客户端里的顺序对不上 —— 而他正是照着面板上那个顺序去跟用户描述
 * "第三个节点"的。种类的取值顺序也照抄(sing-box → Mieru → nginx → realm → 外部代理)。
 *
 * 旧方案下外部代理不参与这次排序 —— 它们在订阅里整块排在自建节点之前或之后
 * (subscription_external_position),列表里按同一规则放到两头。
 */
export type EntryOrderKind = 'singbox' | 'mieru' | 'nginx' | 'realm' | 'external'

/** 与后端 subscription 包里那组常量同序,不能改。 */
const kindRank: Record<EntryOrderKind, number> = {
  singbox: 0,
  mieru: 1,
  nginx: 2,
  realm: 3,
  external: 4,
}

/** 全局排序值的号段宽度与各段范围,与后端 subscription 包里的常量一致。 */
export const GLOBAL_STRIDE = 1000
export const NODE_SORT_MIN = 1
export const NODE_SORT_MAX = 1000
export const ENTRY_SORT_MIN = 0
export const ENTRY_SORT_MAX = GLOBAL_STRIDE - 1

export interface EntryOrderKey {
  /** 机器的排序值与 id。同一台机器上的入口要挨在一起。外部代理这两项给 0。 */
  nodeSort: number
  nodeId: number
  /** 入口自己的 sort_order(自建),或外部代理自带的全局排序值。 */
  sort: number
  kind: EntryOrderKind
  id: number
}

/** 这一条在 GLOBAL 方案里参与比较的那个数。 */
export function globalValue(k: EntryOrderKey): number {
  return k.kind === 'external' ? k.sort : k.nodeSort * GLOBAL_STRIDE + k.sort
}

function sourceRank(k: EntryOrderKey): number {
  return k.kind === 'external' ? 1 : 0
}

function legacyCompare(x: EntryOrderKey, y: EntryOrderKey): number {
  return (
    x.nodeSort - y.nodeSort ||
    x.nodeId - y.nodeId ||
    x.sort - y.sort ||
    kindRank[x.kind] - kindRank[y.kind] ||
    x.id - y.id
  )
}

/**
 * 按位置排序。**必须返回新数组**:computed 里就地 sort 会改到源数组,
 * 而那个源数组是 props 上的东西 —— 改它是在别人的状态上写字。
 *
 * 旧方案里 externalPosition 决定外部代理整块放前面还是后面;GLOBAL 方案不看它。
 */
export function sortByEntryOrder<T>(
  items: T[],
  key: (item: T) => EntryOrderKey,
  scheme: OrderScheme = 'LEGACY',
  externalPosition: 'BEFORE' | 'AFTER' = 'AFTER',
): T[] {
  return [...items].sort((a, b) => {
    const x = key(a)
    const y = key(b)
    if (scheme === 'GLOBAL') {
      return globalValue(x) - globalValue(y) || sourceRank(x) - sourceRank(y) || legacyCompare(x, y)
    }
    const sx = sourceRank(x)
    const sy = sourceRank(y)
    if (sx !== sy) return externalPosition === 'BEFORE' ? sy - sx : sx - sy
    if (sx === 1) return x.sort - y.sort || x.id - y.id
    return legacyCompare(x, y)
  })
}
