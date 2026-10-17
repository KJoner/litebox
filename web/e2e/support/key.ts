/** 固定数据的键:方法 + 路径 + 排过序的查询串。录制与回放共用这一份写法。 */
export function fixtureKey(method: string, url: URL): string {
  const q = [...url.searchParams.entries()].sort(([a], [b]) => a.localeCompare(b))
  const qs = new URLSearchParams(q).toString()
  return `${method} ${url.pathname}${qs ? `?${qs}` : ''}`
}
