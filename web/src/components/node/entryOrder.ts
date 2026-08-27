/**
 * 入口在列表里的先后(V14.1)。
 *
 * ---------- 在此之前:排序字段被"种类"压住了 ----------
 *
 * 三类入口是分三段拼起来的(全部 sing-box → 全部 Mieru → 全部 nginx 转发),
 * 于是 sort_order 只在**同一类之内**有意义:一台机器上把 Mieru 入口排到 0、
 * VLESS 入口排到 1,列表里 VLESS 那条仍然在前面 —— 管理员改了那个数字、
 * 保存成功、什么都没发生,而面板不会说为什么。
 *
 * ---------- 判据必须与后端订阅那一侧完全一致 ----------
 *
 * 后端是 subscription.EntryOrder(先机器、再入口的 sort_order、
 * 平手时按种类与 id 兜底)。两边分叉的话,管理员在「入口管理」里看到的顺序
 * 与用户客户端里的顺序对不上 —— 而他正是照着面板上那个顺序去跟用户描述
 * "第三个节点"的。种类的取值顺序也照抄(sing-box → Mieru → nginx),
 * 所以 sort_order 全是默认值 0 的存量数据,渲染出来的顺序一个字都不变。
 */
export type EntryOrderKind = 'singbox' | 'mieru' | 'nginx' | 'realm'

/** 与后端 subscription 包里那组常量同序,不能改。realm(V15)排在 nginx 之后。 */
const kindRank: Record<EntryOrderKind, number> = {
  singbox: 0,
  mieru: 1,
  nginx: 2,
  realm: 3,
}

export interface EntryOrderKey {
  /** 机器的排序值与 id。同一台机器上的入口要挨在一起。 */
  nodeSort: number
  nodeId: number
  /** 入口自己的 sort_order —— 三类一起排,这是这一版要修的那一条。 */
  sort: number
  kind: EntryOrderKind
  id: number
}

/**
 * 按位置排序。**必须返回新数组**:computed 里就地 sort 会改到源数组,
 * 而那个源数组是 props 上的东西 —— 改它是在别人的状态上写字。
 */
export function sortByEntryOrder<T>(items: T[], key: (item: T) => EntryOrderKey): T[] {
  return [...items].sort((a, b) => {
    const x = key(a)
    const y = key(b)
    return (
      x.nodeSort - y.nodeSort ||
      x.nodeId - y.nodeId ||
      x.sort - y.sort ||
      kindRank[x.kind] - kindRank[y.kind] ||
      x.id - y.id
    )
  })
}
