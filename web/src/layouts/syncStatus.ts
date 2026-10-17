import type { TrafficStatus } from '@/api/client'

/**
 * 侧栏底部「流量同步」那一格说什么,规则只在这里。
 *
 * 失败是调度器在一轮中途逐台写入的,`last_run` 却要等整轮跑完才有 ——
 * 面板刚启动、第一轮里有机器连不上时,原来的写法会同时显示
 * 「N 个节点同步失败」与「上次 尚未运行」。两句都是真的,放在一起却像在说谎。
 * 这里按「有没有失败 / 有没有跑完过一轮 / 是不是正在跑」把它们说成一句话,
 * **失败永远不被盖成正常**:读不到状态、长时间没跑完一轮,也都不说「正常」。
 */
export type SyncTone = 'ok' | 'bad' | 'warn' | 'muted'

export interface SyncSummary {
  tone: SyncTone
  title: string
  /** 第二行的说明;`at` 有值时后面跟一个相对时间。 */
  detail: string
  at?: string
}

/** 超过这么久没有跑完一轮,就不再说「正常」。默认周期 60 秒,十几台机器各等一次超时也到不了这里。 */
export const SYNC_STALE_MS = 15 * 60 * 1000

function age(at: string | undefined, now: number): number | null {
  if (!at) return null
  const t = new Date(at).getTime()
  return Number.isNaN(t) ? null : now - t
}

export function summarizeSync(
  s: TrafficStatus | null,
  loadFailed: boolean,
  now: number = Date.now(),
): SyncSummary | null {
  if (!s) {
    return loadFailed ? { tone: 'warn', title: '同步状态读取失败', detail: '稍后自动重试' } : null
  }

  const failing = s.failing_nodes.length
  if (failing > 0) {
    const title = `${failing} 个节点同步失败`
    if (s.last_run) return { tone: 'bad', title, detail: '上次完成', at: s.last_run }
    if (s.running) return { tone: 'bad', title, detail: '首轮同步仍在进行' }
    const latest = s.failing_nodes
      .map((f) => f.failed_at)
      .filter((x): x is string => !!x)
      .sort()
      .pop()
    return { tone: 'bad', title, detail: latest ? '最近失败' : '面板启动后尚未跑完一轮', at: latest }
  }

  if (!s.last_run) {
    if (s.running) return { tone: 'muted', title: '首轮同步进行中', detail: '开始于', at: s.last_started }
    return { tone: 'muted', title: '等待首轮同步', detail: '面板启动后尚未跑完一轮' }
  }

  const sinceRun = age(s.last_run, now) ?? 0
  const sinceStart = age(s.last_started, now)
  const roundFresh = s.running && sinceStart !== null && sinceStart < SYNC_STALE_MS
  if (sinceRun > SYNC_STALE_MS && !roundFresh) {
    return { tone: 'warn', title: '流量同步可能已停滞', detail: '上次完成', at: s.last_run }
  }
  return { tone: 'ok', title: '流量同步正常', detail: '上次完成', at: s.last_run }
}

export const SYNC_DOT: Record<SyncTone, string> = {
  ok: 'var(--ok)',
  bad: 'var(--bad)',
  warn: 'var(--warn)',
  muted: 'var(--text3)',
}
