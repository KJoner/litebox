import { layout, type LayoutResult, type LEdge, type LNode } from './layout'

/**
 * 在 Worker 里跑布局,超时或失败时报错(调用方保留上一次可用的视图并给出重试)。
 * 浏览器不让开 Worker 时(老内核、特殊环境)退回主线程 —— 小规模图上那也只是几毫秒。
 */
let worker: Worker | null = null
let seq = 0
const waiting = new Map<number, { resolve: (r: { result: LayoutResult; ms: number }) => void; reject: (e: Error) => void }>()

function ensureWorker(): Worker | null {
  if (worker) return worker
  try {
    worker = new Worker(new URL('./layout.worker.ts', import.meta.url), { type: 'module' })
    worker.onmessage = (ev: MessageEvent<{ seq: number; result?: LayoutResult; ms?: number; error?: string }>) => {
      const w = waiting.get(ev.data.seq)
      if (!w) return
      waiting.delete(ev.data.seq)
      if (ev.data.error || !ev.data.result) w.reject(new Error(ev.data.error ?? '布局失败'))
      else w.resolve({ result: ev.data.result, ms: ev.data.ms ?? 0 })
    }
    worker.onerror = () => {
      for (const w of waiting.values()) w.reject(new Error('布局线程出错'))
      waiting.clear()
      worker?.terminate()
      worker = null
    }
  } catch {
    worker = null
  }
  return worker
}

export function runLayout(nodes: LNode[], edges: LEdge[], timeoutMs = 8000): Promise<{ result: LayoutResult; ms: number }> {
  const w = ensureWorker()
  if (!w) {
    const started = performance.now()
    return Promise.resolve({ result: layout(nodes, edges), ms: performance.now() - started })
  }
  const id = ++seq
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (waiting.delete(id)) reject(new Error(`布局超过 ${Math.round(timeoutMs / 1000)} 秒还没算完`))
    }, timeoutMs)
    waiting.set(id, {
      resolve: (r) => {
        clearTimeout(timer)
        resolve(r)
      },
      reject: (e) => {
        clearTimeout(timer)
        reject(e)
      },
    })
    w.postMessage({ seq: id, nodes, edges })
  })
}
