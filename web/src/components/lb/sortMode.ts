/**
 * 排序模式的两个纯函数:挪动、以及把新顺序折算成要写进库的排序值。
 *
 * 折算的规矩:**沿用这一组原来占着的那些值**,只是按新顺序重新分给每一项 —— 这一组以外的条目
 * (别的机器、外部代理)和它们的相对位置因此一个不动。原来有重复值(全是默认的 0)时
 * 往后顺延成严格递增;顺延超出范围才整体从下限起重新编号。只提交值真的变了的那几项。
 */

export interface SortRow {
  key: string
  kind: string
  id: number
  label: string
  sub?: string
  /** 打开时的排序值,也是提交时的「我看到的旧值」 */
  value: number
}

export interface SortBatchItem {
  kind: string
  id: number
  sort_order: number
  expected: number
}

/** 把 keys(保持它们之间原来的先后)挪到 where:上 / 下 / 顶 / 底,或者某个下标之前。 */
export function moveKeys(list: SortRow[], keys: string[], where: 'up' | 'down' | 'top' | 'bottom' | number): SortRow[] {
  const pick = new Set(keys)
  const moving = list.filter((r) => pick.has(r.key))
  if (!moving.length) return list
  const rest = list.filter((r) => !pick.has(r.key))
  let at: number
  if (where === 'top') at = 0
  else if (where === 'bottom') at = rest.length
  else if (where === 'up' || where === 'down') {
    // 以第一项(上移)或最后一项(下移)当前的位置为准挪一格。
    const first = list.findIndex((r) => pick.has(r.key))
    const before = list.slice(0, first).filter((r) => !pick.has(r.key)).length
    at = where === 'up' ? Math.max(0, before - 1) : Math.min(rest.length, before + 1)
  } else {
    // 落在某一行上:放到那一行之前(那一行本身在被挪的集合里时,放到它原来的位置)。
    const before = list.slice(0, where).filter((r) => !pick.has(r.key)).length
    at = Math.min(rest.length, before)
  }
  return [...rest.slice(0, at), ...moving, ...rest.slice(at)]
}

export function assignSortValues(
  original: SortRow[],
  next: SortRow[],
  min: number,
  max: number,
): { items: SortBatchItem[] } | { error: string } {
  const vals = original.map((r) => r.value).sort((a, b) => a - b)
  for (let i = 1; i < vals.length; i++) if (vals[i] <= vals[i - 1]) vals[i] = vals[i - 1] + 1
  if (vals.length && (vals[0] < min || vals[vals.length - 1] > max)) {
    if (min + vals.length - 1 > max) return { error: `条目太多,排序值放不进 ${min} ~ ${max}` }
    for (let i = 0; i < vals.length; i++) vals[i] = min + i
  }
  const items: SortBatchItem[] = []
  next.forEach((r, i) => {
    if (vals[i] !== r.value) items.push({ kind: r.kind, id: r.id, sort_order: vals[i], expected: r.value })
  })
  return { items }
}
