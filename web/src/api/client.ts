// API 客户端。会话通过 HttpOnly Cookie 传递,因此所有请求都要带 credentials。

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    /** 面板返回的 JSON 响应体(有的话)。409 这类带 code / check 的冲突要靠它分辨。 */
    public readonly payload?: unknown,
  ) {
    super(message)
    this.name = 'ApiError'
  }

  /** 响应体里的 code 字段,没有则为空串。 */
  get code(): string {
    const p = this.payload
    return p && typeof p === 'object' && 'code' in p ? String((p as { code: unknown }).code) : ''
  }
}

// describeNonJSON 把一个非 JSON 响应翻译成能直接照着排查的话。
function describeNonJSON(response: Response, text: string): string {
  const snippet = text.trim().replace(/\s+/g, ' ').slice(0, 120)
  const where = `HTTP ${response.status} ${response.headers.get('content-type') ?? '无 Content-Type'}`

  if (response.status === 404 && /404 page not found/i.test(text)) {
    return `接口不存在(${where})。这通常是前端比后端新 —— 页面上有这个按钮,但正在运行的 litebox 里没有这个接口。请重启/升级主控服务后刷新页面。`
  }
  if (response.status === 502 || response.status === 503 || response.status === 504) {
    return `反向代理没能拿到面板的响应(${where})。可能是主控服务崩了或超时,看 journalctl -u litebox -n 50。`
  }
  return `服务器返回了非 JSON 响应(${where}):${snippet || '(空)'}`
}

interface RequestOptions {
  method?: string
  body?: unknown
  query?: Record<string, string | number | undefined>
}

/**
 * 会话失效时的处理(清登录状态、送回对应的登录页、登录完回到原来的页面),由路由那一侧注册。
 *
 * 判据是后端认证中间件给的 code = UNAUTHENTICATED,不是光看 401:改密码时原密码输错、登录时凭据不对
 * 同样是 401,按状态码一刀切的话,输错一次原密码就被踢回登录页。
 */
export type SessionScope = 'admin' | 'portal'
let sessionExpiredHandler: ((scope: SessionScope) => void) | null = null
export function setSessionExpiredHandler(fn: ((scope: SessionScope) => void) | null): void {
  sessionExpiredHandler = fn
}

/** 这几个接口本身就是在问「登没登录」或在登入登出:401 是它们的正常结果,由路由守卫与登录页自己处理。 */
const SESSION_PROBES = new Set([
  '/api/auth/me',
  '/api/auth/login',
  '/api/auth/logout',
  '/api/portal/auth/me',
  '/api/portal/auth/login',
  '/api/portal/auth/logout',
])

export const SESSION_EXPIRED_TEXT = '登录已失效,请重新登录'

/** 这个错误是不是「会话没了」(而不是别的 401)。 */
export function isSessionExpired(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401 && err.code === 'UNAUTHENTICATED'
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query } = options

  let url = path
  if (query) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) params.set(key, String(value))
    }
    const qs = params.toString()
    if (qs) url += `?${qs}`
  }

  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }

  let response: Response
  try {
    response = await fetch(url, init)
  } catch (e) {
    // fetch 只在【根本没拿到响应】时 reject:网络断了、连接被重置、
    // 反向代理在响应到达之前超时。status 用 0 表示"从未收到响应",
    // 调用方(尤其是节点长操作的进度弹窗)据此把话说清楚 —— 那些操作
    // 在后端与请求 ctx 解绑,这一侧断开并不会中止它们。
    throw new ApiError(0, `无法连接到面板(${e instanceof Error ? e.message : '网络错误'})`)
  }
  if (response.status === 204) return undefined as T

  let payload: unknown
  const text = await response.text()
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      // 面板自己的所有响应(含错误)都是 JSON,走到这里说明响应根本不是面板发的:
      // 404 纯文本(路由不存在,通常是前后端版本不一致)、反代的 HTML 错误页,
      // 或者被中间设备拦了。状态码和响应体开头是唯一的线索,必须带出去 ——
      // 只说一句"无法解析"等于把排查所需的信息全丢了。
      throw new ApiError(response.status, describeNonJSON(response, text))
    }
  }

  if (!response.ok) {
    const message =
      payload && typeof payload === 'object' && 'error' in payload
        ? String((payload as { error: unknown }).error)
        : `请求失败(HTTP ${response.status})`
    const err = new ApiError(response.status, message, payload)
    if (isSessionExpired(err)) {
      // 页面上各处拿到这个错误多半会弹一条吐司:说法统一成「登录已失效」,
      // 与随后的登录页提示是一回事,而不是一句让人以为出了故障的「会话无效」。
      const expired = new ApiError(401, SESSION_EXPIRED_TEXT, payload)
      if (!SESSION_PROBES.has(path)) sessionExpiredHandler?.(path.startsWith('/api/portal/') ? 'portal' : 'admin')
      throw expired
    }
    throw err
  }
  return payload as T
}

// ---------- 类型 ----------

export interface Admin {
  id: number
  username: string
}

export interface DashboardSummary {
  user_total: number
  user_active: number
  node_total: number
  node_online: number
  traffic_today: number
  traffic_month: number
  quota_exceeded: number
  expiring_soon: number
  failed_deploys: number
}

export interface AuditLog {
  id: number
  admin_user_id: number | null
  action: string
  target_type: string
  target_id: string
  detail: string
  client_ip: string
  succeeded: boolean
  created_at: string
}

export type UserStatus =
  | 'ACTIVE'
  | 'DISABLED'
  | 'EXPIRED'
  | 'QUOTA_EXCEEDED'
  | 'DEPLOY_PENDING'
  | 'DEPLOY_FAILED'

export interface AccessTier {
  id: number
  /** normal / vip / root,程序内一律用它判断,不要用 name */
  code: string
  name: string
  /** 节点 level <= 用户 level 即可用 */
  level: number
  description: string
  sort_order: number
  created_at: string
  updated_at: string
}

export interface ProxyUser {
  id: number
  user_code: string
  display_name: string
  remark: string
  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number
  status: UserStatus
  quota_bytes: number
  used_uplink: number
  used_downlink: number
  used_total: number
  expires_at: string | null
  reset_cycle: 'NONE' | 'MONTHLY'
  reset_day: number
  last_reset_at: string | null
  /** 管理员单独追加的节点,编辑页面改的就是它 */
  node_ids: number[]
  /** 等级继承 + 额外授权合并后的实际可用节点 */
  effective_node_ids: number[]
  created_at: string
  updated_at: string
  sub_last_access_at: string | null
  sub_last_access_ip: string
  sub_last_user_agent: string
  sub_access_count: number
  /** 门户登录账号,为 null 表示该用户未开通门户登录 */
  portal_account: PortalAccount | null
  /** 仅新建用户时可能出现:用户已建好但登录账号没建成 */
  portal_account_error?: string
  /** 最近一次加流量或延期限的时间,空串表示从未续期 */
  last_renewal_at: string
  /**
   * 流量的下次重置时刻,不重置的用户为空串。
   * 由后端算好下发 —— 与门户上给用户看的是同一份计算,前端不自己推。
   */
  next_reset_at: string
  // 仅详情接口返回
  uuid?: string
  sub_token?: string
  subscription_url?: string
}

export type NodeStatus = 'PENDING' | 'ONLINE' | 'OFFLINE' | 'DISABLED' | 'DEPLOY_FAILED'

/**
 * VPS 商计量这台机器流量的口径。
 *
 * sing-box 计的是客户端↔节点这一段的双向字节,而一次用户下载在网卡上要走两趟
 * (从源站收一份、发给客户端一份)。所以进出合计计费的机器,商家看到的数字
 * 约是 sing-box 计数的两倍。折算由后端做,前端只负责选和显示。
 */
export type NodeBillingMode = 'EGRESS' | 'BOTH'

/**
 * 节点的落地协议。
 *
 * VLESS_REALITY 需要握手目标、REALITY 密钥与 short_id;
 * SHADOWSOCKS 用 2022 系列的加密方法与两级 PSK,不用 REALITY。
 * 一个节点只跑一种 —— 表单按它决定显示哪一组字段。
 */
export type NodeProtocol = 'VLESS_REALITY' | 'SHADOWSOCKS' | 'SNELL'

/** Snell 的服务端版本。5 走 HTTP/TLS 混淆,6 走流量整形。 */
export type SnellVersion = 5 | 6
export type SnellObfsMode = '' | 'none' | 'http' | 'tls'
export type SnellV6Mode = '' | 'default' | 'unshaped' | 'unsafe-raw'

/** Shadowsocks 2022 的加密方法。只有这三种,不收传统 AEAD。 */
export type NodeSSMethod =
  | '2022-blake3-aes-128-gcm'
  | '2022-blake3-aes-256-gcm'
  | '2022-blake3-chacha20-poly1305'

export const PROTOCOL_LABEL: Record<NodeProtocol, string> = {
  VLESS_REALITY: 'VLESS + REALITY',
  SHADOWSOCKS: 'Shadowsocks 2022',
  SNELL: 'Snell',
}

/** Snell 版本的说明。只在这里写一遍。 */
export const SNELL_VERSION_LABEL: Record<number, string> = {
  5: 'v5 —— HTTP/TLS 混淆',
  6: 'v6 —— 流量整形(推荐)',
}

export const SNELL_OBFS_LABEL: Record<string, string> = {
  none: '不混淆',
  http: 'HTTP 混淆',
  tls: 'TLS 混淆',
}

export const SNELL_V6_MODE_LABEL: Record<string, string> = {
  default: '默认整形',
  unshaped: '不整形(更快,特征更明显)',
  'unsafe-raw': '原始(上游标注 unsafe)',
}

/**
 * 加密方法的说明。**只在这里写一遍** —— 建节点表单与入口表单各抄一份的话,
 * 某天改了措辞只改到一处,而两处说的是同一件事。
 *
 * 只提供 2022 系列:传统的 aes-128-gcm 那几种没有 replay 防护,
 * 多用户也要逐个试解密。
 */
export const SS_METHOD_LABEL: Record<NodeSSMethod, string> = {
  '2022-blake3-aes-128-gcm': 'AES-128-GCM —— 默认,低配机器上更快',
  '2022-blake3-aes-256-gcm': 'AES-256-GCM',
  '2022-blake3-chacha20-poly1305': 'ChaCha20-Poly1305 —— 无 AES 硬件加速的老 ARM',
}

/** 列表里的短标记。全称太长,会把展示名挤到换行。 */
export const PROTOCOL_SHORT: Record<NodeProtocol, string> = {
  VLESS_REALITY: 'VLESS',
  SHADOWSOCKS: 'SS2022',
  SNELL: 'Snell',
}

/**
 * 节点角色。
 *
 * LANDING 落地节点 —— V7 之前的全部节点。
 * RELAY   纯中转机 —— 上面不跑 sing-box,只跑 nginx。它没有自己的协议、
 *         端口与用户,也**不产生任何流量数字**(nginx 不接 V2Ray API)。
 *
 * 一经创建不可更改:两个方向都等于"删了重建",而重建会丢掉这台机器的
 * 全部历史数据。
 */
export type NodeRole = 'LANDING' | 'RELAY'

export const NODE_ROLE_LABEL: Record<NodeRole, string> = {
  LANDING: '落地',
  RELAY: '中转',
}

/**
 * 链式出站的落地去向。空串表示本机直连。
 *
 * INBOUND 指的是【落地机器上的某一个入站】而不是整台机器:一台机器上有
 * 两个入口时,「转发到 B」是有歧义的,而歧义的表现是流量进了管理员没打算
 * 用的那个入口(协议、端口、等级都不同),没有任何一层会报错。
 */
export type ChainTargetKind = '' | 'INBOUND' | 'EXTERNAL'

/**
 * 一台落地机器上的一个 sing-box 入站(V8 多入站)。
 *
 * V8 之前这些字段直接挂在 Node 上,因为那时一台机器只有一个入站。
 * 协议、端口、REALITY、TFO、访问等级与出口去向全部降到这一层 ——
 * 同一台机器上的两个入口可以完全不同。
 *
 * **流量拆不到入口。** V2Ray 的用户计数器名里没有入站维度,同一用户在
 * 同一台机器上的流量是所有入口的合计。按节点看的数字仍然完全正确,
 * 但「这个人在 8443 那个入口用了多少」这个问题答不了。
 */
/** Mieru 的传输层。取值与 mita 配置、mihomo 配置里的写法一致,不翻译。 */
export type MieruTransport = 'TCP' | 'UDP'

/** Mieru 的多路复用档位。档位越高越省握手,也越容易在流量特征上聚成一团。 */
export type MieruMultiplexing =
  | 'MULTIPLEXING_OFF'
  | 'MULTIPLEXING_LOW'
  | 'MULTIPLEXING_MIDDLE'
  | 'MULTIPLEXING_HIGH'

/**
 * 一台机器上的一个 Mieru 入口。
 *
 * 它与 NodeInbound 是**两类东西**:服务端是另一个进程(mita),
 * 凭据与流量走另一条通道。界面上两者并成一张列表,但操作摩擦各走各的。
 *
 * **端口在这里是一段而不是一个数** —— 多端口跳跃是 mieru 的主要抗封锁特性。
 * 起止相等表示只有一个端口。
 */
export interface MieruInbound {
  id: number
  node_id: number
  node_name: string
  node_display_name: string
  display_name: string

  /** mita 实际监听的那一段。 */
  listen_port_start: number
  listen_port_end: number
  /** IPv4 条目在订阅里用的那一段,两端都是 0 表示跟随监听段。 */
  public_port_start: number
  public_port_end: number
  /** IPv6 条目用的那一段,两端都是 0 表示跟随 IPv4 公网段。 */
  ipv6_public_port_start: number
  ipv6_public_port_end: number

  ipv6_enabled: boolean
  ipv6_display_name: string
  /** 派生:IPv6 条目在订阅里实际显示的名字。前端渲染它,不自己拼后缀。 */
  ipv6_entry_name: string

  transport: MieruTransport
  multiplexing: MieruMultiplexing
  /** 0 表示用 mieru 自己的默认值。 */
  mtu: number

  /**
   * 节点上【当前正在生效】的那几项。订阅只看它们 ——
   * deployed_transport 为空表示这个入口还没真正上过节点。
   */
  deployed_transport: MieruTransport | ''
  deployed_multiplexing: MieruMultiplexing | ''
  deployed_mtu: number
  deployed_listen_port_start: number
  deployed_listen_port_end: number

  /**
   * mita 与本机 sing-box 之间那一跳的回环端口。0 表示这个入口是**直连**的。
   *
   * 那一跳是协议限制逼出来的:mita 的出口代理只认 SOCKS5,
   * 拨不出 VLESS 或 Shadowsocks。
   */
  egress_socks_port: number
  /** 出口去向。空串表示直连。 */
  chain_target_kind: '' | 'INBOUND' | 'EXTERNAL'
  chain_target_inbound_id: number
  chain_target_external_id: number
  /** 这条链路在【落地】的流量统计里的计数器名。凭据本身永不下发到前端。 */
  chain_code: string

  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number

  sort_order: number
  subscription_enabled: boolean
  public_remark: string
  enabled: boolean

  created_at: string
  updated_at: string
}

/** 一次按服务的安装/卸载的结果。 */
export interface ServiceOpResult {
  /** singbox / mieru / nginx 之一 */
  service: string
  /** 逐步的可读结果,失败的那一步在最后 */
  steps: string[]
  detail: string
}

/** 一次 Mieru 二进制安装的结果。 */
export interface MieruInstallResult {
  mita_path: string
  mita_sha256: string
  client_path: string
  client_sha256: string
  /** 装完真的跑了一次 `mita version` 拿到的 —— 只上传不验证的话,架构不对
   *  这类问题要等到第一次下发才暴露,而那时管理员已经以为装好了。 */
  mita_version: string
}

/** V16:一台机器的额外订阅地址(host 之外)。host 是管理地址,不在这个列表里。 */
export interface NodeAddress {
  id: number
  node_id: number
  family: 'V4' | 'V6'
  address: string
  sort_order: number
}
export interface NodeAddressInput {
  /** 0 或缺省表示新增 */
  id?: number
  family: 'V4' | 'V6'
  address: string
}

export type EndpointKind = 'SINGBOX' | 'MIERU' | 'NGINX' | 'REALM'

/** V16:一个入口在订阅里下发的一条地址。address_id 为 null 表示管理 IP(host)。 */
export interface InboundEndpoint {
  id: number
  address_id: number | null
  /** 0 表示跟随监听端口(单端口类);Mieru 是段起点 */
  public_port: number
  /** 仅 Mieru:段终点,0 表示跟随 */
  public_port_end: number
  /** 空表示跟随入口名(V6 加 -IPV6 后缀) */
  display_name: string
  sort_order: number
}
export interface InboundEndpointInput {
  address_id: number | null
  public_port: number
  public_port_end: number
  display_name: string
}

export interface NodeInbound {
  id: number
  node_id: number
  /** 所属机器的内部名称与展示名称,只给后台用 */
  node_name: string
  node_display_name: string
  /**
   * sing-box 配置里的 inbound.tag,建库时分配、一经分配不可更改。
   * 它同时是入站级流量计数器的名字。
   */
  tag: string
  /** 订阅与门户里显示的名字 */
  display_name: string
  /** 期望协议。改了它必须重新部署,部署成功前订阅仍下发旧协议的条目。 */
  protocol: NodeProtocol
  /** 只在 SHADOWSOCKS 下有值 */
  ss_method: NodeSSMethod | ''
  /**
   * 节点上【已经生效】的协议,只在部署成功时写入。空串表示这个入口
   * 还没真正上过节点 —— 订阅据此过滤,而机器级的部署状态答不了这个问题。
   */
  deployed_protocol: NodeProtocol | ''
  deployed_ss_method: NodeSSMethod | ''
  /** 以下五项只在 SNELL 下有值。snell_version 为 0 表示这不是 Snell 入口。 */
  snell_version: number
  snell_obfs_mode: SnellObfsMode
  /**
   * 混淆时客户端伪装的 Host。**它不进节点配置** —— 服务端没有这个字段,
   * 所以改它不用重新部署,只影响下一次生成的订阅。
   */
  snell_obfs_host: string
  snell_v6_mode: SnellV6Mode
  /**
   * 为真时这个入口**不下发逐用户凭据**,所有人共用 psk。
   *
   * 唯一的理由是让 Clash / mihomo 能用 —— 它们的 snell proxy 没有
   * userkey 那一栏。代价:没有分用户流量、撤销一个人要换 psk
   * (所有人一起断)、用户额度对它不生效。强制版本 5。
   */
  snell_shared_psk: boolean
  /** 节点上【已经生效】的那两项,订阅只看它们,理由同 deployed_protocol。 */
  deployed_snell_version: number
  deployed_snell_obfs_mode: SnellObfsMode
  deployed_snell_v6_mode: SnellV6Mode
  deployed_snell_shared_psk: boolean
  /**
   * 不计流量(V15):用户凭据照旧下发,只是不写 name,于是没有计数器 ——
   * 不计入用户额度,也不计入这台机器的周期用量。三种协议都适用。
   */
  unmetered: boolean
  deployed_unmetered: boolean
  /** sing-box 真正 bind 的端口 */
  listen_port: number
  /** 客户端连接的公网端口。0 表示跟随 listen_port。 */
  public_port: number
  /** IPv6 条目用的公网端口。0 表示跟随 public_port。 */
  ipv6_public_port: number
  /** 这个入口在订阅里要不要多出一条 IPv6 条目。机器没填 IPv6 地址时它没有意义。 */
  ipv6_enabled: boolean
  /** IPv6 条目的独立名称。空串表示跟随 IPv4 名字。**渲染时不要用它**。 */
  ipv6_display_name: string
  /**
   * IPv6 条目在订阅里【实际】显示的名字,后端已经算好回落。
   *
   * 渲染一律用它,不要自己拼 `display_name + '-IPV6'` —— 回落只有
   * subscription.IPv6EntryName 一处实现,前端再拼一遍的话,某天改了规则
   * 只会改到一边,表现是面板上显示的名字与用户客户端里那条对不上。
   */
  ipv6_entry_name: string
  tcp_fast_open: boolean
  /** 节点上【已经生效】的 TFO。订阅只看它,理由同 deployed_protocol。 */
  deployed_tcp_fast_open: boolean
  reality_dest: string
  reality_dest_port: number
  reality_public_key: string
  reality_short_id: string
  handshake_max_record_size: number
  handshake_checked_at: string | null
  chain_target_kind: ChainTargetKind
  chain_target_inbound_id: number
  chain_target_external_id: number
  /** 链路凭据在落地那台机器的流量统计里的计数器名。空串表示还没分配过。 */
  chain_code: string
  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number
  sort_order: number
  /** 关掉后这个入口不再进新生成的订阅,但它仍然在节点上运行 */
  subscription_enabled: boolean
  public_remark: string
  /** 关掉后这个入站不再渲染进 sing-box 配置(下次部署生效) */
  enabled: boolean
  created_at: string
  updated_at: string
}

/** 新增或编辑一个入口的请求体。新增与编辑收同一份字段。 */
export interface NodeInboundInput {
  display_name: string
  protocol: NodeProtocol
  ss_method?: NodeSSMethod | ''
  /**
   * 以下四项只在 SNELL 下提交。
   *
   * **snell_version 留 0 表示「保持原值」**,不是「这不再是 Snell 入口」——
   * 与 access_tier_id 同一个约定。编辑表单必须回填它,漏了的话后端会
   * 沿用原值(那是兜底),但表单上显示的与提交的就对不上了。
   */
  snell_version?: number
  snell_obfs_mode?: SnellObfsMode
  snell_obfs_host?: string
  snell_v6_mode?: SnellV6Mode
  /** 共享凭据。漏传 = 关掉,而关掉是更严的那一档,所以它是普通布尔。 */
  snell_shared_psk?: boolean
  /** 不计流量。同一种约定:漏传 = 关掉(计量)。 */
  unmetered?: boolean
  listen_port: number
  public_port: number
  ipv6_public_port?: number
  /** null 表示保持原值(新增时默认开)。 */
  ipv6_enabled?: boolean
  /** 空串表示「跟随 IPv4 名字」,**不是**「保持原值」—— 编辑时必须回填。 */
  ipv6_display_name?: string
  tcp_fast_open: boolean
  reality_dest?: string
  reality_dest_port?: number
  access_tier_id?: number
  sort_order?: number
  subscription_enabled?: boolean
  enabled?: boolean
  public_remark?: string
}

export interface Node {
  id: number
  /** 内部名称,只在管理后台出现 */
  name: string
  /** 对用户与订阅展示的名称 */
  display_name: string
  /**
   * **机器没有访问等级**(迁移 0020)。等级是入口的属性 ——
   * 机器本身不接受任何连接,入口才接受。见 NodeInbound.access_tier_id。
   *
   * user_nodes 的额外授权仍然是机器级的:它的意思就是「这台机器给他用」,
   * 穿透入口等级。
   */
  sort_order: number
  /** 关掉后不再下发到新生成的订阅,节点与历史数据保留 */
  subscription_enabled: boolean
  public_remark: string
  maintenance_message: string
  /**
   * 管理地址:SSH、探测、部署、流量同步与资源采集一律走它。
   * sub_ipv4_address 留空时它同时兼任 IPv4 订阅地址。
   */
  host: string
  /**
   * 可选的订阅 IPv4,只影响订阅:留空表示 IPv4 条目跟随 host。
   * 填了它之后「面板连的地址」与「用户连的地址」就此分开 ——
   * 前面挂了一层 IP 转发、或者管理口上根本没开代理端口时才需要。
   * 面板一次都不解析它。
   */
  sub_ipv4_address: string
  /**
   * 这台机器**在订阅里**用的 IPv4 地址:填了 sub_ipv4_address 就是它,
   * 留空就是 host。由后端算好下发 —— 回落只有一处实现,
   * 前端渲染它、不自己写那个 ||(与 udp_timeout、next_reset_at 同一条规矩)。
   */
  subscription_host: string
  /**
   * 可选的公网 IPv6,只影响订阅:填了之后这台机器上每个开着 IPv6 条目的入口
   * 都会多下发一条。条目名由入口的 ipv6_entry_name 给出。
   */
  ipv6_address: string
  /**
   * 0 表示不限量。只用于统计与预警,超额不会自动停服。
   * 按**主机计费口径**计,也就是 VPS 商账单上的那个数字。
   */
  traffic_quota_bytes: number
  traffic_reset_cycle: 'NONE' | 'MONTHLY'
  traffic_reset_day: number
  traffic_billing_mode: NodeBillingMode
  ssh_port: number
  ssh_user: string
  /**
   * V2Ray API 的回环端口,全部入站共用一个 —— 一台机器上只有一个
   * sing-box 进程,也就只有一个 API 端点。
   */
  api_port: number
  /**
   * 这台机器上的 sing-box 入口。中转角色恒为空数组。
   *
   * 协议、端口、REALITY、TFO、访问等级与出口去向都在这里,不在 Node 上 ——
   * V8 之前它们是节点的属性,那时一台机器只有一个入站。
   */
  inbounds: NodeInbound[]
  /**
   * 这台机器上的 Mieru 入口。中转角色恒为空数组。
   *
   * 与 inbounds 分成两个数组而不是合成一个:两类入口的服务端是两个进程,
   * 参数与部署方式都不一样,合成一个会让每一处消费者都要先判断
   * "这一项是哪一类"。界面上的合并由入口面板做。
   */
  mieru_inbounds: MieruInbound[]
  /** 配置与备份放在内存文件系统里,磁盘上不留。机器重启后靠巡检重新下发 */
  config_in_ram: boolean
  arch: string
  /**
   * 探测与安装时在节点上读出来的版本,空串表示没装或还没探测过。
   * 这台机器能建哪些协议由后端按它算好(available_protocols),前端不自己比。
   */
  singbox_version: string
  singbox_build_tags: string
  /**
   * 探测到的节点内存,0 表示还没探测过。
   *
   * 它决定入站的 udp_timeout(小内存机器压短 UDP 会话的驻留时间),
   * 所以第一次探测之后节点可能会变成「待部署」—— 那正是这一项要生效。
   */
  mem_total_mb: number
  role: NodeRole
  status: NodeStatus
  last_heartbeat_at: string | null
  config_revision: number
  deployed_config_sha256: string
  /**
   * 按 mem_total_mb 算出来的 UDP 会话超时,空串表示用 sing-box 的默认值(5m)。
   * 它是【机器】的属性,全部入口共用同一个值。
   *
   * 后端算好下发,前端不按内存自己推 —— 分档边界只能有一处实现,
   * 各算一遍会在某个内存刚好卡在边界上的节点上分叉,而两边都不报错。
   */
  udp_timeout: string
  /**
   * 这台机器【现在】能建的入站协议,后端按 singbox_version 算好。
   *
   * 前端渲染这个列表,**不自己比版本号** —— 判据只能有一处实现,
   * 各写一遍的话,某天多一种要求更新版本的协议,这个下拉框会漏掉它
   * 而后端明明支持。与 udp_timeout、subscription_host 是同一条规矩。
   *
   * 只有 GET /api/nodes 与 GET /api/nodes/{id} 会带上它。
   */
  available_protocols?: { value: NodeProtocol; label: string }[]
  /**
   * 绑定的阿里云实例(V17),没绑定时是 null —— 据此决定要不要显示「云实例」
   * 那一块,不显示一个全是空值的卡片。只有 GET /api/nodes 与 GET /api/nodes/{id} 带它。
   */
  cloud?: CloudNodeView | null
  /**
   * 这个节点单配了私钥(true)还是用面板专用密钥(false)。私钥内容从不回显,
   * 但编辑表单要据此提示"当前用的是哪一把",并在单配私钥时给出
   * "改用面板专用密钥"的选项 —— 一把误贴、解不开的私钥同样是 true,
   * 那正是需要那个选项去救的情形。
   */
  uses_custom_key?: boolean
  created_at: string
  updated_at: string
  /**
   * 库里当前应有的配置是否已经在节点上生效。与 status 正交:
   * 一台 ONLINE 的机器完全可能跑着三次变更之前的配置。
   *
   * 只有 GET /api/nodes 与 GET /api/nodes/{id} 会带上这两个字段;
   * 创建与更新的响应里没有(调用方随后都会重新拉列表),所以是可选的。
   */
  config_state?: NodeConfigState
  /** 是否该提示部署。与 config_state 分开给 —— 不确定时不催。 */
  needs_deploy?: boolean
  /** 供应商到期档案(V20)。没接入时为 null;没登记时 state 是 UNSET */
  expiry?: ExpiryView | null
  /** 最近一次改管理地址 / SSH 端口的时间(V20),空串表示没改过。它之前的巡检与采样是旧地址的 */
  host_changed_at?: string
  /** PENDING = 连接参数改了但新地址当时连不上、管理员仍选择保存,还没验证过 */
  ssh_verify_state?: '' | 'PENDING'
}

/** 候选连接参数的验证结果(改管理地址时返回的 409 里带它)。 */
export interface NodeCandidateCheck {
  reachable: boolean
  error?: string
  dialed_ip?: string
  uname?: string
  fingerprint?: string
  old_fingerprint?: string
  /** 库里固定的主机密钥与新地址上出示的不一致 —— 不是同一台机器 */
  host_key_changed: boolean
  /** 库里还没固定过密钥(首次连接) */
  host_key_new: boolean
}

export interface NodeDependentHost {
  node_id: number
  name: string
  kind: string
}

export interface NodeUpdateResponse {
  node: Node
  effect: NodeUpdateEffect
  /** 管理地址 / 端口变了,随即调 recheckNode 全面重检 */
  recheck_required: boolean
  /** 为假表示这次是带 save_unverified 存下来的,节点标成「待验证」 */
  verified: boolean
  host_key_changed: boolean
  /** 依赖这台机器地址的中转 / 链式主机,已被标脏、按依赖顺序重新下发 */
  dependents: NodeDependentHost[]
}

/** 前置检查的一项(V20)。 */
export interface NodeCheckItem {
  key: string
  title: string
  status: 'SATISFIED' | 'REPAIRED' | 'NEEDS_ACTION' | 'NOT_APPLICABLE' | 'FAILED'
  detail: string
  duration_ms: number
  action?: string
}

export interface NodePreflight {
  operation: string
  items: NodeCheckItem[]
  ok: boolean
  repaired: boolean
  block_reason?: string
  service_running: boolean
  config_unchanged: boolean
  resolved_ip?: string
  host_key_fingerprint?: string
}

/** 管理地址变更后的全面重检结果(V20),步骤与部署记录同形。 */
export interface NodeRecheckResult {
  node_id: number
  ok: boolean
  steps: DeployStep[]
  preflight: NodePreflight
  services: NodeServiceFacts | null
  started_at: string
  finished_at: string
}

/**
 * 一条中转转发规则(nginx stream)。
 *
 * 它在订阅里是一条独立条目:**地址是中转主机的,协议参数与凭据是落地的**。
 * 客户端与落地之间的协议完全端到端,中转主机不解密也不认证。
 */
export interface NodeRelay {
  id: number
  node_id: number
  /** 中转主机的内部名称,只在后台出现 */
  node_name: string
  /**
   * 搬字节的程序(V15)。一经创建不可改:nginx 改规则 reload、在途连接不断;
   * realm 改规则 restart、在途连接全断 —— 两者的操作摩擦不同档。
   */
  engine: 'NGINX' | 'REALM'
  display_name: string
  /** nginx / realm 在中转主机上实际监听的端口 */
  listen_port: number
  /**
   * 客户端连接的公网端口。0 表示跟随 listen_port。
   *
   * 存 0 而不是把当时的 listen_port 写进去 —— 那样以后改监听端口,
   * 订阅条目会停在旧端口上,而管理员当初看到的是一个空输入框。
   */
  public_port: number
  /**
   * ADDRESS(V15)是管理员直接填的 host:port —— 面板不知道它背后的协议,
   * 所以这种规则不进订阅、不进门户,拨测也测不了。
   */
  target_kind: 'INBOUND' | 'EXTERNAL' | 'ADDRESS'
  /** 落地是自建节点【某一个入口】的 id,不是机器的 id */
  target_inbound_id: number
  target_external_id: number
  /** 只在 ADDRESS 下有值 */
  target_host: string
  target_port: number
  /** 落地的展示名(机器 / 入口),只给后台看 */
  target_name: string
  /**
   * 落地当前是否能给出可用的协议参数(自建入口要求已成功部署过)。
   *
   * 为 false 时这条线路**不会出现在任何人的订阅里** —— 界面上必须说出来,
   * 否则管理员会对着一条"配好了却不在订阅里"的线路找半天。
   */
  target_ready: boolean
  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number
  sort_order: number
  subscription_enabled: boolean
  public_remark: string
  /** 关掉后 nginx 里不再渲染这个 server 块(与删除不同,配置还留着) */
  enabled: boolean
  created_at: string
  updated_at: string
}

/** 中转主机上 nginx 的实测现状 */
export interface NginxFacts {
  installed: boolean
  binary_path: string
  version: string
  stream_built_in: boolean
  stream_module_path: string
  /** 为 false 时不能渲染任何转发配置 */
  stream_available: boolean
  /**
   * 缺失时该装的包名。
   *
   * 实测:Debian 12 与 Alpine 上 `装了 nginx 但没有 stream 模块` 都是
   * **默认情况**,而两边的报错都是同一句 unknown directive "stream",
   * 没有提到缺哪个包 —— 所以这一栏必须显示出来。
   */
  missing_package: string
  package_manager: string
}

/** 中转主机上 realm 的现状(V15),只读探测。 */
export interface RealmFacts {
  installed: boolean
  binary_path: string
  version: string
  /** 这台机器下发过 realm 配置没有 —— 与 nginx.conf 一样,配置在不在磁盘上是唯一可靠的判据 */
  config_present: boolean
  running: boolean
  state: string
}

/**
 * 一类服务的现状(入口 Tab 的服务卡片)。
 *
 * state 只取 RUNNING / STOPPED / NOT_APPLICABLE:这一次能连上才有结果,
 * 连不上是整个探测失败,不会落到某一个服务上。
 * 卡片据此三选一:没装 → 「安装」;装了没跑 → 「启动」;在跑 → 「停止」。
 */
export interface ServiceStatus {
  installed: boolean
  version: string
  /** 面板下发过的配置在不在节点上 —— 没有它「启动」就没有东西可启 */
  config_present: boolean
  state: ServiceState
  detail: string
}

/** 一个 mita 实例(= 一个下发过的 Mieru 入口)的现状 */
export interface MieruInstanceStatus {
  inbound_id: number
  display_name: string
  state: ServiceState
  detail: string
}

/**
 * 一台机器上四类服务的现状,一次 SSH 问完。
 *
 * 与巡检(NodeHealth)是两回事:巡检按 deployed_* 只问"该跑的还在不在跑",
 * 这里机器上有什么就报什么 —— 装了 realm 但一条规则都没配的机器,
 * 巡检说「不适用」,这里说「已安装、没在跑」。
 */
export interface NodeServiceFacts {
  checked_at: string
  init_system: string
  /**
   * upgradable:节点上的二进制与面板现在分发的不是同一个(按 SHA-256 比),
   * 点「重新安装」会换掉它。任何一边不知道时为 false。
   */
  singbox: ServiceStatus & { upgradable: boolean }
  /** Mieru 逐实例给,不合成一个 —— 「2/3 在跑」里多出的那一截正是要人去救的 */
  mieru: ServiceStatus & { instances: MieruInstanceStatus[] }
  nginx: ServiceStatus & { facts: NginxFacts }
  realm: ServiceStatus
}

/** 链式变更的编排结果:两台机器各一次部署 */
export interface ChainApplyResult {
  /** 落地那一次部署。落地是外部代理时为 null */
  target_deploy: DeployResult | null
  /** 中转主机那一次部署 */
  host_deploy: DeployResult | null
  /** 最终停在哪一步,失败时用来定位是哪台机器 */
  stage: string
}

export type NodeConfigState =
  | 'NEVER_DEPLOYED'
  | 'IN_SYNC'
  | 'PENDING'
  | 'DEPLOY_FAILED'
  | 'UNKNOWN'
  /** 中转机上没有 sing-box —— 这个问题在它身上没有主语 */
  | 'NOT_APPLICABLE'

export interface NodeMetrics {
  node_id: number
  cpu_percent: number
  mem_total_kb: number
  mem_used_kb: number
  net_rx_bps: number
  net_tx_bps: number
  load1: number
  uptime_seconds: number
  disk_total_kb: number
  disk_used_kb: number
  collected_at: string
}

/**
 * 资产卡片的实时网速(一个窗口的平均值)。只在仪表盘开着时由后台读,结果只在内存里;
 * 与资源采样里的 net_rx_bps 同一个口径(除 lo 以外全部网卡)。
 */
export interface NetRate {
  node_id: number
  rx_bps: number
  tx_bps: number
  window_seconds: number
  /** 读数时的开机秒数,卡片上的「在线 N 天」用它。 */
  uptime_seconds: number
  at: string
}

export interface MonitorStatus {
  interval_seconds: number
  retention_hours: number
  last_run_at: string
  errors: Record<number, string>
}

export interface BootstrapResult {
  node_id: number
  /** password 或 local-key */
  method: string
  already_present: boolean
  authorized_keys_path: string
  /**
   * 该节点原先关闭了公钥认证(PubkeyAuthentication no),引导过程中面板把它打开了。
   * 面板改了节点上的 sshd 配置,界面上要说出来 —— 那是这次引导里唯一一个
   * 越出"往 authorized_keys 追加一行"范围的动作。
   */
  pubkey_auth_fixed: boolean
  /**
   * 该节点原先不接受口令登录,引导过程中面板按管理员的勾选把它打开了
   * (PasswordAuthentication yes,root 再加 PermitRootLogin yes)。
   * 这是放宽了别人机器的安全策略,界面上必须单独说出来。
   */
  password_auth_fixed: boolean
  /** 「顺带打开口令登录」那一步的失败原因;它不让引导失败,所以只是一个附带字段。 */
  password_auth_error?: string
  detail: string
}

export interface CreateNodeResult {
  node: Node
  bootstrap?: BootstrapResult
  bootstrap_error?: string
}

/** 订阅排序方案(V20):LEGACY 先机器再入口、外部代理整块;GLOBAL 全局排序值一条数轴。 */
export type OrderScheme = 'LEGACY' | 'GLOBAL'

export interface OrderPlanItem {
  kind: 'NODE' | 'SINGBOX' | 'MIERU' | 'NGINX' | 'REALM' | 'EXTERNAL'
  id: number
  name: string
  node_id?: number
  node_name?: string
  old_sort: number
  new_sort: number
  /** 迁移之后的全局排序值(节点行是它的号段起点) */
  global: number
  note?: string
}

export interface OrderPlan {
  scheme: OrderScheme
  external_position: 'BEFORE' | 'AFTER'
  nodes: OrderPlanItem[]
  entries: OrderPlanItem[]
  externals: OrderPlanItem[]
  changed: number
  errors: string[]
  warnings: string[]
}

export interface PanelSettings {
  /** 订阅排序方案(V20)。 */
  subscription_order_scheme: OrderScheme
  /** 旧方案下外部代理整块排在自建节点之前还是之后。 */
  subscription_external_position: 'BEFORE' | 'AFTER'
  subscription_base_url: string
  config_base_url: string
  panel_public_key: string
  /** 拨测目标的设置值,空串表示跟随默认 */
  probe_url: string
  /** 留空时实际打的那个地址 */
  default_probe_url: string
  /** 云实例定时开关机 HH:MM 的解释时区(IANA 名),空串表示用默认 */
  cloud_timezone: string
  default_cloud_timezone: string
  /** 云账号轮询间隔(秒),0 表示用默认 */
  cloud_poll_interval_sec: number
  default_cloud_poll_interval_sec: number
  /** 到期提醒(V20):提前天数(逗号分隔)、每天几点、时区;空串表示用默认 */
  expiry_lead_days: string
  default_expiry_lead_days: string
  expiry_send_time: string
  default_expiry_send_time: string
  expiry_timezone: string
  /** 实际生效的时区(留空时跟随云实例时区) */
  effective_expiry_timezone: string
  /** TCP 探测(资产卡片):全局开关、间隔、超时、告警阈值、免打扰。数值 0 / 空串表示用默认 */
  probe_enabled: boolean
  probe_interval_sec: number
  default_probe_interval_sec: number
  probe_timeout_ms: number
  default_probe_timeout_ms: number
  probe_alert_after: string
  default_probe_alert_after: number
  probe_quiet_hours: string
}

// ---------- 供应商到期与续费(V20) ----------

export type ExpiryKind = 'NODE' | 'EXTERNAL_PROXY' | 'PROXY_SOURCE'
/** UNSET 没登记;OK 正常;SOON 在提前天数之内;OVERDUE 已到期且没拿到新的到期时间 */
export type ExpiryState = 'UNSET' | 'OK' | 'SOON' | 'OVERDUE'

export interface ExpiryNoticeSummary {
  stage: string
  channel: string
  status: 'PENDING' | 'SENT' | 'FAILED' | 'CANCELLED'
  sent_at: string
  last_error: string
}

/** 一个对象的到期视图:档案字段 + 算好的状态。一律有值,没登记时 state 为 UNSET */
export interface ExpiryView {
  kind: ExpiryKind
  object_id: number
  /** 对象自己有一行档案 */
  has_profile: boolean
  /** 外部代理:字段来自来源(代理源)的档案 */
  inherited: boolean
  inherited_from: number
  /** 商家账单到期时间,RFC3339 UTC;空串 = 未设置(不是永不过期) */
  expires_at: string
  reminder_enabled: boolean
  /** 「已在商家开启自动续费」—— 管理员登记的商家状态,不是面板代为扣款 */
  auto_renew: boolean
  vendor_name: string
  vendor_url: string
  note: string
  /** 按对象覆盖的提前天数;空数组表示继承系统规则 */
  lead_days: number[]
  state: ExpiryState
  /** 距到期还有几天(向上取整,已到期为负);未设置为 null */
  days_left: number | null
  last_notice: ExpiryNoticeSummary | null
  /** 套餐计费字段(资产卡片 P0-B),一律有值;没填过时 has_billing 为 false */
  billing?: BillingView
}

// ---------- 套餐计费与资产(资产卡片) ----------

/** 计费周期。空串 = 未设置 / 未知 */
export type BillingCycle =
  | ''
  | 'MONTHLY'
  | 'QUARTERLY'
  | 'SEMIANNUAL'
  | 'YEARLY'
  | 'CUSTOM'
  | 'ONE_TIME'
  | 'TRAFFIC_PACK'
  | 'PERMANENT'

/** 档案里的计费字段。金额是最小货币单位(-1 = 未填),十进制字符串另给 */
export interface BillingView {
  plan_name: string
  billing_cycle: BillingCycle
  cycle_count: number
  cycle_unit: '' | 'MONTH' | 'DAY'
  currency: string
  price_minor: number
  purchased_at: string
  period_start: string
  period_paid_minor: number
  period_carry_minor: number
  traffic_unlimited: boolean
  cycle_label: string
  price: string
  period_paid: string
  period_carry: string
  has_billing: boolean
}

/** 保存档案时的计费表单。金额用十进制字符串,空串 = 未填;整个对象省略表示保持原值 */
export interface BillingPayload {
  plan_name: string
  billing_cycle: BillingCycle
  cycle_count: number
  cycle_unit: '' | 'MONTH' | 'DAY'
  currency: string
  price: string
  purchased_at: string
  period_start: string
  period_paid: string
  traffic_unlimited: boolean
}

/** 一种口径的估算。不可用时 reason 说原因;可用时 basis 是可复核的算式 */
export interface ValueEstimate {
  available: boolean
  value_minor: number
  value: string
  reason: string
  basis: string
}

/**
 * 此刻的计费视图:剩余未摊销价值(估算)分「按时间」「按流量」两种口径,**不相加**;
 * 它不是余额、不是可退款金额。全部由后端现算,不入库。
 */
export interface Valuation {
  currency: string
  billing_cycle: BillingCycle
  cycle_label: string
  amount_minor: number
  amount: string
  paid_from_price: boolean
  carry_minor: number
  carry: string
  period_start: string
  period_start_derived: boolean
  period_end: string
  next_renewal_at: string
  time: ValueEstimate
  traffic: ValueEstimate
  monthly_cost_minor: number | null
  monthly_cost: string
  daily_cost_minor: number | null
  daily_cost: string
}

export interface AssetLabels {
  kind: ExpiryKind
  object_id: number
  /** ISO 3166-1 alpha-2 大写,空串 = 未设置。面板不按名字猜地区 */
  region: string
  tags: string[]
}

export interface BillingCard {
  kind: ExpiryKind
  object_id: number
  name: string
  plan_name: string
  auto_renew: boolean
  state: ExpiryState
  days_left: number | null
  valuation: Valuation
  has_billing: boolean
}

/** 一个币种的成本汇总。**不同币种不相加** */
export interface CostRow {
  currency: string
  monthly_cost_minor: number
  monthly_cost: string
  recurring_count: number
  unamortized_minor: number
  unamortized: string
  unamortized_count: number
  other_count: number
}

// ---------- 网络探测:出站网络质量(节点 → 公共目标)与入口连通性(主控 → 节点) ----------

export type ProbeKind = 'MGMT' | 'SERVICE' | 'PUBLIC'
export type ProbeFamily = 'V4' | 'V6'
/** INGRESS = 入口连通性(主控 → 这台机器);OUTBOUND = 出站网络质量(这台机器 → 公共目标) */
export type ProbeDirection = 'INGRESS' | 'OUTBOUND'
/** HTTP = 出站 HTTP(S) 请求响应检测(只有出站;V22.6) */
export type ProbeProtocol = 'TCP' | 'ICMP' | 'HTTP'
/** TCP 的两种方式:CONNECT = 一次完整建连;SYN = 短超时握手近似(基于系统 TCP 建连,不是原始套接字 SYN 探测) */
export type ProbeTCPMode = 'CONNECT' | 'SYN'
/**
 * 一条序列看哪个比例,界面一律按它起名,不按协议自己推:
 * LOSS = ICMP 真实丢包率;CONNECT = TCP 连接失败率;SYN = TCP 握手超时无应答率(不是丢包率)。
 */
/**
 * BOCE = Boce 拨测点的 TCPing 丢包率;BOCE_PING = Boce 拨测点的 Ping(ICMP)丢包率;
 * HTTP = HTTP 请求失败率(未取得有效 HTTP 响应的比例;404、5xx 都算取得了响应)。
 */
export type ProbeMetric = 'LOSS' | 'CONNECT' | 'SYN' | 'BOCE' | 'BOCE_PING' | 'HTTP'
/**
 * 一次结果。LOSS = ICMP 部分丢包(可达);SSH_ERROR = 采集失败(面板连不上这台机器,不是丢包);
 * TOOL_ERROR = 辅助程序不可用;BLOCKED = 目标受限;这三种与 DNS_ERROR、SKIPPED 都没有发起,不进分母。
 */
export type ProbeStatus =
  | 'OK' | 'LOSS' | 'TIMEOUT' | 'REFUSED' | 'UNREACHABLE' | 'ERROR'
  | 'DNS_ERROR' | 'BLOCKED' | 'SSH_ERROR' | 'TOOL_ERROR' | 'SKIPPED'
  | 'VANTAGE_ERROR' | 'API_ERROR' | 'EXPIRED'
/** 卡片上一行的状态。几种「没有新数据」分开说:暂停 / 停用 / 关机 / 失效 / 过期 / 等待首次 / 采集失败 / 工具不可用 */
export type ProbeState =
  | 'OK' | 'LOSSY' | 'FAILING' | 'DOWN' | 'PAUSED' | 'DISABLED' | 'STOPPED' | 'INVALID' | 'STALE' | 'PENDING'
  | 'COLLECT_FAILED' | 'TOOL_ERROR' | 'BLOCKED'
  | 'VANTAGE_ERROR' | 'API_ERROR' | 'EXPIRED' | 'SKIPPED'
  /** HTTP 最近一次取得了响应,但状态码是 5xx(服务返回异常状态;网络是通的) */
  | 'HTTP_5XX'
/**
 * 点阵颜色。NONE = 这个窗口没有样本,**不按正常画**;SSH / TOOL 也不按失败画 —— 那几轮根本没有发出去。
 * HTTP5XX = 这个窗口每一次都取得了 HTTP 响应,但其中有 5xx。
 */
export type ProbeBucketState =
  | 'OK' | 'HTTP5XX' | 'PARTIAL' | 'FAIL' | 'DNS' | 'SSH' | 'TOOL' | 'BLOCKED' | 'VANTAGE' | 'API' | 'EXPIRED' | 'SKIP' | 'NONE'

export interface ProbeTarget {
  id: number
  node_id: number
  direction: ProbeDirection
  protocol: ProbeProtocol
  tcp_mode: ProbeTCPMode
  metric: ProbeMetric
  kind: ProbeKind
  ref: string
  /** 出站序列指向的公共目标;入口目标为 0 */
  public_id: number
  /** Boce 序列所属的任务与拨测点;其余为 0 */
  boce_task_id: number
  vantage_id: number
  /** 自建节点拨测(PEER)的拨测来源机器;其余为 0 */
  peer_node_id: number
  family: ProbeFamily
  label: string
  source: string
  enabled: boolean
  resolved_host: string
  resolved_port: number
  generation: number
  generation_since: string
  ref_state: 'OK' | 'INVALID'
  ref_error: string
  consecutive_failures: number
  alert_state: 'OK' | 'DOWN'
  alert_since: string
  alert_notified: boolean
  last_status: ProbeStatus | ''
  last_at: string
  last_success_at: string
  created_at: string
  updated_at: string
}

export interface ProbeBucket {
  at: string
  /** 实际发起的轮数(TCP 即建连数)—— 失败率的分母 */
  attempts: number
  ok: number
  /** ICMP 部分丢包的轮数 */
  lossy: number
  failures: number
  /** 没有发起的几种,各自单列,都不在分母里 */
  dns_errors: number
  skipped: number
  ssh_errors: number
  tool_errors: number
  blocked: number
  /** Boce 独有:拨测点异常、接口异常、任务未取得结果(都不进分母) */
  vantage_errors: number
  api_errors: number
  expired: number
  /** HTTP 取得的响应按状态码分类(都算取得了响应);其余序列恒为 0 */
  http_2xx: number
  http_3xx: number
  http_4xx: number
  http_5xx: number
  /** 按包计(TCP 建连按建连计,握手近似按 SYN 计,HTTP 按请求计);loss_rate = (sent − received) / sent,一次都没发起时为 null */
  sent: number
  received: number
  loss_rate: number | null
  rtt_avg_ms: number | null
  rtt_min_ms: number | null
  rtt_max_ms: number | null
  state: ProbeBucketState
}

export interface ProbeStats {
  from: string
  to: string
  attempts: number
  ok: number
  lossy: number
  failures: number
  dns_errors: number
  skipped: number
  ssh_errors: number
  tool_errors: number
  blocked: number
  vantage_errors: number
  api_errors: number
  expired: number
  http_2xx: number
  http_3xx: number
  http_4xx: number
  http_5xx: number
  sent: number
  received: number
  /** 0~1。TCP 建连的连接失败率;ICMP 与握手近似时是「整轮失败」的比例。一次都没发起过时为 null,不是 0 */
  failure_rate: number | null
  /** 0~1。(发 − 收) ÷ 发:ICMP 丢包率 / TCP 握手超时无应答率;TCP 建连时与 failure_rate 相同 */
  loss_rate: number | null
  rtt_avg_ms: number | null
  rtt_min_ms: number | null
  rtt_max_ms: number | null
}

export interface ProbeLastSample {
  at: string
  status: ProbeStatus
  label: string
  rtt_ms: number | null
  error: string
  sent: number
  received: number
  /** 实际连的 IP(域名每轮重新解析) */
  peer: string
  /** HTTP:取得响应时的状态码 */
  http_status?: number
}

/** 自建节点拨测的来源机器(取当前名字) */
export interface PeerSourceInfo {
  node_id: number
  name: string
  /** Ping / TCPing */
  method: string
}

export interface ProbeSummary extends ProbeTarget {
  source_label: string
  state: ProbeState
  state_text: string
  last: ProbeLastSample | null
  stale: boolean
  h1: ProbeStats
  h24: ProbeStats
  dots: ProbeBucket[]
  dot_seconds: number
  /** 入口用全局间隔,出站用公共目标自己的,Boce 按任务的时间表 */
  interval_sec: number
  note: string
  /** Boce 序列的任务与拨测点信息 */
  boce?: BoceSeriesInfo
  /** 自建节点拨测(PEER)的来源机器 */
  peer_source?: PeerSourceInfo
}

export interface ProbeCandidate {
  ref: string
  kind: ProbeKind
  family: ProbeFamily
  label: string
  host: string
  port: number
  note: string
}

export interface ProbeRoundStatus {
  last_run_at: string
  duration_ms: number
  probed: number
  skipped: number
  invalid: number
  last_error: string
  interval_sec: number
}

export interface OutboundRoundStatus {
  last_run_at: string
  duration_ms: number
  nodes: number
  probed: number
  /** 节点连接正忙而让路的机器数(下一拍再来,不记结果) */
  busy: number
  ssh_errors: number
  tool_errors: number
  skipped: number
  /** 到期了但因为限额延后到下一拍的序列数(不记结果、不算失败) */
  deferred: number
  /** 最近一分钟对第三方目标的实际探测尝试 / 上限 */
  third_party_used: number
  third_party_budget: number
  last_error: string
}

export interface ProbeOverview {
  enabled: boolean
  /** 入口连通性的间隔与超时;出站的是每个公共目标自己的 */
  interval_sec: number
  timeout_ms: number
  alert_after: number
  quiet_hours: string
  sources: { code: string; label: string }[]
  last_run: ProbeRoundStatus
  /** 面板没接入探测引擎时为 false */
  available: boolean
  outbound_available: boolean
  outbound_last_run: OutboundRoundStatus
  public_targets: number
  /** 主控手上有的辅助程序:架构 → 字节数。缺的架构上出站探测会报「工具不可用」 */
  helpers: Record<string, number>
}

export type ProbeOrigin = 'MANUAL' | 'ZSTATIC' | 'TCPQUALITY'
export type ProbeISP = '' | 'CT' | 'CU' | 'CM' | 'CERNET' | 'OTHER'
export type ProbeVerifyState = 'UNVERIFIED' | 'OK' | 'WARN' | 'FAILED'

export interface PublicProbeTarget {
  id: number
  name: string
  host: string
  protocol: ProbeProtocol
  tcp_mode: ProbeTCPMode
  /** 只对 HTTP:完整地址(含查询串,只给管理员看);主机与端口由它推出来 */
  url: string
  metric: ProbeMetric
  port: number
  family: ProbeFamily
  interval_sec: number
  count: number
  timeout_ms: number
  apply_all: boolean
  enabled: boolean
  allow_restricted: boolean
  sort_order: number
  note: string
  created_at: string
  updated_at: string
  source: ProbeOrigin
  source_key: string
  region: string
  province: string
  city: string
  isp: ProbeISP
  /** BACKUP = 上游列表里的备用节点 */
  role: 'PRIMARY' | 'BACKUP'
  verify_state: ProbeVerifyState
  verified_at: string
  verify_detail: string
  /** 导入之后管理员手工改过的字段,同步不覆盖 */
  manual_fields: string[]
  upstream_missing_since: string
  third_party: boolean
  /** 套上来源下限之后的间隔 */
  effective_interval_sec: number
  /** 此刻对它生效的机器数;on / off 是显式的节点覆盖数 */
  node_count: number
  on_count: number
  off_count: number
  /** 目标暂停 / 来源暂停 / 来源未确认授权;空串 = 没有被暂停 */
  pause_reason: string
}

export interface PublicProbeInput {
  name: string
  host: string
  protocol: ProbeProtocol
  tcp_mode?: ProbeTCPMode
  /** 只对 HTTP:http:// 或 https:// 的完整地址 */
  url?: string
  port: number
  family: ProbeFamily
  interval_sec: number
  count: number
  timeout_ms: number
  enabled?: boolean
  allow_restricted: boolean
  sort_order: number
  note: string
  region?: string
  province?: string
  city?: string
  isp?: ProbeISP
}

export interface ProbeSource {
  kind: ProbeOrigin
  name: string
  third_party: boolean
  paused: boolean
  authorized: boolean
  /** 管理员确认「已阅读说明并具备使用授权」的时间与人 —— 只是声明,不代表来源方授权 */
  authorized_at: string
  authorized_by: string
  authorized_note: string
  high_freq_allowed: boolean
  min_interval_sec: number
  per_target_per_min: number
  sync_state: 'NEVER' | 'OK' | 'ERROR'
  last_sync_at: string
  last_success_at: string
  last_error: string
  item_count: number
  homepage: string
  notice: string[]
  syncable: boolean
  presets: boolean
  target_count: number
  active_count: number
}

export interface ProbeSourceInput {
  paused?: boolean
  min_interval_sec?: number
  per_target_per_min?: number
  high_freq_allowed?: boolean
  authorize?: boolean
  authorized_note?: string
}

export interface ProbeLimits {
  third_party_budget_per_min: number
  third_party_per_node: number
  node_concurrency: number
  round_max: number
}

export interface ProbeLimitRange {
  key: string
  label: string
  min: number
  max: number
  default: number
  set: boolean
}

export interface ProbeProvince {
  code: string
  name: string
  region: string
  common: boolean
}

export interface PublicProbeLibrary {
  targets: PublicProbeTarget[]
  probe: ProbeOverview
  sources: ProbeSource[]
  limits: { limits: ProbeLimits; ranges: ProbeLimitRange[] }
  provinces: ProbeProvince[]
}

export type ProbePreviewStatus = 'NEW' | 'SAME' | 'CHANGED' | 'EXISTS' | 'MISSING'

export interface ProbePreviewItem {
  key: string
  name: string
  host: string
  port: number
  family: ProbeFamily
  region: string
  province: string
  city: string
  isp: ProbeISP
  role: 'PRIMARY' | 'BACKUP'
  /** PROVINCE / CITY(Zstatic)或 CDN(TcpQuality) */
  level: string
  /** 上游给的固定 IP(只有 TcpQuality 有) */
  ip: string
  status: ProbePreviewStatus
  local_ids: number[]
  changes: string[]
  suspicious: string[]
  manual_kept: string[]
  exists_as: string
  missing_since: string
}

export interface ProbePreview {
  source: ProbeOrigin
  snapshot_id: string
  fetched_at: string
  tcp_mode: ProbeTCPMode
  host_mode: 'DOMAIN' | 'IP'
  items: ProbePreviewItem[]
  missing: ProbePreviewItem[]
  rejected: { line: number; text: string; reason: string }[]
  /** 按类型统计的、首版不提供批量导入的条目数 */
  unsupported: Record<string, number>
  counts: Record<string, number>
  bulk_warning: string
}

export interface ProbeApplyRequest {
  source: ProbeOrigin
  snapshot_id?: string
  import?: string[]
  update?: string[]
  mark_missing?: boolean
  tcp_mode?: ProbeTCPMode
  host_mode?: 'DOMAIN' | 'IP'
  interval_sec?: number
}

export interface ProbeApplyResult {
  created: number
  updated: number
  missing: number
  restored: number
  skipped: string[]
}

export interface ProbeImportRow {
  line: number
  input: PublicProbeInput
  source: ProbeOrigin
  status: 'OK' | 'DUPLICATE' | 'ERROR'
  reason: string
}

export interface ProbeImportResult {
  rows: ProbeImportRow[]
  ok: number
  errors: number
  duplicates: number
  created: number
}

export interface ProbeVerifyResult {
  public_id: number
  state: ProbeVerifyState
  detail: string
}

export interface ProbeTrialResult {
  public_id: number
  name: string
  metric: ProbeMetric
  status: ProbeStatus
  label: string
  error: string
  peer: string
  rtt_ms: number | null
  sent: number
  received: number
  /** HTTP:取得响应时的状态码 */
  http_status?: number
  verify_state: ProbeVerifyState | ''
}

export interface NodeProbeSettings {
  node_id: number
  paused: boolean
  /** 0 = 跟随各目标自己的间隔;否则这台机器上全部出站目标按它(仍受来源下限约束) */
  interval_sec: number
  /** 暂停这台节点的 Boce 自动检测(手动检测不受影响) */
  boce_paused: boolean
  updated_at: string
}

/** 节点覆盖:ON / OFF / INHERIT(跟随公共目标的「全部节点」开关) */
export type ProbeBindingMode = 'ON' | 'OFF' | 'INHERIT'
export type PublicProbeScope = 'ALL' | 'SELECTED' | 'NONE'

export interface NodePublicProbe extends PublicProbeTarget {
  mode: ProbeBindingMode
  effective: boolean
  /** 这台机器测它的实际间隔(本机覆盖与来源下限都算上) */
  node_interval_sec: number
  /** 这台机器上不测它的原因里与选择无关的那几种(含「本机已暂停出站探测」) */
  node_pause_reason: string
}

export interface PublicProbeNode {
  node_id: number
  name: string
  display_name: string
  mode: ProbeBindingMode
  effective: boolean
  /** 这台机器暂停了出站探测 */
  paused: boolean
}

export interface NodeProbe {
  probe: ProbeOverview
  /** 出站网络质量;public 是整个目标库在这台机器上的开关 */
  outbound: ProbeSummary[]
  public: NodePublicProbe[]
  node_settings: NodeProbeSettings
  /** 入口连通性 */
  targets: ProbeSummary[]
  candidates: ProbeCandidate[]
  /** Boce 外部入站拨测的序列(每个「任务 × 拨测点」一条) */
  boce: ProbeSummary[]
  /** 自建节点互测入站(别的自建机器 → 这台机器的订阅入口) */
  peer: ProbeSummary[]
  /** 新建自建节点拨测时可选的来源(除这台之外的全部机器) */
  peer_sources: PeerSourceOption[]
  card: { prefs: CardPrefs; pick: CardPick }
}

export interface PeerSourceOption {
  node_id: number
  name: string
  display_name: string
  /** 非空 = 这台机器眼下测不了的原因(停用、暂停了本机探测、没探测过架构);仍可选 */
  note: string
}

export interface PeerInput {
  ref: string
  family: ProbeFamily
  source_node_ids: number[]
  /** ICMP = Ping,TCP = TCPing */
  methods: ('ICMP' | 'TCP')[]
  /** false = 只建、不跑周期检测(手动「立即检测」仍可用) */
  enabled?: boolean
}

export interface PeerRunResult {
  id: number
  status: ProbeStatus | ''
  label: string
  rtt_ms: number | null
  error: string
  /** false = 来源机器正忙,这一次没有测、什么都没记 */
  recorded: boolean
}

export interface ProbeFailurePoint {
  at: string
  status: ProbeStatus
  label: string
  error: string
  sent: number
  received: number
  peer: string
  /** HTTP:取得了 5xx 响应的那几轮(status 是 OK) */
  http_status: number
}

export interface ProbeHistory {
  target: ProbeTarget
  range: '1h' | '24h' | '7d'
  bucket_seconds: number
  source: string
  buckets: ProbeBucket[]
  stats: ProbeStats
  failures: ProbeFailurePoint[]
}

/** 外部代理卡片的「网络」一格:只有上次【手动】检查的结果,不做定时探测 */
export interface ExternalCheckStatus {
  proxy_id: number
  last_check_at: string | null
  last_check_ok: boolean | null
  latency_ms: number | null
  message: string
}

/**
 * 仪表盘卡片视图的聚合接口:一次返回全部卡片要的「新」数据。
 * 节点列表、资源采样、周期流量、巡检、到期仍由原有的批量接口给 —— 卡片不按卡片逐个请求。
 */
export interface DashboardCards {
  generated_at: string
  probe: ProbeOverview
  labels: AssetLabels[]
  billing: BillingCard[]
  /** 入口连通性(主控 → 节点) */
  targets: ProbeSummary[]
  /** 出站网络质量(节点 → 公共目标) */
  outbound: ProbeSummary[]
  /** Boce 外部入站拨测的序列 */
  boce: ProbeSummary[]
  /** 自建节点互测入站的序列 */
  peer: ProbeSummary[]
  /** 每台节点卡片上入站 / 出站各展示哪几项 */
  card_picks: CardPick[]
  cost_summary: CostRow[]
  expiring_7d: number
  overdue: number
  /** 只算自建节点的成本与到期数:卡片视图只画自建节点,概览要与卡片对得上(上面三项含外部线路) */
  node_cost_summary: CostRow[]
  node_expiring_7d: number
  node_overdue: number
  /** 读失败的块,点名;其余照常给 */
  errors: string[]
  external: ExternalCheckStatus[]
}


// ---------- Boce 外部入站拨测(付费)与卡片展示偏好 ----------

/** Boce 时间表:INTERVAL = 固定间隔(从运行窗口起点对齐);TIMES = 固定时间点(窗口不参与) */
export interface BoceScheduleSpec {
  schedule_mode: 'INTERVAL' | 'TIMES'
  interval_min: number
  times: string[]
  window_start: string
  window_end: string
  /** 星期掩码:bit0 = 周一 … bit6 = 周日,127 = 每天 */
  weekdays: number
  timezone: string
}

export interface BoceSettings {
  enabled: boolean
  key_configured: boolean
  /** 只有「…末 4 位」,Key 本身从不返回 */
  key_hint: string
  points_per_check: number
  point_price: string
  daily_points: number
  monthly_points: number
  run_points: number
  alert_percent: number
  low_balance_points: number
  timezone: string
  default_schedule: BoceScheduleSpec
}

export interface BoceSettingsInput extends Omit<BoceSettings, 'key_configured' | 'key_hint'> {
  /** 不传 = 保持原值;空串 = 清空 */
  api_key?: string
}

export interface BoceState {
  verify_state: 'NEVER' | 'OK' | 'ERROR'
  verified_at: string
  verify_error: string
  balance_yuan: string
  /** Boce 账户的真实剩余波点(可能受面板以外的调用影响);null = 没查到过 */
  balance_points: number | null
  balance_at: string
  balance_error: string
  vantages_synced_at: string
  vantages_error: string
  auto_paused: boolean
  auto_pause_reason: string
  auto_paused_at: string
  consecutive_api_errors: number
  last_api_error: string
  last_api_error_at: string
}

/** 面板自己的估算消耗(不是账户余额) */
export interface BoceUsage {
  day_key: string
  month_key: string
  today_points: number
  month_points: number
  total_points: number
  daily_limit: number
  monthly_limit: number
  today_yuan: number
  month_yuan: number
  total_yuan: number
  timezone: string
}

export interface BoceEstimate {
  points_per_round: number
  rounds_today: number
  points_today: number
  month: string
  days_in_month: number
  rounds_month: number
  points_month: number
  peak_day_points: number
  daily_avg_points: number
  points_30d: number
  yuan_today: number
  yuan_month: number
  others_month_points: number
  others_daily_avg: number
  total_month_points: number
  total_daily_avg: number
  total_yuan_month: number
  balance_points: number | null
  days_runnable: number | null
  point_price: number
  daily_limit: number
  monthly_limit: number
  run_limit: number
  over_run_limit: boolean
  over_daily: boolean
  over_monthly: boolean
}

export interface BoceEstimateInput extends BoceScheduleSpec {
  vantages: number
  targets?: number
  exclude_task_id?: number
}

export interface BoceRangeDTO {
  key: string
  label: string
  min: number
  max: number
  default: number
}

export interface BoceOverview {
  available: boolean
  settings: BoceSettings
  state: BoceState
  usage: BoceUsage
  /** 非空 = 此刻不会新建任何付费任务 */
  blocked: string
  engine: { running: boolean; last_tick_at: string; last_error: string }
  vantage_counts: { cn: number; oversea: number; missing: number }
  plan: BoceEstimate | null
  ranges: BoceRangeDTO[]
}

/** 缓存的 Boce 拨测点。地区与运营商只有接口实际返回的值 */
export interface BoceVantage {
  id: number
  area: 'CN' | 'OVERSEA'
  node_name: string
  isp_name: string
  isp_code: number
  first_seen_at: string
  last_seen_at: string
  /** 非空 = 已从 Boce 节点列表里消失(不会被自动换成别的付费拨测点) */
  missing_since: string
}

/** Boce 的检测方式。创建后不可改:换测法就是另一条测量。 */
export type BoceTestType = 'TCPING' | 'PING'

export interface BoceTask extends BoceScheduleSpec {
  id: number
  node_id: number
  name: string
  test_type: BoceTestType
  ref: string
  family: ProbeFamily
  vantage_ids: number[]
  month_points_limit: number
  enabled: boolean
  auto_paused_reason: string
  consecutive_failures: number
  next_run_at: string
  last_run_at: string
  last_state: string
  last_error: string
  archived_at: string
  note: string
  created_at: string
  updated_at: string
}

export type BoceRunState = 'CREATING' | 'UNKNOWN' | 'POLLING' | 'DONE' | 'TIMEOUT' | 'EXPIRED' | 'FAILED' | 'SKIPPED'

export interface BoceResult {
  vantage_id: number
  target_id: number
  at: string
  status: ProbeStatus
  label: string
  node_name: string
  origin_ip: string
  ip: string
  ip_region: string
  ip_isp: string
  /** 上游原样(单位文档没写),不参与统计 */
  packet_loss: number
  sent: number
  received: number
  rtt_avg_ms: number
  rtt_min_ms: number
  rtt_max_ms: number
  error_code: number
  error: string
  charged: boolean
}

export interface BoceRun {
  id: number
  /** 0 = 任务已删除(执行记录与估算消耗保留) */
  task_id: number
  node_id: number
  trigger: 'SCHEDULE' | 'MANUAL'
  /** 这一轮提交时的测法 */
  test_type: BoceTestType
  slot: string
  state: BoceRunState
  state_text: string
  vendor_task_id: string
  host: string
  generation: number
  vantage_ids: number[]
  points: number
  points_reserved: number
  day_key: string
  month_key: string
  attempts: number
  polls: number
  next_poll_at: string
  results: number
  error_code: number
  error: string
  created_at: string
  sent_at: string
  finished_at: string
  items: BoceResult[]
}

export interface BoceTaskView extends BoceTask {
  node_name: string
  target_label: string
  target_host: string
  target_port: number
  /** 非空 = 被测入口此刻解析不出来 */
  target_error: string
  month_points: number
  total_points: number
  plan_month_points: number
  plan_daily_avg: number
  points_per_round: number
  /** 非空 = 此刻不会自动执行 */
  auto_reason: string
  last_run: BoceRun | null
}

export interface BoceTaskInput extends BoceScheduleSpec {
  name: string
  /** 创建时空 = TCPing;修改时不传(测法不能改) */
  test_type?: BoceTestType
  ref?: string
  family?: ProbeFamily
  vantage_ids: number[]
  month_points_limit: number
  enabled?: boolean
  note: string
}

export interface NodeBoce {
  tasks: BoceTaskView[]
  /** 只有订阅入口;带 note 的(IPv6、私网地址)选不了 */
  candidates: ProbeCandidate[]
  paused: boolean
  blocked: string
  default_schedule: BoceScheduleSpec
  points_per_check: number
  point_price: number
}

export interface BoceSeriesInfo {
  task_id: number
  task_name: string
  vantage_id: number
  vantage_name: string
  isp_name: string
  area: string
  missing: boolean
  schedule: string
  next_run_at: string
  auto_on: boolean
}

export type CardMode = 'AUTO' | 'MANUAL'

/** 卡片展示偏好:每个方向最多 3 项。这是展示数量,不是检测数量 */
export interface CardPrefs {
  node_id: number
  ingress_mode: CardMode
  ingress_ids: number[]
  outbound_mode: CardMode
  outbound_ids: number[]
  /** 序列 id → 卡片上的显示名称(只改展示,不改检测;空 = 原始名称) */
  ingress_aliases: Record<string, string>
  outbound_aliases: Record<string, string>
  updated_at: string
}

/** 一台节点的卡片此刻实际展示哪几项(手动 / 自动已经解析好) */
export interface CardPick {
  node_id: number
  ingress_mode: CardMode
  ingress_ids: number[]
  outbound_mode: CardMode
  outbound_ids: number[]
  /** 手动选了、但那条序列已经删掉的项:提示,不自动顶上别的 */
  invalid_ingress: number[]
  invalid_outbound: number[]
  auto_ingress: number[]
  auto_outbound: number[]
  /** 卡片上的显示名称(只含还存在的序列) */
  ingress_aliases: Record<string, string>
  outbound_aliases: Record<string, string>
}

export interface ExpiryRenewal {
  id: number
  kind: ExpiryKind
  object_id: number
  request_id: string
  old_expires_at: string
  new_expires_at: string
  method: string
  method_text: string
  base: string
  admin_user_id: number | null
  admin_name: string
  note: string
  created_at: string
  /** 这一笔的实际支付(最小单位,-1 = 没记)与十进制字符串 */
  amount_minor?: number
  amount?: string
  currency?: string
  /** 这一笔建立的已付费周期从哪天起 */
  period_start?: string
}

export interface ExpiryDetail {
  view: ExpiryView
  renewals: ExpiryRenewal[]
  /** 此刻的计费估值(现算) */
  valuation?: Valuation
  /** 地区与标签(资产卡片);没接入时为 null */
  labels?: AssetLabels | null
  /** 全局规则,表单上显示「继承:7,3,1」用 */
  lead_days: number[]
  send_time: string
  timezone: string
}

export interface ExpiryProfilePayload {
  expires_at: string
  reminder_enabled: boolean
  auto_renew: boolean
  vendor_name: string
  vendor_url: string
  note: string
  lead_days: number[]
  /** 省略 = 计费字段保持原值(节点 / 外部代理 / 代理源的编辑表单只带到期那几栏) */
  billing?: BillingPayload
}

export type ExpiryRenewMethod = 'MONTHS' | 'YEARS' | 'DAYS' | 'ABSOLUTE' | 'CLEAR'
export type ExpiryRenewBase = 'CURRENT' | 'NOW' | ''

export interface ExpiryRenewPayload {
  method: ExpiryRenewMethod
  count?: number
  base?: ExpiryRenewBase
  expires_at?: string
  /** 弹窗打开时生成一次;重复点击、网络重试都带同一个值 */
  request_id?: string
  note?: string
  /** 只算不写 */
  preview?: boolean
  /** 这一笔的实际支付(十进制字符串,空串 = 没记);币种留空取套餐币种 */
  amount?: string
  currency?: string
}

export interface ExpiryRenewPlan {
  old_expires_at: string
  new_expires_at: string
  base: string
  method_text: string
  method: string
}

/** 续费之后的本期:起点、本次支付(-1 = 按标价)、上期结转 */
export interface ExpiryRenewPeriod {
  start: string
  paid_minor: number
  /** 十进制字符串,按币种小数位格式化;paid_minor 为 -1 时是空串 */
  paid: string
  carry_minor: number
  carry: string
  currency: string
}

export interface ExpiryRenewResult {
  plan: ExpiryRenewPlan
  period: ExpiryRenewPeriod | null
  /** 这个 request_id 已经提交过,这次没有再延长 */
  duplicate: boolean
  renewal: ExpiryRenewal | null
  view: ExpiryView | null
}

// ---------- 阿里云 CDT 主机(V17) ----------

/** 阿里云实例状态,取值是阿里云的原文;空串表示还没查过 */
export type CloudInstanceStatus = 'Running' | 'Stopped' | 'Starting' | 'Stopping' | 'Pending' | ''
export type CloudStoppedMode = 'KeepCharging' | 'StopCharging'
export type CloudThresholdAction = 'NOTIFY' | 'STOP'
export type CloudStoppedBy = '' | 'THRESHOLD' | 'SCHEDULE' | 'MANUAL'
export type CloudTrafficClass = 'INTL' | 'CN'

export interface CloudAccountState {
  intl_bytes: number
  cn_bytes: number
  /** 上一次【成功】采样的时间,空串表示从未成功 */
  sampled_at: string
  last_error: string
  consecutive_failures: number
}

/**
 * 云账号。Secret 永远不随接口返回;编辑时留空表示不改。
 * 两个池子的百分比与超没超由后端算好 —— 阈值判定只能有一处实现。
 */
export interface CloudAccount {
  id: number
  name: string
  provider: 'ALIYUN'
  access_key_id: string
  access_key_id_masked: string
  /** 两个池子的免费额度(字节),0 表示不限 */
  cdt_quota_intl_bytes: number
  cdt_quota_cn_bytes: number
  threshold_percent: number
  enabled: boolean
  created_at: string
  updated_at: string
  state: CloudAccountState
  intl_percent: number | null
  cn_percent: number | null
  intl_over: boolean
  cn_over: boolean
  bound_nodes: number
  intl_label: string
  cn_label: string
}

/** 挂在节点上的云实例信息:绑定 + 运行态 + 所在池子(账号级)的用量。 */
export interface CloudNodeView {
  node_id: number
  account_id: number
  region_id: string
  instance_id: string
  traffic_class: CloudTrafficClass
  threshold_action: CloudThresholdAction
  stopped_mode: CloudStoppedMode
  schedule_enabled: boolean
  start_time: string
  stop_time: string
  keepalive: boolean
  instance_status: CloudInstanceStatus
  status_at: string
  public_ip: string
  has_eip: boolean
  spot: boolean
  charge_type: string
  stopped_by: CloudStoppedBy
  stopped_at: string
  last_error: string
  keepalive_failures: number
  keepalive_retry_at: string
  account_name: string
  class_label: string
  used_bytes: number
  quota_bytes: number
  usage_percent: number | null
  over: boolean
  sampled: boolean
  sampled_at: string
  query_error: string
  status_label: string
  stopped_by_label: string
  stopped_mode_label: string
  /** 实例的对外地址与节点管理地址不一致(管理地址是 IP 字面量时才比) */
  ip_mismatch: boolean
}

export interface CloudInstance {
  instance_id: string
  instance_name: string
  region_id: string
  zone_id: string
  instance_type: string
  status: CloudInstanceStatus
  stopped_mode: string
  charge_type: string
  spot_strategy: string
  public_ip: string
  eip: string
  os_name: string
}

export interface CloudPowerEvent {
  id: number
  node_id: number
  account_id: number
  kind: string
  kind_label: string
  status: 'SENT' | 'FAILED' | 'SKIPPED'
  detail: string
  created_at: string
}

export interface CloudTestResult {
  regions: { business_region_id: string; bytes: number }[]
  intl_bytes: number
  cn_bytes: number
}

export interface CloudSample {
  bucket_ts: number
  bytes: number
}

export interface NodeUpdateEffect {
  /** 连接参数变了,面板已丢弃该节点的 SSH 长连接 */
  ssh_changed: boolean
  /** 节点上跑的配置已与期望状态不一致,需要重新部署才生效 */
  needs_deploy: boolean
  /** 访问等级变了,面板已自动标脏并会尽快重新部署 */
  tier_changed: boolean
  changes: string[]
}

export interface ProbeResult {
  arch: string
  kernel: string
  os_name: string
  mem_total_mb: number
  singbox_path: string
  /**
   * 这次连接实际连上的 IP。节点填 IP 字面量时与它相同;填域名时是**此刻**的
   * 解析结果 —— 动态 DNS 的节点上,这是唯一能看到「域名现在指到哪儿」的地方。
   */
  resolved_ip: string
  singbox_version: string
  build_tags: string[]
  has_v2ray_api: boolean
  /** 节点上 mita 的版本。只在这台机器有 Mieru 入口时才问,空串表示没问或没装。 */
  mita_version: string
  /**
   * 有没有 unshare(util-linux)。
   *
   * 每个 mita 实例要一个私有的挂载命名空间 —— 共用 /var/lib/mita 的那份
   * metrics.pb 会让实例之间互相覆盖流量计数,而每个实例都"正常运行"。
   */
  has_unshare: boolean
  /** systemd 或 openrc,两者都没有则为空 */
  init_system: string
  init_version: string
  /** 实测的 SSH 通道能力:yes / no / unknown */
  tcp_forwarding: string
  /** 「这台机器跑不了 sing-box」级别的问题 */
  problems: string[]
  /** 「能跑,但面板的某些功能用不了」,例如 sshd 禁掉了 TCP 转发 */
  warnings: string[]
}

export interface DestCheckResult {
  server: string
  port: number
  usable: boolean
  tls13: boolean
  curve_name: string
  alpn: string
  max_record_size: number
  record_sizes: number[]
  cert_issuer: string
  cert_chain_len: number
  problems: string[]
  warnings: string[]
  checked_at: string
}

/**
 * TCP 调优单项的状态。
 *
 * UNSUPPORTED 与 READONLY 不能混:前者是这个内核压根没有这个键,后者是
 * 键在但由宿主机控制(容器)。两者都不是故障,但给管理员的结论不一样 ——
 * 前者换内核才有,后者换机器才有。
 */
export type TuneState =
  | 'PENDING'
  | 'SAME'
  | 'APPLIED'
  | 'UNSUPPORTED'
  | 'READONLY'
  | 'FAILED'

export interface TuneItem {
  group: string
  key: string
  desired: string
  current: string
  /** 这个数字是怎么算出来的。它同时被写进节点上的 conf 文件 */
  reason: string
  state: TuneState
  detail: string
}

export interface TuneFacts {
  os_name: string
  kernel: string
  virt: string
  init_system: string
  mem_total_kb: number
  cpu_count: number
  disk_total_kb: number
  disk_free_kb: number
  cc_available: string[]
  cc_current: string
  qdisc_now: string
  reserved_now: string
  /** 节点上 sing-box 进程**实际**的句柄上限,进程没跑时为 0 */
  nofile_limit: number
  has_sysctl: boolean
}

export interface TuneReport {
  node_id: number
  mode: 'PREVIEW' | 'APPLY' | 'RESTORE'
  facts: TuneFacts
  profile: string
  items: TuneItem[]
  warnings: string[]
  /** 刻意没做的事及其原因。不显示的话下一个人会以为是漏了 */
  notes: string[]
  conf_path: string
  tuned_at: string
  baseline_present: boolean
  changed: boolean
  summary: string
  generated_at: string
}

// 状态的文案不在这里定义:它与形状、颜色是同一个三重编码的三面,
// 拆开放两处迟早会出现「文案改了形状没改」。见 NodeTuningPanel 的 stateMeta。

export interface DeployStep {
  name: string
  status: 'SUCCESS' | 'FAILED' | 'SKIPPED'
  duration_ms: number
  detail?: string
}

export interface DeployResult {
  node_id: number
  revision: number
  config_sha256: string
  status: 'SUCCESS' | 'FAILED' | 'ROLLED_BACK'
  steps: DeployStep[]
  /** 前置检查发现配置已一致,这次没有重启服务(V20) */
  unchanged?: boolean
  error_message?: string
  rollback_result?: string
  started_at: string
  finished_at: string
}

export interface DeploymentRecord {
  id: number
  node_id: number
  revision: number
  config_sha256: string
  status: string
  started_at: string
  finished_at: string | null
  error_message: string
  rollback_result: string
  steps: DeployStep[]
}

/** 一个 Mieru 入口的「库里想要的」与「节点上跑着的」之差。 */
export interface MieruConfigDiff {
  inbound_id: number
  display_name: string
  /** false 表示这个入口还没上过节点 —— 那不是"有差异",是"还没开始"。 */
  deployed: boolean
  changed: boolean
  /** 参数层面的差异(端口段、传输层、MTU),判据是库里的期望值与 deployed_*。 */
  attrs: string[] | null
  desired_users: string[] | null
  remote_users: string[] | null
  users_added: string[] | null
  users_removed: string[] | null
  /** 读节点那一步失败的原因。不中止整份比对 —— 一台机器上有好几个实例。 */
  error?: string
}

export interface ConfigDiff {
  node_id: number
  revision: number
  desired_sha256: string
  remote_sha256: string
  in_sync: boolean
  desired_users: string[] | null
  /**
   * 这台机器上有没有 sing-box 入口。false 时上面那几项没有意义 ——
   * 界面必须据此换一句话,否则「节点上尚无配置」会被读成"部署丢了"。
   */
  has_singbox: boolean
  /** 逐个 Mieru 入口的比对。与上面那份分开:它们是另一个进程、另一份配置。 */
  mieru: MieruConfigDiff[] | null
  diff: {
    changed: boolean
    users: { added: string[] | null; removed: string[] | null; uuid_reset: string[] | null }
    node_attr: string[] | null
    summary: string
  }
}

export interface DailyPoint {
  day: string
  uplink: number
  downlink: number
  total: number
}

/** 节点在当前额度周期内的用量。周期边界由后端统一计算,前端不自己算。 */
export interface NodeCycleUsage {
  node_id: number
  period_start: string
  /** NONE 周期没有下次重置时间 */
  next_reset_at: string | null
  /** sing-box 的原始计数,永远不乘倍数 —— 它回答的是「代理转发了多少」 */
  uplink_bytes: number
  downlink_bytes: number
  proxy_bytes: number
  /** 折算到主机计费口径之后的量,与 quota_bytes 同口径 */
  used_bytes: number
  quota_bytes: number
  /** 不限量时为 null —— 不能当 0 用,那会画成"剩余 0"的红条 */
  remaining_bytes: number | null
  usage_percent: number | null
  unlimited: boolean
  exceeded: boolean
  warning_level: 'UNLIMITED' | 'NORMAL' | 'WARNING' | 'DANGER' | 'EXCEEDED'
  reset_cycle: 'NONE' | 'MONTHLY'
  reset_day: number
  /** EGRESS 只计出站(×1);BOTH 进出合计(×2) */
  billing_mode: NodeBillingMode
  billing_factor: number
}

export interface UserNodeTraffic {
  user_code: string
  node_id: number
  node_name: string
  uplink: number
  downlink: number
  total: number
}

/** 区间统计的实际范围(V20)。 */
export interface UserTrafficRange {
  from: string
  to: string
  granularity: 'hour' | 'day'
  source: 'ledger' | 'daily'
  /** 最近一条入账的时间,空串表示从没入过账 */
  updated_at: string
}

export interface UserTrafficTotal {
  uplink: number
  downlink: number
  total: number
}

export interface UserNodeRange {
  node_id: number
  node_name: string
  node_sort_order: number
  deleted: boolean
  uplink: number
  downlink: number
  total: number
}

export interface UserNodeNoData {
  node_id: number
  node_name: string
  reason: 'no_records' | 'collect_failed'
  detail?: string
}

export interface UserTraffic {
  /** 区间统计(V20):合计、趋势、按节点明细都只看这一段 */
  range: UserTrafficRange
  total: UserTrafficTotal
  series: TrafficSeriesPoint[]
  no_data: UserNodeNoData[]
  user_code: string
  used_uplink: number
  used_downlink: number
  used_total: number
  quota_bytes: number
  by_node: UserNodeRange[]
  daily: DailyPoint[]
}

export interface SyncResult {
  node_id: number
  batch_id: string
  restarted: boolean
  counters_read: number
  entries_added: number
  bytes_added: number
  synced_at: string
  /** 主机流量(vnStat)那一路的结果,只有手工「同步流量」会带上它 */
  host?: HostTrafficSyncResult
}

/** 主机流量(vnStat)一次同步 / 安装的结果(V15) */
export interface HostTrafficSyncResult {
  installed: boolean
  iface: string
  version: string
  rows_upserted: number
  install_steps: string[]
  total_rx: number
  total_tx: number
}

export interface HostTrafficState {
  node_id: number
  installed: boolean
  iface: string
  vnstat_version: string
  /** 空串表示从没同步过 */
  synced_at: string
  last_error: string
}

/** 代理流量按粒度聚合的一个桶。at 是桶起点(UTC RFC3339)。 */
export interface TrafficSeriesPoint {
  at: string
  uplink: number
  downlink: number
  total: number
}

/** 主机流量的一个桶(网卡视角) */
export interface HostTrafficPoint {
  at: string
  rx: number
  tx: number
  total: number
}

export interface NodeTrafficSeries {
  node_id: number
  granularity: 'HOUR' | 'DAY' | 'MONTH'
  limit: number
  metered: boolean
  proxy: TrafficSeriesPoint[]
  host: HostTrafficPoint[]
  host_state: HostTrafficState | null
}

/** /proc/net/dev 的一次即时读数:累计值,速率由前端按两次读数之差算 */
export interface HostLiveSample {
  iface: string
  rx_bytes: number
  tx_bytes: number
  at: string
}

export interface TrafficStatus {
  /** 上一轮【跑完】的时刻;面板启动以来还没跑完过一轮时没有这个字段。 */
  last_run?: string
  /** 当前或上一轮开始的时刻。 */
  last_started?: string
  /** 此刻是否有一轮正在跑。旧后端没有这个字段。 */
  running?: boolean
  failing_nodes: { node_id: number; error: string; failed_at?: string }[]
}

// ---------- 续期与调整 ----------

export type AdjustAction =
  | 'ADD_QUOTA'
  | 'SET_QUOTA'
  | 'RESET_TRAFFIC'
  | 'EXTEND_EXPIRY'
  | 'SET_EXPIRY'
  | 'CHANGE_TIER'
  | 'ENABLE_USER'
  | 'DISABLE_USER'

export interface AdjustmentRecord {
  id: number
  proxy_user_id: number
  action: AdjustAction
  action_text: string
  quota_delta_bytes: number
  expiry_delta_days: number
  before_json: string
  after_json: string
  remark: string
  admin_user_id: number | null
  created_at: string
}

/** 用户门户能看到的版本:没有管理员 ID,也没有前后 JSON */
export interface PublicAdjustment {
  action: AdjustAction
  action_text: string
  quota_delta_bytes: number
  expiry_delta_days: number
  remark: string
  created_at: string
}

export interface AdjustPayload {
  action: AdjustAction
  quota_delta_bytes?: number
  quota_bytes?: number
  expiry_delta_days?: number
  expires_at?: string
  access_tier_id?: number
  remark?: string
}

export interface BatchAdjustResult {
  total: number
  succeeded: number
  items: { user_id: number; ok: boolean; error?: string }[]
}

export interface DashboardAlert {
  level: 'warning' | 'error'
  /**
   * V17 起多了云账号一类(CDT 用量超阈值 / 查询失败);外部代理的源与条目
   * (到期、同步连续失败)早在 V4 就会出现在这里,类型上一直漏写了这两种。
   */
  category: 'user' | 'node' | 'cloud_account' | 'proxy_source' | 'external_proxy'
  target: string
  target_id: number
  message: string
}

// ---------- 用户门户 ----------

export interface PortalAccount {
  id: number
  proxy_user_id: number
  username: string
  login_enabled: boolean
  must_change_password: boolean
  last_login_at: string | null
  last_login_ip: string
  session_count: number
  created_at: string
  updated_at: string
}

export interface PortalIdentity {
  username: string
  display_name: string
  must_change_password: boolean
}

export interface PortalAlert {
  level: 'info' | 'warning' | 'error'
  message: string
}

export interface PortalDashboard {
  display_name: string
  user_code: string
  tier_name: string
  tier_code: string
  status: UserStatus
  status_text: string
  serviceable: boolean
  reason: string
  used_uplink: number
  used_downlink: number
  used_total: number
  quota_bytes: number
  remaining: number
  /** 不限量时为 null,前端据此显示「不限量」而不是 0% */
  used_percent: number | null
  expires_at: string | null
  remaining_days: number | null
  last_reset_at: string | null
  next_reset_at: string | null
  node_count: number
  alerts: PortalAlert[]
}

export interface PortalNode {
  id: number
  display_name: string
  tier_name: string
  tier_code: string
  status: 'normal' | 'maintenance' | 'disabled'
  protocol: string
  public_port: number
  public_remark: string
  maintenance_message: string
  in_subscription: boolean
  /** 这个入口在订阅里会多出一条 IPv6 条目;地址本身不下发给用户 */
  supports_ipv6: boolean
  /** 不计流量的入口:用它的流量不扣额度(按节点上已生效的配置判断) */
  unmetered: boolean
  today_bytes: number
  month_bytes: number
  total_bytes: number
  last_seen_at: string | null
}

export interface PortalNodeShare {
  node_id: number
  display_name: string
  uplink: number
  downlink: number
  total: number
  percent: number
}

export interface PortalTraffic {
  days: number
  daily: DailyPoint[]
  by_node: PortalNodeShare[]
  total: number
  uplink: number
  downlink: number
}

export interface PortalSubscription {
  available: boolean
  reason: string
  base_url: string
  url_base64: string
  url_uri: string
  url_singbox: string
  /**
   * mihomo(Clash.Meta)原生 YAML。
   *
   * 与 url_base64 不是同一份东西的两种包装:通用订阅是分享链接的列表,
   * 而有些协议根本没有通用的分享链接,只有这一条路能把它们送到 Clash 用户手上。
   */
  url_clash: string
  /** 物理节点数,与「我的节点」的条数一致 */
  node_count: number
  ipv6_count: number
  /** 订阅文件里的条目数:配了 IPv6 的节点会多一条 */
  entry_count: number
  last_access_at: string | null
  access_count: number
  /**
   * 管理员配置的配置文件。一份都没配时是空数组,这一整块在页面上不出现 ——
   * 不显示灰掉的按钮,也不显示「暂未配置」:用户对此做不了任何事。
   */
  profiles: PortalProfileLink[]
}

/**
 * 门户里的一份配置文件。
 *
 * 刻意没有的东西:正文、内部名称、内部备注。
 * available 为假时链接照常给出(它本身没变),但前端只展示原因。
 */
export interface PortalProfileLink {
  id: number
  kind: ProfileKind
  name: string
  description: string
  filename: string
  url: string
  available: boolean
  reason: string
}

export interface PortalSession {
  id: number
  created_at: string
  expires_at: string
  last_seen_at: string
  client_ip: string
  user_agent: string
  current: boolean
}


/**
 * 门户里的一条外部代理。
 *
 * 与 PortalNode 分开而不是混进同一个数组:混在一起的话它的流量字段只能填 0,
 * 而 0 与「真的没用过」长得一模一样。
 *
 * 刻意没有的东西:服务器地址与端口、**来源(哪个机场)**、任何流量数字。
 * 来源这一条尤其重要 —— 用户知道了没有用处,只会引出
 * 「那我能不能自己去买」和「你加价了多少」。
 */
export interface PortalExternalNode {
  id: number
  display_name: string
  tier_name: string
  tier_code: string
  status: 'normal' | 'maintenance' | 'disabled'
  public_remark: string
  maintenance_message: string
  in_subscription: boolean
}

/**
 * 门户里的一条中转线路。
 *
 * 与 PortalNode、PortalExternalNode 都分开:
 *   - 不并进节点 —— 那一组每行都有流量数字,而中转主机上跑的是 nginx,
 *     它不接 V2Ray API,面板在那台机器上拿不到任何计数。混进去只能填 0,
 *     与「真的没用过」长得一模一样;
 *   - 不并进外部代理 —— 那一组是「买来的成品线路」,而中转的凭据是我们发的。
 *
 * 刻意没有的东西:中转主机与落地的地址、端口、协议参数,以及**落地是谁**。
 * 最后一条是内部拓扑,用户知道了只会引出「那我能不能直接连落地」。
 */
export interface PortalRelayNode {
  id: number
  display_name: string
  tier_name: string
  tier_code: string
  status: 'normal' | 'maintenance'
  public_remark: string
  in_subscription: boolean
}

// ---------- 门户探针 ----------

/** 点阵的一个点:只有时间与粗粒度结果。没发起的几种对用户都是「没有数据」(NONE),不按失败画。 */
export interface PortalProbeDot {
  at: string
  state: 'OK' | 'PARTIAL' | 'FAIL' | 'HTTP5XX' | 'NONE'
}

/** 门户卡片上的一个探测项。白名单:没有地址、端口、来源机器与内部原因。 */
export interface PortalProbeItem {
  name: string
  metric: ProbeMetric
  state: 'OK' | 'LOSSY' | 'FAILING' | 'DOWN' | 'HTTP_5XX' | 'PAUSED' | 'NO_DATA'
  rtt_ms: number | null
  last_label: string
  h1_rate: number | null
  h24_rate: number | null
  dots: PortalProbeDot[]
  dot_seconds: number
}

/** 门户探针的一台机器。只有百分比,没有内存 / 磁盘的绝对用量,也没有流量、网速与套餐。 */
export interface PortalProbeCard {
  name: string
  region: string
  tags: string[]
  status: 'online' | 'offline' | 'disabled'
  uptime_seconds: number | null
  uptime_at: string
  resources: {
    cpu: number | null
    mem: number | null
    disk: number | null
    stale: boolean
    collected_at: string
  } | null
  ingress: PortalProbeItem[]
  outbound: PortalProbeItem[]
}

export const portalApi = {
  login: (username: string, password: string) =>
    request<PortalIdentity>('/api/portal/auth/login', {
      method: 'POST',
      body: { username, password },
    }),
  logout: () => request<{ message: string }>('/api/portal/auth/logout', { method: 'POST' }),
  me: () => request<PortalIdentity>('/api/portal/auth/me'),
  changePassword: (oldPassword: string, newPassword: string) =>
    request<{ message: string }>('/api/portal/auth/password', {
      method: 'POST',
      body: { old_password: oldPassword, new_password: newPassword },
    }),
  sessions: () => request<{ items: PortalSession[] }>('/api/portal/auth/sessions'),
  revokeSession: (id: number) =>
    request<{ message: string }>(`/api/portal/auth/sessions/${id}`, { method: 'DELETE' }),
  logoutAll: () =>
    request<{ message: string }>('/api/portal/auth/logout-all', { method: 'POST' }),

  dashboard: () => request<PortalDashboard>('/api/portal/dashboard'),
  nodes: () =>
    request<{
      items: PortalNode[]
      external: PortalExternalNode[]
      relays: PortalRelayNode[]
    }>('/api/portal/nodes'),
  traffic: (days = 30) => request<PortalTraffic>('/api/portal/traffic', { query: { days } }),
  probe: () => request<{ generated_at: string; items: PortalProbeCard[] }>('/api/portal/probe'),
  subscription: () => request<PortalSubscription>('/api/portal/subscription'),
  regenerateSubscription: () =>
    request<PortalSubscription>('/api/portal/subscription/regenerate', { method: 'POST' }),
  adjustments: () => request<{ items: PublicAdjustment[] }>('/api/portal/adjustments'),
}

// ---------- 接口 ----------


/* ---------------- 外部代理 ---------------- */

/**
 * 外部代理:不属于本面板、不由本面板部署的成品线路。
 *
 * 与自建节点只有「能被用户连」这一点相同 —— 没有 SSH、不能部署、
 * **统计不到流量**(流量走的是上游的服务器)。
 */
export type ExternalProtocol =
  | 'SHADOWSOCKS'
  | 'VMESS'
  | 'VLESS'
  | 'TROJAN'
  | 'HYSTERIA2'
  | 'TUIC'
  | 'UNKNOWN'

/** 给人看的协议名。程序内一律用常量判断,不要拿它做判断 —— 展示名以后会改。 */
export const EXTERNAL_PROTOCOL_LABEL: Record<ExternalProtocol, string> = {
  SHADOWSOCKS: 'Shadowsocks',
  VMESS: 'VMess',
  VLESS: 'VLESS',
  TROJAN: 'Trojan',
  HYSTERIA2: 'Hysteria2',
  TUIC: 'TUIC',
  UNKNOWN: '未知协议',
}

/** ACTIVE 正常 / DISABLED 手工停用 / EXCLUDED 上游有但我不要 */
export type ExternalProxyStatus = 'ACTIVE' | 'DISABLED' | 'EXCLUDED'

export type ExternalOrigin = 'MANUAL' | 'IMPORTED'

export interface ExternalProxy {
  id: number
  /** null 表示手工添加,不参与任何同步 */
  source_id: number | null
  source_name: string
  name_prefix: string
  /** 内部名称,唯一;删除确认时要输入的就是它 */
  name: string
  /** 不含前缀的展示名 */
  display_name: string
  /** 管理员改过的名字。非空时完全取代「前缀 + 展示名」 */
  display_name_override: string
  /** 上游给的原始名称,同步匹配的二级键 */
  raw_name: string
  /** 后端拼好的最终展示名 —— 与订阅里下发的是同一个 */
  final_display_name: string
  protocol: ExternalProtocol
  server: string
  port: number
  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number
  subscription_enabled: boolean
  sort_order: number
  public_remark: string
  maintenance_message: string
  expires_at: string | null
  origin: ExternalOrigin
  identity_key: string
  locked_fields: string
  /** 已锁定的字段列表,同步时不会被上游覆盖 */
  locked_list: string[]
  /** 上游连续多少轮没出现。达到阈值自动退出订阅,但永不自动删除 */
  missing_rounds: number
  missing_since: string | null
  last_seen_at: string | null
  status: ExternalProxyStatus
  last_check_at: string | null
  last_check_ok: boolean | null
  last_check_message: string
  last_check_latency_ms: number | null
  /**
   * 能不能把它当成某个入口的「出口」。
   *
   * 说的不是面板认不认识这个协议 —— Hysteria2 与 TUIC 走 QUIC,而节点上的
   * sing-box 是精简构建(不含 with_quic),拨不动它们。这两种线路照常进订阅、
   * 用户自己的客户端照常能用,只有「让我们的节点去连它」做不了。
   * 由后端算:构建选项前端没有办法知道。
   */
  dialable_by_node: boolean
  /**
   * dialable_by_node 为假时的原因,一句话。不只按协议判:一条 SS 线路的
   * 插件名 sing-box 不认、或 SS2022 密钥长度不对,协议本身"能拨",
   * 而部署会在 check 那一步 FATAL —— 后端按渲染出站的同一条路算出来。
   */
  dialable_reason: string
  /** 能不能用 nginx 透传到它。QUIC 系是纯 UDP,而 stream 这边只搬 TCP 字节 */
  relayable: boolean
  created_at: string
  updated_at: string
  /**
   * 供应商到期档案(V20)。与上面的 expires_at 是两件事:那个是面板控制的
   * 「可用截止时间」(到期即退出订阅),这个是商家账单什么时候到期(只提醒)。
   * 跟随来源的条目 inherited 为真。
   */
  expiry?: ExpiryView | null
}

/**
 * 切换「配置不落盘」的结果。
 *
 * 与链式切换同一个形状:多阶段、可能中途失败,而失败时最要紧的信息
 * 是**停在哪一步**。
 */
export interface ConfigRAMResult {
  enabled: boolean
  stage: string
  /** /run 实测到的文件系统类型。不是 tmpfs 就不让开 */
  runtime_fs?: string
  deploy?: DeployResult
  /** 被清掉的旧位置文件。让管理员看得见"磁盘上那份真的没了" */
  cleaned: string[]
}

/** 推送事件类型。程序内用常量判断,展示名从 available_kinds 里取。 */
export type NotifyKind =
  | 'SERVICE_DOWN'
  | 'SERVICE_RECOVERED'
  | 'RECOVER_FAILED'
  | 'DEPLOY_FAILED'
  | 'NODE_QUOTA'
  | 'CLOUD_THRESHOLD'
  | 'CLOUD_POWER'
  | 'CLOUD_QUERY_FAILED'
  | 'EXPIRY_SOON'
  | 'EXPIRY_AUTO_RENEW'
  | 'EXPIRY_OVERDUE'
  | 'PROBE_DOWN'
  | 'PROBE_RECOVERED'
  | 'BOCE_ALERT'
  | 'BOCE_BUDGET'

/**
 * 推送设置。
 *
 * **凭据不在里面。** Bark 的整条地址与 Telegram 的 bot token / 代理密钥
 * 都是凭据(设备 key 与 token 在路径里),后端的结构体上打了 json:"-",
 * 所以它们根本没有位置可填。要知道配没配过,看 *_configured。
 */
export interface NotifySettings {
  enabled: boolean
  bark_enabled: boolean
  bark_group: string
  bark_sound: string
  bark_configured: boolean
  telegram_enabled: boolean
  telegram_chat_id: string
  telegram_thread_id: string
  telegram_configured: boolean
  /** 空数组表示全开 —— 新加的事件类型会自动被收到 */
  kinds: NotifyKind[]
  auto_recover: boolean
  available_kinds: { kind: NotifyKind; label: string }[]
}

/** 单个渠道的发送结果。 */
export interface NotifyResult {
  channel: string
  ok: boolean
  error?: string
}

/** 巡检里一个服务的状态。 */
export type ServiceState = 'RUNNING' | 'STOPPED' | 'UNREACHABLE' | 'NOT_APPLICABLE'

/**
 * 一台机器的巡检结果。
 *
 * UNREACHABLE 与 STOPPED 严格分开:前者是「SSH 连不上,服务是死是活
 * 我们并不知道」,后者是「服务定义在、进程没跑」。混为一谈会让管理员
 * 在一次正常重启后收到"服务停了"。
 */
/** 一个 Mieru 入口(= 一个 mita 实例)的巡检状态。 */
export interface MieruServiceReport {
  inbound_id: number
  display_name: string
  state: ServiceState
  detail: string
}

export interface NodeHealth {
  node_id: number
  node_name: string
  checked_at: string
  singbox: ServiceState
  singbox_detail: string
  nginx: ServiceState
  nginx_detail: string
  /** realm 转发(V15),与 nginx 平行。 */
  realm: ServiceState
  realm_detail: string
  /**
   * 每个 Mieru 入口(= 一个 mita 实例)的状态。
   *
   * **逐实例给,不能合成一个。** 它们各自独立地跑与崩 —— 把"有一个在跑"
   * 算成正常的话,挂掉的那个再也不会被发现;合成一句"Mieru 没在跑"
   * 又看不出该查哪一个。
   */
  mieru: MieruServiceReport[] | null
  recovered: boolean
  recover_error?: string
  fail_streak: number
}

export type ProxySourceSyncStatus = 'NEVER' | 'OK' | 'FAILED'

export interface ProxySource {
  id: number
  name: string
  /** 订阅地址不随列表返回 —— 它含 token,等同密码 */
  has_url: boolean
  name_prefix: string
  default_access_tier_id: number
  default_subscription_enabled: boolean
  auto_sync_enabled: boolean
  sync_interval_minutes: number
  expires_at: string | null
  /** 上游给的数字。**只在这一页展示**,不进任何用户视图 */
  upstream_used_bytes: number
  upstream_total_bytes: number
  upstream_expires_at: string | null
  upstream_seen_at: string | null
  last_sync_at: string | null
  last_sync_status: ProxySourceSyncStatus
  last_sync_message: string
  last_sync_added: number
  last_sync_updated: number
  last_sync_missing: number
  last_sync_skipped: number
  consecutive_failures: number
  enabled: boolean
  remark: string
  sort_order: number
  proxy_count: number
  created_at: string
  updated_at: string
  /** 供应商到期档案(V20);源下跟随来源的条目都按它提醒,按源只发一次 */
  expiry?: ExpiryView | null
}

export interface ProxyPreviewItem {
  identity_key: string
  name: string
  protocol: string
  server: string
  port: number
  method: string
  /** 默认勾选状态。疑似公告的条目默认不勾 */
  suggested: boolean
  /** 疑似公告条目。仍然列出 —— 识别规则一定会误伤 */
  announcement: boolean
  /** 库里已经有这一条,导入时走「更新」 */
  existing: boolean
}

export interface ProxySkippedGroup {
  protocol: string
  label: string
  count: number
}

export interface ProxyPreviewResult {
  format: string
  format_label: string
  items: ProxyPreviewItem[]
  skipped: ProxySkippedGroup[]
  parse_errors: string[]
  upstream: {
    used_bytes: number
    total_bytes: number
    expires_at: string | null
  } | null
}

export interface ProxySyncResult {
  added: number
  updated: number
  unchanged: number
  missing: number
  skipped: number
  /** 本次因连续消失而自动退出订阅的条目名 */
  unlisted: string[]
  skipped_by_protocol: ProxySkippedGroup[]
  parse_errors: string[]
}

export interface ProxyCheckResult {
  ok: boolean
  message: string
  latency_ms: number
  /** 后端一起下发,免得前端某处忘了写这句 */
  disclaimer: string
}

/** 可锁定的字段与它们的中文名。server/port/凭据不可锁定 —— 那是上游的事实。 */
export const LOCKABLE_FIELD_LABEL: Record<string, string> = {
  display_name: '展示名称',
  access_tier_id: '访问等级',
  subscription_enabled: '下发订阅',
  sort_order: '排序',
  public_remark: '公开备注',
}

/* ---------------- 配置文件订阅 ---------------- */

/**
 * 配置文件订阅:管理员上传整份客户端配置,面板按用户替换占位符。
 *
 * 与节点订阅的分工 —— 节点订阅给的是一串节点,这个给的是一整份带
 * 分流规则、DNS 与入站的配置。系统里不预置任何模板。
 */
export type ProfileKind = 'SINGBOX' | 'CLASH' | 'SHADOWROCKET'

export const profileKindLabel: Record<ProfileKind, string> = {
  SINGBOX: 'sing-box',
  CLASH: 'Clash / mihomo',
  SHADOWROCKET: '小火箭',
}

/** 每种类型在客户端里的用法不同,这句话直接决定用户会不会用错。 */
export const profileKindHint: Record<ProfileKind, string> = {
  SINGBOX: '整份配置里就是节点本身,导入后不需要再单独加节点订阅',
  CLASH: '配置里的 proxy-providers 自己去拉节点,面板只替换其中的订阅地址',
  SHADOWROCKET: '只有规则,节点要另外用「Clash 配置」地址添加(通用订阅里没有 Snell)',
}

export interface SubscriptionProfile {
  id: number
  kind: ProfileKind
  name: string
  display_name: string
  filename: string
  /** 只有详情接口带正文;列表里是 undefined,不是空字符串 */
  content?: string
  content_bytes: number
  singbox_landing_detour: string
  description: string
  remark: string
  enabled: boolean
  sort_order: number
  created_at: string
  updated_at: string
}

export interface ProfilePlaceholder {
  name: string
  description: string
  kinds: ProfileKind[]
  once: boolean
}

export interface ProfilePlaceholderInfo {
  items: ProfilePlaceholder[]
  default_filenames: Record<ProfileKind, string>
  max_bytes: number
  landing_keywords: string[]
}

export interface ProfilePreview {
  rendered: string
  /** 语法自检。不影响保存 —— 我们的检查一定比客户端严格 */
  warning: { line: number; message: string } | null
  sample_used: boolean
  user_code: string
  node_count: number
  landing_count: number
}

export const api = {
  login: (username: string, password: string) =>
    request<Admin>('/api/auth/login', { method: 'POST', body: { username, password } }),
  logout: () => request<{ message: string }>('/api/auth/logout', { method: 'POST' }),
  me: () => request<Admin>('/api/auth/me'),
  changePassword: (oldPassword: string, newPassword: string) =>
    request<{ message: string }>('/api/auth/password', {
      method: 'POST',
      body: { old_password: oldPassword, new_password: newPassword },
    }),

  dashboardSummary: () => request<DashboardSummary>('/api/dashboard/summary'),
  dashboardAlerts: () => request<{ items: DashboardAlert[] }>('/api/dashboard/alerts'),

  auditLogs: (params: { limit?: number; targetType?: string; targetId?: string } = {}) =>
    request<{ items: AuditLog[] }>('/api/audit-logs', {
      query: {
        limit: params.limit ?? 100,
        target_type: params.targetType,
        target_id: params.targetId,
      },
    }),

  // 用户
  users: () => request<{ items: ProxyUser[] }>('/api/users'),
  user: (id: number) => request<ProxyUser>(`/api/users/${id}`),
  createUser: (body: Record<string, unknown>) =>
    request<ProxyUser>('/api/users', { method: 'POST', body }),
  updateUser: (id: number, body: Record<string, unknown>) =>
    request<ProxyUser>(`/api/users/${id}`, { method: 'PATCH', body }),
  deleteUser: (id: number) =>
    request<{ message: string }>(`/api/users/${id}`, { method: 'DELETE' }),
  setUserEnabled: (id: number, enabled: boolean) =>
    request<ProxyUser>(`/api/users/${id}/enabled`, { method: 'POST', body: { enabled } }),
  resetUserTraffic: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/reset-traffic`, { method: 'POST' }),
  regenerateUserUUID: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-uuid`, { method: 'POST' }),
  /** 只影响跑 Shadowsocks 的节点 —— UUID 不出现在它们的配置里,反之亦然。 */
  regenerateUserSSPassword: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-ss-password`, { method: 'POST' }),
  /**
   * 只影响跑 Snell 的节点(V14)。
   *
   * 与另外两把是三件独立的事 —— 复用一把的话,点"重置 Snell 凭据"
   * 会连带作废这个人的 VLESS 与 Shadowsocks 访问。
   */
  regenerateUserSnellKey: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-snell-key`, { method: 'POST' }),
  regenerateSubToken: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-sub-token`, { method: 'POST' }),
  /** 区间 [from, to) 为 RFC3339;两者都不传时按 days 回落。 */
  userTraffic: (id: number, opts: { days?: number; from?: string; to?: string } = {}) =>
    request<UserTraffic>(`/api/users/${id}/traffic`, {
      query: { days: opts.from ? undefined : (opts.days ?? 30), from: opts.from, to: opts.to },
    }),

  // 节点
  nodes: () => request<{ items: Node[] }>('/api/nodes'),
  node: (id: number) => request<Node>(`/api/nodes/${id}`),
  createNode: (body: Record<string, unknown>) =>
    request<CreateNodeResult>('/api/nodes', { method: 'POST', body }),
  bootstrapNode: (id: number, rootPassword: string, enablePasswordLogin: boolean) =>
    request<BootstrapResult>(`/api/nodes/${id}/bootstrap`, {
      method: 'POST',
      body: { root_password: rootPassword, enable_password_login: enablePasswordLogin },
    }),
  uninstallNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/uninstall`, { method: 'POST' }),
  panelKey: () => request<{ public_key: string }>('/api/panel-key'),
  updateNode: (id: number, body: Record<string, unknown>) =>
    request<NodeUpdateResponse>(`/api/nodes/${id}`, { method: 'PUT', body }),
  /** 管理地址变更后的全面重检(只读):连接、转发、服务、配置、端口、采集。 */
  recheckNode: (id: number) =>
    request<NodeRecheckResult>(`/api/nodes/${id}/recheck`, { method: 'POST' }),
  deleteNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}`, { method: 'DELETE' }),
  /** 就地改排序号(列表「编号」列,V20)。只影响订阅与门户里的先后。 */
  setNodeSortOrder: (id: number, sortOrder: number) =>
    request<{ sort_order: number }>(`/api/nodes/${id}/sort-order`, { method: 'PUT', body: { sort_order: sortOrder } }),
  /**
   * 改任意一种条目的排序(V20 入口管理页):NODE 是节点排序号,SINGBOX / MIERU / NGINX / REALM
   * 是节点内的入口序号,EXTERNAL 是外部代理自带的全局排序值。只影响订阅与门户里的先后。
   */
  setEntrySortOrder: (kind: OrderPlanItem['kind'], id: number, sortOrder: number) =>
    request<{ kind: string; id: number; sort_order: number }>(`/api/sort-order/${kind}/${id}`, {
      method: 'PUT',
      body: { sort_order: sortOrder },
    }),
  /** 旧排序 → 全局排序值的迁移计划(只算不写)。 */
  orderMigrationPlan: () => request<OrderPlan>('/api/subscription/order-migration'),
  /** 执行迁移并把方案切到 GLOBAL;计划里有错误时返回 409。 */
  applyOrderMigration: () => request<OrderPlan>('/api/subscription/order-migration', { method: 'POST' }),
  setNodeEnabled: (id: number, enabled: boolean) =>
    request<{ message: string }>(`/api/nodes/${id}/enabled`, { method: 'POST', body: { enabled } }),
  testNodeSSH: (id: number) =>
    request<{ ok: boolean; uname: string; resolved_ip: string }>(
      `/api/nodes/${id}/test-ssh`,
      { method: 'POST' },
    ),
  probeNode: (id: number) => request<ProbeResult>(`/api/nodes/${id}/probe`, { method: 'POST' }),
  /**
   * 安装 sing-box。面板只分发一支,已经装过的机器上再点就是换成这一版。
   */
  installNode: (id: number) =>
    request<{
      binary_path: string
      binary_sha256: string
      service_name: string
      /** 装完之后在节点上读出来的版本 */
      version: string
      /**
       * false 表示节点上已经是同一个二进制,什么都没换。true 而服务正在跑时,
       * 跑着的仍是旧的那一个,要等它下一次启动(强制重新下发、重启都算;配置没变时
       * 普通下发会跳过重启,不算)才换过去。
       */
      uploaded: boolean
      init_system: string
      installed: boolean
      detail: string
      /** 安装时顺带检查并打开的 sshd TCP 转发 */
      tcp_forwarding: {
        allowed: boolean
        changed: boolean
        config_path: string
        detail: string
      }
    }>(`/api/nodes/${id}/install`, { method: 'POST' }),
  /**
   * 从这台机器的出口实测一个握手目标。**只检测,不写入。**
   *
   * 写入必须指名道姓地写到某一个入口上(applyInboundDest)—— 多入站之后
   * 一台机器上可以有两个 REALITY 入口,各自指向不同的目标,
   * 而「写到这个节点上」已经不再指向一个确定的对象。
   */
  checkNodeDest: (id: number, dest: string) =>
    request<DestCheckResult>(`/api/nodes/${id}/dest-check`, {
      method: 'POST',
      body: { dest, port: 443 },
    }),
  scanNodeDests: (id: number) =>
    request<{ items: DestCheckResult[] }>(`/api/nodes/${id}/dest-scan`, { method: 'POST' }),
  /** force 为真时配置已一致也照样重启;默认一致就跳过重启。 */
  deployNode: (id: number, opts: { force?: boolean } = {}) =>
    request<DeployResult>(`/api/nodes/${id}/deploy`, { method: 'POST', body: { force: !!opts.force } }),
  restartNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/restart`, { method: 'POST' }),
  resetNodeHostKey: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/reset-host-key`, { method: 'POST' }),
  nodeDeployments: (id: number, limit = 20) =>
    request<{ items: DeploymentRecord[] }>(`/api/nodes/${id}/deployments`, { query: { limit } }),
  nodeConfigDiff: (id: number) => request<ConfigDiff>(`/api/nodes/${id}/config-diff`),
  nodeTraffic: (id: number, days = 30) =>
    request<{
      node_id: number
      /** 中转主机上没有任何计数,这里是 null 而不是一行 0 */
      cycle: NodeCycleUsage | null
      daily: DailyPoint[]
      /**
       * 这台机器是否被面板计流量。
       *
       * 中转主机为 false —— 它上面跑的是 nginx,接不了统计接口。
       * 界面要据此显示「面板不计流量」,而不是画成「读不到」——
       * 后者带一个永远好不了的重试按钮。
       */
      metered?: boolean
      reason?: string
    }>(`/api/nodes/${id}/traffic`, { query: { days } }),
  destCandidates: () =>
    request<{ items: string[]; max_record_size: number }>('/api/dest-candidates'),

  // sing-box 入口(V8 多入站)。一台落地机器可以有多个入口,
  // 各自的协议、端口、访问等级与出口去向互不相干。
  //
  // 增删改一律【不自动部署】:那会重启 sing-box,把这台机器上全部入口的
  // 在线连接一起踢掉,而管理员做的只是动其中一个。界面上写明「下次部署后生效」。
  nodeInbounds: (nodeID: number) =>
    request<{ items: NodeInbound[] }>(`/api/nodes/${nodeID}/inbounds`),
  createInbound: (nodeID: number, body: Record<string, unknown>) =>
    request<{ inbound: NodeInbound; needs_deploy: boolean }>(`/api/nodes/${nodeID}/inbounds`, {
      method: 'POST',
      body,
    }),
  updateInbound: (id: number, body: Record<string, unknown>) =>
    request<{ inbound: NodeInbound; needs_deploy: boolean }>(`/api/inbounds/${id}`, {
      method: 'PUT',
      body,
    }),
  deleteInbound: (id: number) =>
    request<{ deleted: boolean; needs_deploy: boolean }>(`/api/inbounds/${id}`, {
      method: 'DELETE',
    }),
  // Mieru 入口走独立路由:它与 sing-box 入站的 id 空间会撞
  // (两张表各自从 1 开始),共用一条路由的话,请求会被打到另一类对象上。
  // 增删改同样【不自动下发】—— 下发会重启 mita,踢掉这台机器上全部 Mieru 连接。
  nodeMieruInbounds: (nodeID: number) =>
    request<{
      items: MieruInbound[]
      transports: MieruTransport[]
      multiplexings: MieruMultiplexing[]
    }>(`/api/nodes/${nodeID}/mieru-inbounds`),
  createMieruInbound: (nodeID: number, body: Record<string, unknown>) =>
    request<{ inbound: MieruInbound; needs_deploy: boolean }>(
      `/api/nodes/${nodeID}/mieru-inbounds`,
      { method: 'POST', body },
    ),
  updateMieruInbound: (id: number, body: Record<string, unknown>) =>
    request<{ inbound: MieruInbound; needs_deploy: boolean }>(`/api/mieru-inbounds/${id}`, {
      method: 'PUT',
      body,
    }),
  deleteMieruInbound: (id: number) =>
    request<{ needs_deploy: boolean }>(`/api/mieru-inbounds/${id}`, { method: 'DELETE' }),
  /**
   * 把 mita 与 mieru 客户端装到一台机器上。
   *
   * 与「安装 sing-box」并列的一个动作,不是它的一部分:两者装的是不同的
   * 二进制、不同的服务,失败的原因也完全不同(比如这台机器缺 unshare)。
   */
  installMieru: (nodeID: number) =>
    request<{ result: MieruInstallResult }>(`/api/nodes/${nodeID}/mieru-install`, {
      method: 'POST',
      body: {},
    }),
  /**
   * 下发一个 Mieru 入口。
   *
   * usersOnly 为真时只 reload —— **一条连接都不断**。它只在"这次改动只动了
   * 用户"时才成立:端口、传输层与出口的变更 reload 生效不了,而 reload
   * 不释放旧端口 —— 新旧两段会同时监听,旧端口上那个入口还在服务。
   */
  deployMieruInbound: (id: number, usersOnly = false) =>
    request<{ result: DeployResult; error?: string }>(`/api/mieru-inbounds/${id}/deploy`, {
      method: 'POST',
      body: { users_only: usersOnly },
    }),
  setMieruChain: (id: number, body: Record<string, unknown>) =>
    request<{
      inbound: MieruInbound
      needs_deploy: boolean
      needs_singbox_deploy: boolean
    }>(`/api/mieru-inbounds/${id}/chain`, { method: 'POST', body }),
  clearMieruChain: (id: number) =>
    request<{ needs_deploy: boolean; needs_singbox_deploy: boolean }>(
      `/api/mieru-inbounds/${id}/chain`,
      { method: 'DELETE' },
    ),
  /** 实测握手目标并在通过后写入这个入口;不通过时拒绝保存。 */
  applyInboundDest: (id: number, dest: string) =>
    request<{ result: DestCheckResult; error?: string }>(`/api/inbounds/${id}/dest-check`, {
      method: 'POST',
      body: { dest, port: 443 },
    }),

  // 中转:转发规则的增删改只 reload nginx,不打断任何在途连接,
  // 因此这一组接口的操作摩擦比「部署」低一档。
  relays: () => request<{ items: NodeRelay[] }>('/api/relays'),
  // V16:额外订阅地址(地址池)与入口的订阅地址条目。都只影响订阅内容,
  // 不动节点配置 —— 保存后不需要部署。
  nodeAddresses: (nodeID: number) =>
    request<{ items: NodeAddress[] }>(`/api/nodes/${nodeID}/addresses`),
  saveNodeAddresses: (nodeID: number, addresses: NodeAddressInput[]) =>
    request<{ items: NodeAddress[] }>(`/api/nodes/${nodeID}/addresses`, {
      method: 'PUT',
      body: { addresses },
    }),
  inboundEndpoints: (kind: EndpointKind, entryID: number) =>
    request<{ items: InboundEndpoint[] }>(`/api/inbound-endpoints/${kind}/${entryID}`),
  saveInboundEndpoints: (kind: EndpointKind, entryID: number, endpoints: InboundEndpointInput[]) =>
    request<{ items: InboundEndpoint[] }>(`/api/inbound-endpoints/${kind}/${entryID}`, {
      method: 'PUT',
      body: { endpoints },
    }),

  nodeRelays: (nodeID: number) =>
    request<{ items: NodeRelay[] }>(`/api/nodes/${nodeID}/relays`),
  createRelay: (nodeID: number, body: Record<string, unknown>) =>
    request<{ relay: NodeRelay }>(`/api/nodes/${nodeID}/relays`, { method: 'POST', body }),
  updateRelay: (id: number, body: Record<string, unknown>) =>
    request<{ relay: NodeRelay }>(`/api/relays/${id}`, { method: 'PUT', body }),
  deleteRelay: (id: number) =>
    request<{ deleted: boolean }>(`/api/relays/${id}`, { method: 'DELETE' }),
  /**
   * 按服务的安装/卸载。
   *
   * 与整机的 uninstallNode 分开:那一个把三类服务一起摘掉,给的是
   * "这台机器不归面板管了";这几个只动一类,另外两类还在服务用户。
   *
   * 失败时后端仍然返回已经做完的步骤 —— "停了服务但没删定义"与
   * "什么都没做"要人做的事完全不同。
   */
  uninstallSingBox: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(
      `/api/nodes/${nodeID}/singbox-uninstall`,
      { method: 'POST', body: {} },
    ),
  uninstallMieru: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(
      `/api/nodes/${nodeID}/mieru-uninstall`,
      { method: 'POST', body: {} },
    ),
  installNginx: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/nginx-install`, {
      method: 'POST',
      body: {},
    }),
  uninstallNginx: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/nginx-uninstall`, {
      method: 'POST',
      body: {},
    }),
  deployRelays: (nodeID: number) =>
    request<{ result: DeployResult; error?: string }>(`/api/nodes/${nodeID}/relays/deploy`, {
      method: 'POST',
    }),
  /** 只读探测,不会在节点上安装任何东西 */
  nodeNginx: (nodeID: number) => request<NginxFacts>(`/api/nodes/${nodeID}/nginx`),

  // realm(V15):第二种转发引擎。**下发与重启都是 restart**,断开这台机器上
  // 全部 realm 线路的在途连接 —— 这一组的摩擦比 nginx 那一组高一档。
  nodeRealm: (nodeID: number) => request<RealmFacts>(`/api/nodes/${nodeID}/realm`),
  installRealm: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/realm-install`, {
      method: 'POST',
      body: {},
    }),
  uninstallRealm: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/realm-uninstall`, {
      method: 'POST',
      body: {},
    }),
  restartRealm: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/realm-restart`, {
      method: 'POST',
      body: {},
    }),
  stopRealm: (nodeID: number) =>
    request<{ result: ServiceOpResult; error?: string }>(`/api/nodes/${nodeID}/realm-stop`, {
      method: 'POST',
      body: {},
    }),
  deployRealm: (nodeID: number) =>
    request<{ result: DeployResult; error?: string }>(`/api/nodes/${nodeID}/realm/deploy`, {
      method: 'POST',
    }),

  /**
   * 四类服务的现状,只读、一次 SSH。入口 Tab 打开时拉一次,「检查」再拉。
   *
   * 与仪表盘「不主动触发 SSH 采集」不冲突:那条针对的是每次刷新连一遍
   * **全部**机器;这里是管理员点开**一台**机器的入口 Tab,而四张卡片
   * 存在的意义就是这四个状态。
   */
  nodeServices: (nodeID: number) => request<NodeServiceFacts>(`/api/nodes/${nodeID}/services`),
  /**
   * 运维用的直接启停,不重新渲染配置。sing-box 的「重启」仍走 restartNode。
   *
   * 停止都是临时的:巡检看到服务定义在、进程没跑会把它拉起来 ——
   * 这句话在每个确认框里都要写。
   */
  controlService: (
    nodeID: number,
    service: 'singbox' | 'nginx' | 'mieru',
    op: 'start' | 'stop' | 'restart',
  ) =>
    request<{ result: ServiceOpResult; error?: string }>(
      `/api/nodes/${nodeID}/${service}-${op}`,
      { method: 'POST', body: {} },
    ),

  // 链式出站是两台机器的复合操作:启用时先部署落地再部署中转主机,
  // 解除时顺序相反。顺序由后端保证,前端只负责把两次部署的结果都显示出来。
  applyChain: (inboundID: number, body: Record<string, unknown>) =>
    request<{ result: ChainApplyResult; error?: string }>(`/api/inbounds/${inboundID}/chain`, {
      method: 'POST',
      body,
    }),
  clearChain: (inboundID: number) =>
    request<{ result: ChainApplyResult; error?: string }>(`/api/inbounds/${inboundID}/chain`, {
      method: 'DELETE',
    }),

  // TCP 调优。preview 只读,apply/restore 会改节点上的内核参数。
  tuningPreview: (id: number) =>
    request<TuneReport>(`/api/nodes/${id}/tcp-tuning/preview`, { method: 'POST' }),
  tuningApply: (id: number) =>
    request<TuneReport>(`/api/nodes/${id}/tcp-tuning/apply`, { method: 'POST' }),
  tuningRestore: (id: number) =>
    request<TuneReport>(`/api/nodes/${id}/tcp-tuning/restore`, { method: 'POST' }),

  // 流量与部署
  deployments: (limit = 50) =>
    request<{ items: DeploymentRecord[] }>('/api/deployments', { query: { limit } }),
  syncNodeTraffic: (id: number) =>
    request<SyncResult>(`/api/nodes/${id}/sync-traffic`, { method: 'POST' }),
  // V15:流量 Tab 的粒度切换(代理 + 主机两条序列)、实时网卡读数、vnStat 安装。
  nodeTrafficSeries: (id: number, granularity: 'hour' | 'day' | 'month') =>
    request<NodeTrafficSeries>(`/api/nodes/${id}/traffic/series`, { query: { granularity } }),
  nodeHostTrafficLive: (id: number) =>
    request<HostLiveSample>(`/api/nodes/${id}/host-traffic/live`),
  installHostTraffic: (id: number) =>
    request<{ result: HostTrafficSyncResult; summary?: string; error?: string }>(
      `/api/nodes/${id}/host-traffic/install`,
      { method: 'POST', body: {} },
    ),
  trafficStatus: () => request<TrafficStatus>('/api/traffic/status'),

  // 节点资源监控
  nodeMetricsLatest: () => request<{ items: NodeMetrics[] }>('/api/metrics/nodes-latest'),
  /** 只读内存,同时顺延「有页面在看」的租约;interval_seconds 为 0 表示采样关着,不必轮询。 */
  nodeNetRates: () => request<{ interval_seconds: number; items: NetRate[] }>('/api/metrics/net-rates'),
  nodeMetricsHistory: (id: number, hours = 6) =>
    request<{ items: NodeMetrics[]; hours: number }>(`/api/nodes/${id}/metrics`, {
      query: { hours },
    }),
  collectNodeMetrics: (id: number) =>
    request<NodeMetrics>(`/api/nodes/${id}/collect-metrics`, { method: 'POST' }),
  monitorStatus: () => request<MonitorStatus>('/api/metrics/status'),

  // 续期与额度调整
  adjustUser: (id: number, body: AdjustPayload) =>
    request<ProxyUser>(`/api/users/${id}/adjust`, { method: 'POST', body }),
  userAdjustments: (id: number, limit = 50) =>
    request<{ items: AdjustmentRecord[] }>(`/api/users/${id}/adjustments`, { query: { limit } }),
  batchAdjust: (userIds: number[], body: AdjustPayload) =>
    request<BatchAdjustResult>('/api/users/batch-adjust', {
      method: 'POST',
      body: { user_ids: userIds, ...body },
    }),

  // 用户门户账号(管理端)
  setPortalAccount: (
    id: number,
    body: { username: string; password?: string; must_change_password?: boolean },
  ) => request<PortalAccount>(`/api/users/${id}/portal-account`, { method: 'PUT', body }),
  deletePortalAccount: (id: number) =>
    request<{ message: string }>(`/api/users/${id}/portal-account`, { method: 'DELETE' }),
  setPortalLoginEnabled: (id: number, enabled: boolean) =>
    request<PortalAccount>(`/api/users/${id}/portal-login-enabled`, {
      method: 'POST',
      body: { enabled },
    }),
  revokePortalSessions: (id: number) =>
    request<{ message: string }>(`/api/users/${id}/revoke-portal-sessions`, { method: 'POST' }),

  // 访问等级
  accessTiers: () => request<{ items: AccessTier[] }>('/api/access-tiers'),
  updateAccessTier: (id: number, body: Record<string, unknown>) =>
    request<AccessTier>(`/api/access-tiers/${id}`, { method: 'PUT', body }),


  // 外部代理
  externalProxies: (opts: { sourceId?: number | null; includeExcluded?: boolean } = {}) =>
    request<{ items: ExternalProxy[]; excluded_count: number }>('/api/external-proxies', {
      query: {
        source_id: opts.sourceId ?? undefined,
        include_excluded: opts.includeExcluded ? 1 : undefined,
      },
    }),
  externalProxy: (id: number) => request<ExternalProxy>(`/api/external-proxies/${id}`),
  createExternalProxy: (body: Record<string, unknown>) =>
    request<ExternalProxy>('/api/external-proxies', { method: 'POST', body }),
  updateExternalProxy: (id: number, body: Record<string, unknown>) =>
    request<{ proxy: ExternalProxy; effect: { changes: string[]; locked_fields: string[] } }>(
      `/api/external-proxies/${id}`,
      { method: 'PUT', body },
    ),
  deleteExternalProxy: (id: number) =>
    request<void>(`/api/external-proxies/${id}`, { method: 'DELETE' }),
  setExternalProxyStatus: (id: number, status: ExternalProxyStatus) =>
    request<ExternalProxy>(`/api/external-proxies/${id}/status`, {
      method: 'POST',
      body: { status },
    }),
  setExternalProxySubscription: (id: number, enabled: boolean) =>
    request<ExternalProxy>(`/api/external-proxies/${id}/subscription`, {
      method: 'POST',
      body: { enabled },
    }),
  detachExternalProxy: (id: number) =>
    request<ExternalProxy>(`/api/external-proxies/${id}/detach`, { method: 'POST' }),
  setExternalProxyLocks: (id: number, fields: string[]) =>
    request<ExternalProxy>(`/api/external-proxies/${id}/locked-fields`, {
      method: 'POST',
      body: { fields },
    }),
  replaceExternalProxyEndpoint: (id: number, body: Record<string, unknown>) =>
    request<ExternalProxy>(`/api/external-proxies/${id}/endpoint`, { method: 'POST', body }),
  /** 凭据单独取,每次查看都写审计 —— 它是别人家的账号 */
  externalProxyCredentials: (id: number) =>
    request<{
      method: string
      password: string
      plugin: string
      plugin_opts: string
      share_uri: string
    }>(`/api/external-proxies/${id}/credentials`),
  checkExternalProxy: (id: number) =>
    request<ProxyCheckResult>(`/api/external-proxies/${id}/check`, { method: 'POST' }),
  /** 粘贴分享链接解析,不落库。响应里没有密码 */
  // ---------- 阿里云 CDT 主机(V17) ----------
  cloudAccounts: () =>
    request<{ items: CloudAccount[]; last_run: string }>('/api/cloud-accounts'),
  createCloudAccount: (body: Record<string, unknown>) =>
    request<CloudAccount>('/api/cloud-accounts', { method: 'POST', body }),
  updateCloudAccount: (id: number, body: Record<string, unknown>) =>
    request<CloudAccount>(`/api/cloud-accounts/${id}`, { method: 'PUT', body }),
  deleteCloudAccount: (id: number) =>
    request<void>(`/api/cloud-accounts/${id}`, { method: 'DELETE' }),
  /** 用一对凭据当场查一次 CDT 用量。传 account_id 用库里的 Secret,或直接传一对新凭据。 */
  testCloudAccount: (body: { account_id?: number; access_key_id?: string; access_key_secret?: string }) =>
    request<CloudTestResult>('/api/cloud-accounts/test', { method: 'POST', body }),
  refreshCloudAccount: (id: number) =>
    request<CloudAccount>(`/api/cloud-accounts/${id}/refresh`, { method: 'POST' }),
  cloudInstances: (id: number, region: string) =>
    request<{ items: CloudInstance[] }>(
      `/api/cloud-accounts/${id}/instances?region=${encodeURIComponent(region)}`,
    ),
  cloudSamples: (id: number, cls: CloudTrafficClass, days = 31) =>
    request<{ items: CloudSample[] }>(`/api/cloud-accounts/${id}/samples?class=${cls}&days=${days}`),
  nodeCloud: (id: number) => request<CloudNodeView>(`/api/nodes/${id}/cloud`),
  saveNodeCloud: (id: number, body: Record<string, unknown>) =>
    request<CloudNodeView>(`/api/nodes/${id}/cloud`, { method: 'PUT', body }),
  deleteNodeCloud: (id: number) => request<void>(`/api/nodes/${id}/cloud`, { method: 'DELETE' }),
  refreshNodeCloud: (id: number) =>
    request<CloudNodeView>(`/api/nodes/${id}/cloud/refresh`, { method: 'POST' }),
  startNodeCloud: (id: number) =>
    request<CloudNodeView>(`/api/nodes/${id}/cloud/start`, { method: 'POST' }),
  stopNodeCloud: (id: number) =>
    request<CloudNodeView>(`/api/nodes/${id}/cloud/stop`, { method: 'POST' }),
  nodeCloudEvents: (id: number, limit = 30) =>
    request<{ items: CloudPowerEvent[] }>(`/api/nodes/${id}/cloud/events?limit=${limit}`),

  notifySettings: () => request<NotifySettings>('/api/settings/notify'),
  updateNotifySettings: (body: Record<string, unknown>) =>
    request<NotifySettings>('/api/settings/notify', { method: 'PUT', body }),
  testNotify: () =>
    request<{ results: NotifyResult[] }>('/api/settings/notify/test', { method: 'POST' }),
  nodeHealth: () =>
    request<{ items: NodeHealth[]; enabled: boolean }>('/api/nodes/health'),
  runNodeHealth: () =>
    request<{ items: NodeHealth[] }>('/api/nodes/health/run', { method: 'POST' }),
  setConfigInRAM: (id: number, enabled: boolean) =>
    request<{ result: ConfigRAMResult; error?: string }>(`/api/nodes/${id}/config-in-ram`, {
      method: 'POST',
      body: { enabled },
    }),

  parseProxyURI: (uri: string) =>
    request<{
      protocol: ExternalProtocol
      protocol_label: string
      transport: string
      tls: boolean
      dialable_by_node: boolean
      dialable_reason: string
      relayable: boolean
      display_name: string
      server: string
      port: number
      method: string
      plugin: string
      plugin_opts: string
      has_password: boolean
    }>('/api/external-proxies/parse', { method: 'POST', body: { uri } }),

  // 代理源
  proxySources: () =>
    request<{
      items: ProxySource[]
      sync_failure_alert_threshold: number
      missing_rounds_before_unlist: number
    }>('/api/proxy-sources'),
  createProxySource: (body: Record<string, unknown>) =>
    request<ProxySource>('/api/proxy-sources', { method: 'POST', body }),
  updateProxySource: (id: number, body: Record<string, unknown>) =>
    request<ProxySource>(`/api/proxy-sources/${id}`, { method: 'PUT', body }),
  /** 条目去向必须显式给,没有默认值 */
  deleteProxySource: (id: number, proxies: 'delete' | 'detach') =>
    request<{ affected: number; mode: string }>(`/api/proxy-sources/${id}`, {
      method: 'DELETE',
      query: { proxies },
    }),
  proxySourceURL: (id: number) => request<{ url: string }>(`/api/proxy-sources/${id}/url`),
  previewProxySource: (url: string) =>
    request<ProxyPreviewResult>('/api/proxy-sources/preview', { method: 'POST', body: { url } }),
  previewExistingProxySource: (id: number) =>
    request<ProxyPreviewResult>(`/api/proxy-sources/${id}/preview`, { method: 'POST', body: {} }),
  importProxySource: (body: Record<string, unknown>) =>
    request<{ source: ProxySource; result: ProxySyncResult; error: string }>(
      '/api/proxy-sources/import',
      { method: 'POST', body },
    ),
  syncProxySource: (id: number) =>
    request<ProxySyncResult>(`/api/proxy-sources/${id}/sync`, { method: 'POST' }),

  // 配置文件订阅
  subscriptionProfiles: () =>
    request<{ items: SubscriptionProfile[] }>('/api/subscription-profiles'),
  subscriptionProfile: (id: number) =>
    request<SubscriptionProfile>(`/api/subscription-profiles/${id}`),
  createSubscriptionProfile: (body: Record<string, unknown>) =>
    request<SubscriptionProfile>('/api/subscription-profiles', { method: 'POST', body }),
  updateSubscriptionProfile: (id: number, body: Record<string, unknown>) =>
    request<SubscriptionProfile>(`/api/subscription-profiles/${id}`, { method: 'PUT', body }),
  deleteSubscriptionProfile: (id: number) =>
    request<{ message: string }>(`/api/subscription-profiles/${id}`, { method: 'DELETE' }),
  setSubscriptionProfileEnabled: (id: number, enabled: boolean) =>
    request<SubscriptionProfile>(`/api/subscription-profiles/${id}/enabled`, {
      method: 'POST',
      body: { enabled },
    }),
  /** 占位符说明由后端给,前端不另写一份 —— 两处会分叉 */
  profilePlaceholders: () => request<ProfilePlaceholderInfo>('/api/subscription-profiles/placeholders'),
  previewSubscriptionProfile: (body: Record<string, unknown>) =>
    request<ProfilePreview>('/api/subscription-profiles/preview', { method: 'POST', body }),

  // 供应商到期与续费(V20)。三类对象共用一组接口,种类走路径。
  expiry: (kind: ExpiryKind, id: number) =>
    request<ExpiryDetail>(`/api/expiry/${kind}/${id}`),
  saveExpiry: (kind: ExpiryKind, id: number, body: ExpiryProfilePayload) =>
    request<ExpiryView>(`/api/expiry/${kind}/${id}`, { method: 'PUT', body }),
  /** 清除对象自己的档案:外部代理回到「跟随来源」,其他回到「未设置」 */
  clearExpiry: (kind: ExpiryKind, id: number) =>
    request<ExpiryView>(`/api/expiry/${kind}/${id}`, { method: 'DELETE' }),
  renewExpiry: (kind: ExpiryKind, id: number, body: ExpiryRenewPayload) =>
    request<ExpiryRenewResult>(`/api/expiry/${kind}/${id}/renew`, { method: 'POST', body }),
  expiryRenewals: (kind: ExpiryKind, id: number) =>
    request<{ items: ExpiryRenewal[] }>(`/api/expiry/${kind}/${id}/renewals`),

  // 资产卡片:一次返回全部卡片的计费估值、标签、探测汇总与成本汇总。只读库,不连节点。
  dashboardCards: () => request<DashboardCards>('/api/dashboard/cards'),
  saveAssetLabels: (kind: ExpiryKind, id: number, body: { region: string; tags: string[] }) =>
    request<AssetLabels>(`/api/asset-labels/${kind}/${id}`, { method: 'PUT', body }),
  // TCP 探测目标:只能从机器已登记的地址 × 端口里选(candidates)。
  nodeProbe: (nodeID: number) => request<NodeProbe>(`/api/nodes/${nodeID}/probe`),
  createProbeTarget: (nodeID: number, body: { ref: string; family: ProbeFamily; label?: string }) =>
    request<ProbeTarget>(`/api/nodes/${nodeID}/probe-targets`, { method: 'POST', body }),
  updateProbeTarget: (id: number, body: { enabled?: boolean; label?: string }) =>
    request<ProbeTarget>(`/api/probe-targets/${id}`, { method: 'PUT', body }),
  deleteProbeTarget: (id: number) =>
    request<{ message: string }>(`/api/probe-targets/${id}`, { method: 'DELETE' }),
  probeHistory: (id: number, range: '1h' | '24h' | '7d') =>
    request<ProbeHistory>(`/api/probe-targets/${id}/history`, { query: { range } }),
  publicProbeTargets: () => request<PublicProbeLibrary>('/api/probe/public-targets'),
  createPublicProbeTarget: (body: PublicProbeInput) =>
    request<PublicProbeTarget>('/api/probe/public-targets', { method: 'POST', body }),
  updatePublicProbeTarget: (id: number, body: PublicProbeInput) =>
    request<PublicProbeTarget>(`/api/probe/public-targets/${id}`, { method: 'PUT', body }),
  deletePublicProbeTarget: (id: number) =>
    request<{ message: string }>(`/api/probe/public-targets/${id}`, { method: 'DELETE' }),
  publicProbeTargetNodes: (id: number) =>
    request<{ nodes: PublicProbeNode[] }>(`/api/probe/public-targets/${id}/nodes`),
  setPublicProbeScope: (id: number, body: { scope: PublicProbeScope; node_ids?: number[] }) =>
    request<PublicProbeTarget>(`/api/probe/public-targets/${id}/scope`, { method: 'PUT', body }),
  setProbeBinding: (nodeID: number, publicID: number, mode: ProbeBindingMode) =>
    request<{ message: string }>(`/api/nodes/${nodeID}/probe-public/${publicID}`, { method: 'PUT', body: { mode } }),
  setProbeBindings: (body: { node_ids: number[]; public_ids: number[]; mode: ProbeBindingMode }) =>
    request<{ message: string }>('/api/probe/bindings', { method: 'POST', body }),
  updateProbeSource: (kind: ProbeOrigin, body: ProbeSourceInput) =>
    request<ProbeSource>(`/api/probe/sources/${kind}`, { method: 'PUT', body }),
  updateProbeLimits: (values: Record<string, number>) =>
    request<{ limits: ProbeLimits; ranges: ProbeLimitRange[] }>('/api/probe/limits', { method: 'PUT', body: { values } }),
  publicProbeBatch: (body: { ids: number[]; action: 'pause' | 'resume' | 'delete' | 'scope'; scope?: PublicProbeScope; node_ids?: number[] }) =>
    request<{ affected: number; series: number }>('/api/probe/public-targets/batch', { method: 'POST', body }),
  importPublicProbes: (text: string, commit: boolean) =>
    request<ProbeImportResult>('/api/probe/public-targets/import', { method: 'POST', body: { text, commit } }),
  verifyPublicProbes: (ids: number[]) =>
    request<{ results: ProbeVerifyResult[] }>('/api/probe/public-targets/verify', { method: 'POST', body: { ids } }),
  zstaticPresets: (body: { provinces: string[]; isps: string[]; families: ProbeFamily[]; tcp_mode: ProbeTCPMode }) =>
    request<ProbePreview>('/api/probe/sources/zstatic/presets', { method: 'POST', body }),
  fetchProbeSource: (kind: 'zstatic' | 'tcpquality', body: { tcp_mode: ProbeTCPMode; host_mode?: 'DOMAIN' | 'IP' }) =>
    request<ProbePreview>(`/api/probe/sources/${kind}/fetch`, { method: 'POST', body }),
  applyProbeSource: (body: ProbeApplyRequest) =>
    request<ProbeApplyResult>('/api/probe/sources/apply', { method: 'POST', body }),
  nodeProbeSettings: (nodeID: number) => request<NodeProbeSettings>(`/api/nodes/${nodeID}/probe-settings`),
  setNodeProbeSettings: (nodeID: number, body: { paused: boolean; interval_sec: number }) =>
    request<NodeProbeSettings>(`/api/nodes/${nodeID}/probe-settings`, { method: 'PUT', body }),
  probeTrial: (nodeID: number, publicIDs: number[]) =>
    request<{ results: ProbeTrialResult[] }>(`/api/nodes/${nodeID}/probe-trial`, { method: 'POST', body: { public_ids: publicIDs } }),
  // 卡片展示偏好:只写一张表,不触发任何探测、不调 Boce。
  // 别名不传 = 保持原样,空对象 = 全部清掉。
  setCardPrefs: (nodeID: number, body: Pick<CardPrefs, 'ingress_mode' | 'ingress_ids' | 'outbound_mode' | 'outbound_ids'>
    & Partial<Pick<CardPrefs, 'ingress_aliases' | 'outbound_aliases'>>) =>
    request<CardPrefs>(`/api/nodes/${nodeID}/probe-card`, { method: 'PUT', body }),
  // 自建节点互测入站:按「来源 × 测法」各建一条;立即检测的结果照常进历史。启停与删除走 updateProbeTarget / deleteProbeTarget。
  createProbePeers: (nodeID: number, body: PeerInput) =>
    request<{ created: ProbeTarget[]; duplicates: number }>(`/api/nodes/${nodeID}/probe-peers`, { method: 'POST', body }),
  runProbePeers: (nodeID: number, ids: number[]) =>
    request<{ results: PeerRunResult[] }>(`/api/nodes/${nodeID}/probe-peers/run`, { method: 'POST', body: { ids } }),

  // Boce 外部入站拨测。页面访问只读库;会调 Boce 的只有校验 / 余额 / 同步(免费)与手动检测(付费,经预算)。
  boceOverview: () => request<BoceOverview>('/api/boce'),
  updateBoceSettings: (body: BoceSettingsInput) => request<BoceSettings>('/api/boce/settings', { method: 'PUT', body }),
  boceVerify: () => request<{ balance: string; point: number }>('/api/boce/verify', { method: 'POST' }),
  boceBalance: () => request<{ balance: string; point: number }>('/api/boce/balance', { method: 'POST' }),
  boceSyncVantages: () =>
    request<{ results: { area: string; total: number; added: number; missing: number; error: string }[] }>(
      '/api/boce/vantages/sync',
      { method: 'POST' },
    ),
  boceVantages: () => request<{ vantages: BoceVantage[]; state: BoceState }>('/api/boce/vantages'),
  boceResume: () => request<{ message: string }>('/api/boce/resume', { method: 'POST' }),
  boceEstimate: (body: BoceEstimateInput) => request<BoceEstimate>('/api/boce/estimate', { method: 'POST', body }),
  boceTasks: () => request<{ tasks: BoceTaskView[] }>('/api/boce/tasks'),
  boceBatch: (body: { template: BoceTaskInput; nodes: { node_id: number; ref: string }[] }) =>
    request<{ results: { node_id: number; task_id: number; error: string }[] }>('/api/boce/tasks/batch', {
      method: 'POST',
      body,
    }),
  nodeBoce: (nodeID: number) => request<NodeBoce>(`/api/nodes/${nodeID}/boce`),
  createBoceTask: (nodeID: number, body: BoceTaskInput) =>
    request<BoceTask>(`/api/nodes/${nodeID}/boce/tasks`, { method: 'POST', body }),
  updateBoceTask: (id: number, body: BoceTaskInput) => request<BoceTask>(`/api/boce/tasks/${id}`, { method: 'PUT', body }),
  setBoceTaskEnabled: (id: number, enabled: boolean) =>
    request<BoceTask>(`/api/boce/tasks/${id}/enabled`, { method: 'PUT', body: { enabled } }),
  archiveBoceTask: (id: number, archived: boolean) =>
    request<BoceTask>(`/api/boce/tasks/${id}/archive`, { method: 'POST', body: { archived } }),
  deleteBoceTask: (id: number) => request<{ message: string }>(`/api/boce/tasks/${id}`, { method: 'DELETE' }),
  runBoceTask: (id: number, requestID: string) =>
    request<BoceRun>(`/api/boce/tasks/${id}/run`, { method: 'POST', body: { request_id: requestID } }),
  boceRuns: (id: number, limit = 30) => request<{ runs: BoceRun[] }>(`/api/boce/tasks/${id}/runs`, { query: { limit } }),
  setNodeBocePaused: (nodeID: number, paused: boolean) =>
    request<NodeProbeSettings>(`/api/nodes/${nodeID}/boce/paused`, { method: 'PUT', body: { paused } }),

  // 面板设置
  settings: () => request<PanelSettings>('/api/settings'),
  updateSettings: (body: Record<string, unknown>) =>
    request<PanelSettings>('/api/settings', { method: 'PUT', body }),
  nodesTodayTraffic: () =>
    request<{ items: { node_id: number; bytes: number }[] }>('/api/traffic/nodes-today'),
  // 批量取,节点列表才不会每行发一个请求。
  nodesCycleTraffic: () =>
    request<{ items: NodeCycleUsage[] }>('/api/traffic/nodes-cycle'),
  // 全站每日流量。只返回确实有记录的日子,缺的日子由前端画成缺口而不是 0。
  siteDailyTraffic: (days = 30) =>
    request<{ daily: DailyPoint[] }>('/api/traffic/daily', { query: { days } }),
}
