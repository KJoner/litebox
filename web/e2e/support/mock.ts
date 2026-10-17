import { test as base, expect, type Page, type Route } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { fixtureKey } from './key'

/**
 * 接口模拟层。所有用例都从这里的 `test` 起步,而不是直接用 @playwright/test 的。
 *
 * 三条规矩,缺一条都可能让测试去碰真东西或者测了个寂寞:
 *
 *   1. 页面发出的请求全部经过这里。同源的静态资源放行,/api/ 与 /sub/ 由固定数据回答,
 *      其余域名一律中止。
 *   2. 没模拟到的 GET 返回 404 并记进 `unmocked`;写操作(POST / PUT / PATCH / DELETE)
 *      没有显式 `api.on(...)` 就返回 501 并同样记下 —— 每个用例结束时断言 `unmocked`
 *      为空。点「确认」之后打到的永远是这一层,不会是一台真机器。
 *   3. 时钟固定在录制那一刻(`CAPTURED_AT`)。「3 分钟前」「采样过期」这类相对时间
 *      才与固定数据对得上,截图才稳定。
 */

export interface MockRequest {
  method: string
  path: string
  query: URLSearchParams
  body: unknown
}

export interface Reply {
  status?: number
  json?: unknown
  /** 非 JSON 响应体(订阅内容之类)。 */
  text?: string
  contentType?: string
  delayMs?: number
}

export type Responder = Reply | ((req: MockRequest) => Reply | Promise<Reply>)

interface FixtureFile {
  capturedAt: string
  responses: Record<string, unknown>
}

const read = (name: string) => JSON.parse(readFileSync(new URL(`../fixtures/${name}`, import.meta.url), 'utf8'))

/** 录到的(api.json,经 sanitize.mjs 脱敏)+ 手工补的(manual.json,同名以它为准)。 */
const FIXTURE: FixtureFile = (() => {
  const recorded = read('api.json') as FixtureFile
  const manual = read('manual.json') as { responses: Record<string, unknown> }
  return { capturedAt: recorded.capturedAt, responses: { ...recorded.responses, ...manual.responses } }
})()

/** 录制那一刻。所有用例的时钟都固定在这里。 */
export const CAPTURED_AT = FIXTURE.capturedAt

function clone<T>(v: T): T {
  return v === undefined ? v : JSON.parse(JSON.stringify(v))
}

export class ApiMock {
  /** 页面发出的全部 /api/ 与 /sub/ 请求,按时间先后。 */
  readonly calls: MockRequest[] = []
  /** 没有模拟到的请求。用例结束时必须为空。 */
  readonly unmocked: string[] = []

  private routes: { method: string; pattern: string | RegExp; responder: Responder }[] = []
  private data: Record<string, unknown> = clone(FIXTURE.responses)

  /**
   * 覆盖一个接口。`pattern` 是字符串时精确匹配路径;是正则时匹配「路径 + 查询串」。
   * 后注册的优先,所以用例可以在中途改口(比如先失败、点重试之后成功)。
   */
  on(method: string, pattern: string | RegExp, responder: Responder): this {
    this.routes.unshift({ method: method.toUpperCase(), pattern, responder })
    return this
  }

  /** 让一个接口失败。默认 500 + 面板错误格式 `{ error }`。 */
  fail(method: string, pattern: string | RegExp, status = 500, error = '模拟的服务器错误'): this {
    return this.on(method, pattern, { status, json: { error } })
  }

  /** 撤掉之前对某个接口的全部覆盖,回到固定数据。 */
  reset(method: string, pattern: string | RegExp): this {
    this.routes = this.routes.filter(
      (r) => !(r.method === method.toUpperCase() && String(r.pattern) === String(pattern)),
    )
    return this
  }

  /** 读一份固定数据(深拷贝),用来在它的基础上改出别的状态。 */
  fixture<T = any>(key: string): T {
    if (!(key in this.data)) throw new Error(`固定数据里没有 ${key}`)
    return clone(this.data[key]) as T
  }

  /** 改一份固定数据。之后的请求都拿到改过的版本。 */
  patch<T = any>(key: string, fn: (v: T) => T | void): this {
    const cur = this.fixture<T>(key)
    const next = fn(cur)
    this.data[key] = next === undefined ? cur : next
    return this
  }

  /** 页面发出的写操作。 */
  mutations(): MockRequest[] {
    return this.calls.filter((c) => c.method !== 'GET')
  }

  /** 打到某个接口的请求。 */
  requests(method: string, pattern: string | RegExp): MockRequest[] {
    return this.calls.filter((c) => c.method === method.toUpperCase() && matches(pattern, c))
  }

  async install(page: Page): Promise<void> {
    await page.route('**/*', (route) => this.handle(route))
  }

  private async handle(route: Route): Promise<void> {
    const req = route.request()
    const url = new URL(req.url())
    const local = url.hostname === 'localhost' || url.hostname === '127.0.0.1'
    if (!local) return route.abort('blockedbyclient')
    if (!url.pathname.startsWith('/api/') && !url.pathname.startsWith('/sub/')) return route.continue()

    let body: unknown = undefined
    const raw = req.postData()
    if (raw) {
      try {
        body = JSON.parse(raw)
      } catch {
        body = raw
      }
    }
    const call: MockRequest = { method: req.method(), path: url.pathname, query: url.searchParams, body }
    this.calls.push(call)

    const hit = this.routes.find((r) => r.method === call.method && matches(r.pattern, call))
    let reply: Reply
    if (hit) {
      reply = typeof hit.responder === 'function' ? await hit.responder(call) : hit.responder
    } else if (call.method === 'GET') {
      const found = this.lookup(url)
      if (found === undefined) {
        this.unmocked.push(fixtureKey('GET', url))
        reply = { status: 404, json: { error: `e2e:没有模拟 GET ${url.pathname}` } }
      } else {
        reply = { json: found }
      }
    } else {
      this.unmocked.push(fixtureKey(call.method, url))
      reply = { status: 501, json: { error: `e2e:没有模拟写操作 ${call.method} ${url.pathname}` } }
    }

    if (reply.delayMs) await new Promise((r) => setTimeout(r, reply.delayMs))
    if (reply.text !== undefined) {
      return route.fulfill({ status: reply.status ?? 200, contentType: reply.contentType ?? 'text/plain', body: reply.text })
    }
    if (reply.status === 204) return route.fulfill({ status: 204 })
    return route.fulfill({
      status: reply.status ?? 200,
      contentType: 'application/json',
      body: JSON.stringify(reply.json ?? {}),
    })
  }

  private lookup(url: URL): unknown {
    const exact = fixtureKey('GET', url)
    if (exact in this.data) return clone(this.data[exact])
    const bare = `GET ${url.pathname}`
    if (bare in this.data) return clone(this.data[bare])
    // 同一路径换了查询参数(区间、条数),回落到录到的任意一份。
    const any = Object.keys(this.data).find((k) => k.startsWith(`${bare}?`))
    return any ? clone(this.data[any]) : undefined
  }
}

function matches(pattern: string | RegExp, req: { path: string; query: URLSearchParams }): boolean {
  if (typeof pattern === 'string') return req.path === pattern
  const qs = req.query.toString()
  return pattern.test(qs ? `${req.path}?${qs}` : req.path)
}

export const test = base.extend<{ api: ApiMock }>({
  // auto:用例没写 api 参数也一样装上拦截层 —— 漏写一个参数不该让页面直接去连后端。
  api: [
    async ({ page }, use) => {
      const api = new ApiMock()
      await page.clock.setFixedTime(new Date(CAPTURED_AT))
      await api.install(page)
      await use(api)
      expect(api.unmocked, '有接口没有被模拟(或者有未经确认的写操作)').toEqual([])
    },
    { auto: true },
  ],
})

export { expect }

/** 以未登录身份打开(管理端与门户的 me 都回 401)。 */
export function signedOut(api: ApiMock): ApiMock {
  return api
    .on('GET', '/api/auth/me', { status: 401, json: { error: '未登录' } })
    .on('GET', '/api/portal/auth/me', { status: 401, json: { error: '未登录' } })
}

/** 只登录了管理端。门户的 me 回 401。 */
export function adminOnly(api: ApiMock): ApiMock {
  return api.on('GET', '/api/portal/auth/me', { status: 401, json: { error: '未登录' } })
}

/** 只登录了门户。管理端的 me 回 401。 */
export function portalOnly(api: ApiMock): ApiMock {
  return api.on('GET', '/api/auth/me', { status: 401, json: { error: '未登录' } })
}
