import { computed, reactive, type Ref } from 'vue'
import type { ProbeMetric, PublicProbeTarget } from '@/api/client'
import { ispText, metricLabel, originText } from '@/components/asset/probeView'

/**
 * 公共目标的筛选:地区、省份、城市、运营商、来源、协议方式、地址族、状态、关键字。
 * 目标库页、节点详情的目标选择器、节点列表的批量应用三处共用 —— 各写一份的话,
 * 同一个筛选条件在三处筛出来的不一样。整个目标库最多一千项,筛选在页面上做。
 */
export interface TargetFilter {
  q: string
  source: string
  region: string
  province: string
  city: string
  isp: string
  metric: '' | ProbeMetric
  family: string
  /** '' / active(至少一台在测)/ idle(没有对任何节点生效)/ paused(目标或来源暂停、未授权)/ missing(上游已消失) */
  status: string
}

export function emptyFilter(): TargetFilter {
  return { q: '', source: '', region: '', province: '', city: '', isp: '', metric: '', family: '', status: '' }
}

export function matchTarget(p: PublicProbeTarget, f: TargetFilter): boolean {
  if (f.source && p.source !== f.source) return false
  if (f.region && p.region !== f.region) return false
  if (f.province && p.province !== f.province) return false
  if (f.city && p.city !== f.city) return false
  if (f.isp && p.isp !== f.isp) return false
  if (f.metric && p.metric !== f.metric) return false
  if (f.family && p.family !== f.family) return false
  switch (f.status) {
    case 'active':
      if (!p.node_count) return false
      break
    case 'idle':
      if (p.node_count || p.pause_reason) return false
      break
    case 'paused':
      if (!p.pause_reason) return false
      break
    case 'missing':
      if (!p.upstream_missing_since) return false
      break
  }
  const q = f.q.trim().toLowerCase()
  if (q) {
    const hay = [p.name, p.host, p.province, p.city, p.region, p.note, ispText(p.isp)].join(' ').toLowerCase()
    if (!hay.includes(q)) return false
  }
  return true
}

function distinct(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))].sort((a, b) => a.localeCompare(b, 'zh-CN'))
}

/** 一组筛选状态加筛选结果与各下拉的可选项(可选项随上一级收窄:选了省份,城市只列这个省的)。 */
export function useTargetFilter(items: Readonly<Ref<PublicProbeTarget[]>>) {
  const filter = reactive<TargetFilter>(emptyFilter())
  const filtered = computed(() => items.value.filter((p) => matchTarget(p, filter)))
  const options = computed(() => {
    const all = items.value
    const inProvince = filter.province ? all.filter((p) => p.province === filter.province) : all
    return {
      source: distinct(all.map((p) => p.source)).map((v) => ({ value: v, label: originText(v) })),
      region: distinct(all.map((p) => p.region)).map((v) => ({ value: v, label: v })),
      province: distinct(all.map((p) => p.province)).map((v) => ({ value: v, label: v })),
      city: distinct(inProvince.map((p) => p.city)).map((v) => ({ value: v, label: v })),
      isp: distinct(all.map((p) => p.isp)).map((v) => ({ value: v, label: ispText(v) || v })),
      metric: distinct(all.map((p) => p.metric)).map((v) => ({ value: v, label: metricLabel(v as ProbeMetric) })),
      family: distinct(all.map((p) => p.family)).map((v) => ({ value: v, label: v === 'V6' ? 'IPv6' : 'IPv4' })),
    }
  })
  const active = computed(() => Object.entries(filter).some(([, v]) => v !== ''))
  function reset() {
    Object.assign(filter, emptyFilter())
  }
  return { filter, filtered, options, active, reset }
}
