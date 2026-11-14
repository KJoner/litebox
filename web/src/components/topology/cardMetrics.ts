import type { TopoEndpoint, TopoEntry } from '@/api/client'

/**
 * 卡片尺寸只在这里算一次:布局按它排位置,卡片按它画 —— 两处各算一遍的话,
 * 线会接到卡片的空白处,或者穿过卡片正文。
 *
 * 一个入口恰有三条地址时正常缩放下完整显示三条;更多的折叠,写出总数,展开或在详情抽屉里看全量。
 */
export const CARD_W = 272
export const AUX_W = 208
export const AUX_H = 68
export const FOLD_AT = 3

const PAD = 10
const TITLE = 24
const SUB = 18
const ACCESS = 20
const ROW = 20
const MORE = 18
const STATUS = 22
const GAP = 4

/** 卡片里显示哪几条地址。 */
export function visibleEndpoints(e: TopoEntry, expanded: boolean): TopoEndpoint[] {
  if (expanded || e.endpoints.length <= FOLD_AT) return e.endpoints
  return e.endpoints.slice(0, FOLD_AT)
}

export function hasMore(e: TopoEntry): boolean {
  return e.endpoints.length > FOLD_AT
}

export function cardHeight(e: TopoEntry, expanded: boolean): number {
  const rows = visibleEndpoints(e, expanded).length
  return PAD * 2 + TITLE + SUB + ACCESS + rows * ROW + (hasMore(e) ? MORE : 0) + STATUS + GAP * 4
}

/** 第 idx 条地址那一行的中线(相对卡片顶):入线接在被选中的那一行上。 */
export function endpointRowY(idx: number): number {
  return PAD + TITLE + GAP + SUB + GAP + ACCESS + GAP + idx * ROW + ROW / 2
}

/** 折叠行(「另外 N 条」)的中线:选中的地址在折叠区里时先接到这里,选中边时自动展开。 */
export function moreRowY(e: TopoEntry, expanded: boolean): number {
  return PAD + TITLE + GAP + SUB + GAP + ACCESS + GAP + visibleEndpoints(e, expanded).length * ROW + MORE / 2
}

export function outPortY(e: TopoEntry, expanded: boolean): number {
  // 出线从状态行(写着下一跳摘要的那一行)出去。
  return cardHeight(e, expanded) - PAD - STATUS / 2
}

/** 入线落在哪个纵向位置:选定的地址条目那一行;默认地址或兼容快照落在第一行。 */
export function inPortY(target: TopoEntry, endpointID: number | undefined, expanded: boolean): number {
  const vis = visibleEndpoints(target, expanded)
  const idx = endpointID ? vis.findIndex((ep) => ep.id === endpointID) : vis.findIndex((ep) => ep.default)
  if (idx >= 0) return endpointRowY(idx)
  if (endpointID && target.endpoints.some((ep) => ep.id === endpointID)) return moreRowY(target, expanded)
  return endpointRowY(0)
}
