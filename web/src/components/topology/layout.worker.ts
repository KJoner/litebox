/// <reference lib="webworker" />
import { layout, type LEdge, type LNode } from './layout'

// 布局在 Worker 里算,几百张卡片也不卡住整个管理端。只收卡片尺寸与边,不收任何业务数据。
self.onmessage = (ev: MessageEvent<{ seq: number; nodes: LNode[]; edges: LEdge[] }>) => {
  const { seq, nodes, edges } = ev.data
  try {
    const started = performance.now()
    const result = layout(nodes, edges)
    ;(self as unknown as Worker).postMessage({ seq, result, ms: performance.now() - started })
  } catch (e) {
    ;(self as unknown as Worker).postMessage({ seq, error: e instanceof Error ? e.message : String(e) })
  }
}
