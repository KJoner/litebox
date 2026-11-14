/**
 * 链路画布的分层布局(v23.3):从左到右,没有上游的根在左,中间跳逐列排列,终点在各自连通分量的右侧。
 *
 * 没用 ELK:它的打包体积约 1.4 MB,而这里要的只是有向分层图里最常见的那几步 ——
 * 最长路径分层、长边插虚拟点、重心法减少交叉、按层的通道里正交走线 —— 几百行就够,
 * 也不必再引一个图组件库。纯函数,不碰 DOM,所以能原样放进 Web Worker 里跑(layout.worker.ts)。
 *
 * 真实的业务边方向**永远不倒过来**:存量数据里成环的那几条边找出来(back),从分量下方绕一圈画回去,
 * 由界面标成「成环」,不为了满足从左到右悄悄把箭头反过来。
 */

export interface LNode {
  id: string
  w: number
  h: number
  /** 稳定排序的依据(沿用主机与入口的排序号),只影响同层内的先后,不改任何业务排序 */
  order: number
}

export interface LEdge {
  id: string
  from: string
  to: string
  /** 出线 / 入线在卡片内的纵向位置(相对卡片顶);缺省取卡片中线。入线落在所选地址那一行 */
  fromY?: number
  toY?: number
}

export interface LayoutResult {
  nodes: Record<string, { x: number; y: number; w: number; h: number; layer: number; component: number }>
  edges: Record<string, { points: [number, number][]; back: boolean }>
  width: number
  height: number
  /** 分区:每个连通分量一块,独立入口单独一块 */
  blocks: { x: number; y: number; w: number; h: number; kind: 'component' | 'isolated'; count: number }[]
}

const GAP_X = 120
const GAP_Y = 24
const COMP_GAP = 56
const PAD = 32
const DUMMY_H = 10
const BACK_LANE = 14

interface Box {
  id: string
  w: number
  h: number
  order: number
  dummy: boolean
  layer: number
  pos: number
  x: number
  y: number
}

export function layout(nodesIn: LNode[], edgesIn: LEdge[]): LayoutResult {
  const byId = new Map<string, LNode>()
  for (const n of nodesIn) byId.set(n.id, n)
  const edges = edgesIn.filter((e) => byId.has(e.from) && byId.has(e.to) && e.from !== e.to)

  // 连通分量(无向)。
  const parent = new Map<string, string>()
  const find = (x: string): string => {
    let r = x
    while (parent.get(r) !== r) r = parent.get(r)!
    let c = x
    while (parent.get(c) !== r) {
      const nx = parent.get(c)!
      parent.set(c, r)
      c = nx
    }
    return r
  }
  for (const n of nodesIn) parent.set(n.id, n.id)
  for (const e of edges) {
    const a = find(e.from)
    const b = find(e.to)
    if (a !== b) parent.set(a, b)
  }
  const groups = new Map<string, LNode[]>()
  for (const n of nodesIn) {
    const r = find(n.id)
    if (!groups.has(r)) groups.set(r, [])
    groups.get(r)!.push(n)
  }
  const touched = new Set<string>()
  for (const e of edges) {
    touched.add(e.from)
    touched.add(e.to)
  }
  const comps: LNode[][] = []
  const isolated: LNode[] = []
  for (const g of groups.values()) {
    if (g.length === 1 && !touched.has(g[0].id)) isolated.push(g[0])
    else comps.push(g)
  }
  const minOrder = (g: LNode[]) => Math.min(...g.map((n) => n.order))
  comps.sort((a, b) => minOrder(a) - minOrder(b))
  isolated.sort((a, b) => a.order - b.order)

  const result: LayoutResult = { nodes: {}, edges: {}, width: 0, height: 0, blocks: [] }
  let yOff = PAD
  let maxW = 0

  comps.forEach((comp, ci) => {
    const r = layoutComponent(comp, edges.filter((e) => comp.some((n) => n.id === e.from)), ci)
    for (const [id, b] of Object.entries(r.nodes)) {
      result.nodes[id] = { ...b, x: b.x + PAD, y: b.y + yOff }
    }
    for (const [id, e] of Object.entries(r.edges)) {
      result.edges[id] = { back: e.back, points: e.points.map(([x, y]) => [x + PAD, y + yOff]) }
    }
    result.blocks.push({ x: PAD - 12, y: yOff - 12, w: r.width + 24, h: r.height + 24, kind: 'component', count: comp.length })
    yOff += r.height + COMP_GAP
    maxW = Math.max(maxW, r.width)
  })

  // 独立入口(没有上游、也没有下一跳的):打成网格放在最后一块,带直出标记,不拉成一条长列。
  if (isolated.length) {
    const w = Math.max(...isolated.map((n) => n.w))
    const cols = Math.max(1, Math.min(isolated.length, Math.floor(Math.max(maxW, w * 4 + GAP_Y * 3) / (w + GAP_Y))))
    let rowY = yOff
    let rowH = 0
    isolated.forEach((n, i) => {
      const col = i % cols
      if (col === 0 && i > 0) {
        rowY += rowH + GAP_Y
        rowH = 0
      }
      result.nodes[n.id] = { x: PAD + col * (w + GAP_Y), y: rowY, w: n.w, h: n.h, layer: 0, component: -1 }
      rowH = Math.max(rowH, n.h)
    })
    const blockW = cols * (w + GAP_Y) - GAP_Y
    result.blocks.push({ x: PAD - 12, y: yOff - 12, w: blockW + 24, h: rowY + rowH - yOff + 24, kind: 'isolated', count: isolated.length })
    yOff = rowY + rowH + COMP_GAP
    maxW = Math.max(maxW, blockW)
  }
  result.width = maxW + PAD * 2
  result.height = Math.max(yOff - COMP_GAP + PAD, PAD * 2)
  return result
}

function layoutComponent(nodes: LNode[], edges: LEdge[], ci: number) {
  const ids = new Set(nodes.map((n) => n.id))
  const out = new Map<string, LEdge[]>()
  const inc = new Map<string, LEdge[]>()
  for (const n of nodes) {
    out.set(n.id, [])
    inc.set(n.id, [])
  }
  for (const e of edges) {
    if (!ids.has(e.to)) continue
    out.get(e.from)!.push(e)
    inc.get(e.to)!.push(e)
  }
  const sorted = [...nodes].sort((a, b) => a.order - b.order)

  // 找环里的回边:从根(没有上游的)开始按排序深搜,指回栈上的就是回边。
  const back = new Set<string>()
  const state = new Map<string, number>() // 0 未访问,1 在栈上,2 完成
  const visit = (start: string) => {
    const stack: { id: string; i: number }[] = [{ id: start, i: 0 }]
    state.set(start, 1)
    while (stack.length) {
      const top = stack[stack.length - 1]
      const outs = out.get(top.id)!
      if (top.i >= outs.length) {
        state.set(top.id, 2)
        stack.pop()
        continue
      }
      const e = outs[top.i++]
      const s = state.get(e.to) ?? 0
      if (s === 1) back.add(e.id)
      else if (s === 0) {
        state.set(e.to, 1)
        stack.push({ id: e.to, i: 0 })
      }
    }
  }
  for (const n of sorted) if (inc.get(n.id)!.length === 0 && !state.get(n.id)) visit(n.id)
  for (const n of sorted) if (!state.get(n.id)) visit(n.id)

  // 最长路径分层(只看非回边)。
  const layer = new Map<string, number>()
  const indeg = new Map<string, number>()
  for (const n of nodes) indeg.set(n.id, inc.get(n.id)!.filter((e) => !back.has(e.id)).length)
  const queue = sorted.filter((n) => indeg.get(n.id) === 0).map((n) => n.id)
  for (const id of queue) layer.set(id, 0)
  while (queue.length) {
    const id = queue.shift()!
    for (const e of out.get(id)!) {
      if (back.has(e.id)) continue
      layer.set(e.to, Math.max(layer.get(e.to) ?? 0, (layer.get(id) ?? 0) + 1))
      indeg.set(e.to, indeg.get(e.to)! - 1)
      if (indeg.get(e.to) === 0) queue.push(e.to)
    }
  }
  let maxLayer = 0
  for (const n of nodes) {
    if (!layer.has(n.id)) layer.set(n.id, 0)
    maxLayer = Math.max(maxLayer, layer.get(n.id)!)
  }
  // 终点(有上游、没有下一跳)对齐到分量最右一列。
  for (const n of nodes) {
    const hasOut = out.get(n.id)!.some((e) => !back.has(e.id))
    if (!hasOut && inc.get(n.id)!.length > 0) layer.set(n.id, maxLayer)
  }

  // 盒子(含长边上的虚拟点)。
  const boxes = new Map<string, Box>()
  for (const n of nodes) {
    boxes.set(n.id, { id: n.id, w: n.w, h: n.h, order: n.order, dummy: false, layer: layer.get(n.id)!, pos: 0, x: 0, y: 0 })
  }
  const chains = new Map<string, string[]>()
  for (const e of edges) {
    if (!ids.has(e.to) || back.has(e.id)) continue
    const l0 = layer.get(e.from)!
    const l1 = layer.get(e.to)!
    const chain = [e.from]
    for (let l = l0 + 1; l < l1; l++) {
      const id = `${e.id}#${l}`
      boxes.set(id, { id, w: 0, h: DUMMY_H, order: boxes.get(e.from)!.order, dummy: true, layer: l, pos: 0, x: 0, y: 0 })
      chain.push(id)
    }
    chain.push(e.to)
    chains.set(e.id, chain)
  }
  const layers: Box[][] = Array.from({ length: maxLayer + 1 }, () => [])
  for (const b of boxes.values()) layers[b.layer].push(b)
  for (const L of layers) {
    L.sort((a, b) => a.order - b.order || (a.id < b.id ? -1 : 1))
    L.forEach((b, i) => (b.pos = i))
  }
  const preds = new Map<string, string[]>()
  const succs = new Map<string, string[]>()
  for (const b of boxes.values()) {
    preds.set(b.id, [])
    succs.set(b.id, [])
  }
  for (const chain of chains.values()) {
    for (let i = 0; i + 1 < chain.length; i++) {
      succs.get(chain[i])!.push(chain[i + 1])
      preds.get(chain[i + 1])!.push(chain[i])
    }
  }

  // 重心法减少交叉:上下各扫几遍,同值按原来的先后(稳定)。
  const bary = (b: Box, nb: string[]) =>
    nb.length ? nb.reduce((s, id) => s + boxes.get(id)!.pos, 0) / nb.length : b.pos
  for (let iter = 0; iter < 6; iter++) {
    const down = iter % 2 === 0
    const range = down ? layers.map((_, i) => i).slice(1) : layers.map((_, i) => i).reverse().slice(1)
    for (const li of range) {
      const L = layers[li]
      const key = new Map(L.map((b) => [b.id, bary(b, down ? preds.get(b.id)! : succs.get(b.id)!)]))
      L.sort((a, b) => key.get(a.id)! - key.get(b.id)! || a.pos - b.pos)
      L.forEach((b, i) => (b.pos = i))
    }
  }

  // 纵向:先顺排,再按相邻层的重心往中间靠(不重叠)。
  for (const L of layers) {
    let y = 0
    for (const b of L) {
      b.y = y
      y += b.h + GAP_Y
    }
  }
  const center = (b: Box) => b.y + b.h / 2
  for (let pass = 0; pass < 4; pass++) {
    const order = pass % 2 === 0 ? layers.map((_, i) => i) : layers.map((_, i) => i).reverse()
    for (const li of order) {
      const L = layers[li]
      const want = L.map((b) => {
        const nb = [...preds.get(b.id)!, ...succs.get(b.id)!].map((id) => boxes.get(id)!)
        return nb.length ? nb.reduce((s, x) => s + center(x), 0) / nb.length - b.h / 2 : b.y
      })
      let floor = 0
      for (let i = 0; i < L.length; i++) {
        L[i].y = Math.max(want[i], floor)
        floor = L[i].y + L[i].h + GAP_Y
      }
      // 往回收:最后一个往下挤出去的那一截,整体往上推回来一点(不越过上一个)。
      for (let i = L.length - 2; i >= 0; i--) {
        const limit = L[i + 1].y - GAP_Y - L[i].h
        if (L[i].y > limit) L[i].y = limit
      }
      const top = Math.min(...L.map((b) => b.y))
      if (top < 0) for (const b of L) b.y -= top
    }
  }
  let minY = Infinity
  for (const b of boxes.values()) minY = Math.min(minY, b.y)
  for (const b of boxes.values()) b.y -= minY

  // 横向:每一层的宽度取它最宽的卡片。
  const layerW = layers.map((L) => Math.max(0, ...L.map((b) => b.w)))
  const layerX: number[] = []
  let x = 0
  for (let li = 0; li < layers.length; li++) {
    layerX.push(x)
    x += (layerW[li] || 40) + GAP_X
  }
  for (const b of boxes.values()) b.x = layerX[b.layer]
  const width = x - GAP_X

  let height = 0
  for (const b of boxes.values()) height = Math.max(height, b.y + b.h)

  // 走线:相邻两层之间是一条通道,同一条通道里的竖线按出发高度分到不同的轨道上。
  const edgeById = new Map(edges.map((e) => [e.id, e]))
  const portY = (id: string, which: 'from' | 'to', e: LEdge) => {
    const b = boxes.get(id)!
    if (b.dummy) return b.y + b.h / 2
    const off = which === 'from' ? e.fromY : e.toY
    return b.y + (off ?? b.h / 2)
  }
  type Seg = { edge: string; idx: number; y0: number; y1: number }
  const channels = new Map<number, Seg[]>()
  const segPoints = new Map<string, { x0: number; y0: number; x1: number; y1: number; layer: number }[]>()
  for (const [eid, chain] of chains) {
    const e = edgeById.get(eid)!
    const segs: { x0: number; y0: number; x1: number; y1: number; layer: number }[] = []
    for (let i = 0; i + 1 < chain.length; i++) {
      const a = boxes.get(chain[i])!
      const b = boxes.get(chain[i + 1])!
      const y0 = i === 0 ? portY(a.id, 'from', e) : portY(a.id, 'from', e)
      const y1 = i + 1 === chain.length - 1 ? portY(b.id, 'to', e) : portY(b.id, 'to', e)
      const x0 = a.dummy ? layerX[a.layer] + layerW[a.layer] : a.x + a.w
      segs.push({ x0, y0, x1: b.x, y1, layer: a.layer })
      if (!channels.has(a.layer)) channels.set(a.layer, [])
      channels.get(a.layer)!.push({ edge: eid, idx: i, y0, y1 })
    }
    segPoints.set(eid, segs)
  }
  const track = new Map<string, number>()
  for (const [li, segs] of channels) {
    segs.sort((p, q) => p.y0 - q.y0 || p.y1 - q.y1)
    const start = layerX[li] + layerW[li]
    const end = layerX[li + 1] ?? start + GAP_X
    const inner = Math.max(8, end - start - 32)
    segs.forEach((s, i) => track.set(`${s.edge}:${s.idx}`, start + 16 + ((i + 1) * inner) / (segs.length + 1)))
  }
  const resEdges: LayoutResult['edges'] = {}
  for (const [eid, segs] of segPoints) {
    const pts: [number, number][] = []
    segs.forEach((s, i) => {
      const xt = track.get(`${eid}:${i}`) ?? (s.x0 + s.x1) / 2
      if (i === 0) pts.push([s.x0, s.y0])
      else pts.push([s.x0 - layerW[s.layer], s.y0], [s.x0, s.y0])
      pts.push([xt, s.y0], [xt, s.y1], [s.x1, s.y1])
    })
    resEdges[eid] = { back: false, points: simplify(pts) }
  }
  // 回边:从分量下方绕回去,方向照旧。
  let lane = 0
  for (const e of edges) {
    if (!back.has(e.id) || !ids.has(e.to)) continue
    const a = boxes.get(e.from)!
    const b = boxes.get(e.to)!
    const yb = height + 18 + lane * BACK_LANE
    lane++
    const ya = portY(a.id, 'from', e)
    const yt = portY(b.id, 'to', e)
    resEdges[e.id] = {
      back: true,
      points: [
        [a.x + a.w, ya],
        [a.x + a.w + 18, ya],
        [a.x + a.w + 18, yb],
        [b.x - 18, yb],
        [b.x - 18, yt],
        [b.x, yt],
      ],
    }
  }
  if (lane) height += 18 + lane * BACK_LANE

  const resNodes: LayoutResult['nodes'] = {}
  for (const b of boxes.values()) {
    if (!b.dummy) resNodes[b.id] = { x: b.x, y: b.y, w: b.w, h: b.h, layer: b.layer, component: ci }
  }
  return { nodes: resNodes, edges: resEdges, width, height }
}

/** 去掉共线的中间点,让圆角只出现在真正的拐弯处。 */
function simplify(pts: [number, number][]): [number, number][] {
  const out: [number, number][] = []
  for (const p of pts) {
    const last = out[out.length - 1]
    if (last && Math.abs(last[0] - p[0]) < 0.5 && Math.abs(last[1] - p[1]) < 0.5) continue
    out.push(p)
  }
  const res: [number, number][] = []
  for (let i = 0; i < out.length; i++) {
    const a = res[res.length - 1]
    const b = out[i]
    const c = out[i + 1]
    if (a && c && ((Math.abs(a[0] - b[0]) < 0.5 && Math.abs(b[0] - c[0]) < 0.5) || (Math.abs(a[1] - b[1]) < 0.5 && Math.abs(b[1] - c[1]) < 0.5))) {
      continue
    }
    res.push(b)
  }
  return res
}

/** 把一条折线画成带圆角的 SVG 路径。 */
export function roundedPath(pts: [number, number][], r = 8): string {
  if (pts.length < 2) return ''
  let d = `M${pts[0][0]},${pts[0][1]}`
  for (let i = 1; i < pts.length - 1; i++) {
    const [x0, y0] = pts[i - 1]
    const [x1, y1] = pts[i]
    const [x2, y2] = pts[i + 1]
    const d1 = Math.hypot(x1 - x0, y1 - y0)
    const d2 = Math.hypot(x2 - x1, y2 - y1)
    const rr = Math.min(r, d1 / 2, d2 / 2)
    const ax = x1 - ((x1 - x0) / (d1 || 1)) * rr
    const ay = y1 - ((y1 - y0) / (d1 || 1)) * rr
    const bx = x1 + ((x2 - x1) / (d2 || 1)) * rr
    const by = y1 + ((y2 - y1) / (d2 || 1)) * rr
    d += ` L${ax},${ay} Q${x1},${y1} ${bx},${by}`
  }
  const last = pts[pts.length - 1]
  d += ` L${last[0]},${last[1]}`
  return d
}
