#!/usr/bin/env node
/**
 * 把录到的原始数据(record.spec.ts 的输出)处理成能进仓库的固定数据。
 *
 *   node e2e/tools/sanitize.mjs <raw.json> > e2e/fixtures/api.json
 *
 * 录到的是 scratch 面板的真实响应:里面有测试机的公网地址、主机别名、REALITY 公钥、
 * 订阅 token、配置哈希。处理规则:
 *
 *   - 公网 IPv4 → 203.0.113.0/24 / 198.51.100.0/24(RFC 5737 文档地址),同一个地址
 *     始终映射到同一个,保住「这两处是同一台机器」这层关系;回环与私网地址不动
 *   - 公网 IPv6 → 2001:db8::/32
 *   - 名字像凭据的字段(公钥、short id、哈希、token、口令、指纹、uuid、identity_key……)→ 同长度、
 *     同字符集的确定性假值
 *   - 订阅地址里的 token → 假值;主机别名 → 中性名字(对照表在 sanitize.local.json)
 *   - 测试机的真实端口 → 虚构端口,数值字段与字符串里的独立数字都换(同上)
 *   - SSH 主机密钥指纹(`SHA256:` 开头,出现在部署步骤的详情里)→ 同形假值
 *
 * 处理完一定要人工 grep 一遍原始地址与别名,确认一个都没剩下(见 e2e/README.md)。
 */
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'

const [, , input] = process.argv
if (!input) {
  console.error('用法:node e2e/tools/sanitize.mjs <raw.json>')
  process.exit(1)
}
const raw = JSON.parse(readFileSync(input, 'utf8'))

/**
 * 测试机的真实端口与主机别名不写在这里 —— 这个文件要进公开仓库,把真实值列成一张对照表
 * 等于换个地方公开它们。它们放在同目录的 sanitize.local.json(已 gitignore),形状见
 * sanitize.local.example.json:
 *
 *   ports    真实端口 → 虚构端口(SSH、代理、API)。地址换成了文档地址段,而端口与历史里的
 *            旧截图拼在一起仍然能还原出管理通道,所以录制新的测试机时要把它的端口补进去
 *   aliases  主机别名 → 中性名字,大小写不敏感
 *
 * 没有这个文件时照样能跑,只是端口与别名一个都不换 —— 输出前会在 stderr 上说出来。
 */
const LOCAL = new URL('./sanitize.local.json', import.meta.url)
const local = existsSync(LOCAL) ? JSON.parse(readFileSync(LOCAL, 'utf8')) : null
if (!local) console.error('警告:没有 e2e/tools/sanitize.local.json,端口与主机别名不会被替换')
const PORTS = new Map(Object.entries(local?.ports ?? {}).map(([k, v]) => [Number(k), Number(v)]))
const ALIASES = Object.entries(local?.aliases ?? {})

const SECRET_KEY = /(public_?key|pubkey|private|short_?id|sha256|hash|token|password|passwd|psk|secret|fingerprint|uuid|access_?key|cookie|session|identity)/i

function digest(s) {
  return createHash('sha256').update(s).digest()
}

/** 同长度、同字符集的确定性假值。 */
function fake(value) {
  const h = digest(value)
  const hex = /^[0-9a-f]+$/i.test(value)
  const alphabet = hex
    ? '0123456789abcdef'
    : 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
  let out = ''
  for (let i = 0; i < value.length; i++) {
    const c = value[i]
    if (/[-_.:/+=]/.test(c)) {
      out += c
      continue
    }
    out += alphabet[h[i % h.length] % alphabet.length]
  }
  return out
}

const v4map = new Map()
function mapV4(ip) {
  const [a, b] = ip.split('.').map(Number)
  if (a === 127 || a === 10 || a === 0 || (a === 192 && b === 168) || (a === 172 && b >= 16 && b <= 31)) return ip
  if ((a === 203 && b === 0) || (a === 198 && b === 51) || (a === 192 && b === 0)) return ip
  if (!v4map.has(ip)) {
    const n = v4map.size + 10
    v4map.set(ip, n < 250 ? `203.0.113.${n}` : `198.51.100.${n - 240}`)
  }
  return v4map.get(ip)
}

const v6map = new Map()
function mapV6(ip) {
  const low = ip.toLowerCase()
  if (low === '::1' || low.startsWith('fe80') || low.startsWith('fd') || low.startsWith('fc') || low.startsWith('2001:db8')) return ip
  if (!v6map.has(low)) v6map.set(low, `2001:db8::${(v6map.size + 1).toString(16)}`)
  return v6map.get(low)
}

// 前面是 / 或 . 的不算(浏览器 UA 里的 Chrome/155.0.0.0)。
const V4 = /(?<![/\w.])(?:\d{1,3}\.){3}\d{1,3}\b/g
// 粗略的 IPv6:至少两个冒号、只含十六进制段。时间戳(12:34:56)不含字母也会被它看上,
// 所以要求至少有一段是 3~4 位十六进制。
const V6 = /\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b/gi

function scrubString(s, key) {
  if (key && SECRET_KEY.test(key) && s) return fake(s)
  let out = s
  out = out.replace(/\/sub\/[A-Za-z0-9_-]{8,}/g, (m) => `/sub/${fake(m.slice(5))}`)
  out = out.replace(/SHA256:[A-Za-z0-9+/]{40,}={0,2}/g, (m) => `SHA256:${fake(m.slice(7))}`)
  // 前后不能是字母数字或点:哈希、时间戳、版本号里碰巧出现的同一串数字不动。
  out = out.replace(/(?<![\w.])(\d{4,5})(?![\w.])/g, (m) => String(PORTS.get(Number(m)) ?? m))
  out = out.replace(V4, (m) => (m.split('.').every((x) => Number(x) <= 255) ? mapV4(m) : m))
  out = out.replace(V6, (m) => (/[0-9a-f]{3,4}/i.test(m) && /[a-f]/i.test(m) ? mapV6(m) : m))
  for (const [from, to] of ALIASES) out = out.replace(new RegExp(from, 'gi'), to)
  return out
}

function walk(v, key) {
  if (typeof v === 'string') return scrubString(v, key)
  if (typeof v === 'number' && key && /port/i.test(key)) return PORTS.get(v) ?? v
  if (Array.isArray(v)) return v.map((x) => walk(x, key))
  if (v && typeof v === 'object') {
    const out = {}
    for (const [k, x] of Object.entries(v)) out[scrubString(k)] = walk(x, k)
    return out
  }
  return v
}

const responses = walk(raw.responses)
process.stdout.write(JSON.stringify({ capturedAt: raw.capturedAt, responses }, null, 2) + '\n')
