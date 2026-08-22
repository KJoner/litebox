// API 客户端。会话通过 HttpOnly Cookie 传递,因此所有请求都要带 credentials。

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
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

  const response = await fetch(url, init)
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
    throw new ApiError(response.status, message)
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
export type NodeProtocol = 'VLESS_REALITY' | 'SHADOWSOCKS'

/** Shadowsocks 2022 的加密方法。只有这三种,不收传统 AEAD。 */
export type NodeSSMethod =
  | '2022-blake3-aes-128-gcm'
  | '2022-blake3-aes-256-gcm'
  | '2022-blake3-chacha20-poly1305'

export const PROTOCOL_LABEL: Record<NodeProtocol, string> = {
  VLESS_REALITY: 'VLESS + REALITY',
  SHADOWSOCKS: 'Shadowsocks 2022',
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
  /** sing-box 真正 bind 的端口 */
  listen_port: number
  /** 客户端连接的公网端口。0 表示跟随 listen_port。 */
  public_port: number
  /** IPv6 条目用的公网端口。0 表示跟随 public_port。 */
  ipv6_public_port: number
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
  listen_port: number
  public_port: number
  ipv6_public_port?: number
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
  /** IPv4 地址,同时是 SSH 管理地址与 IPv4 订阅地址 */
  host: string
  /** 可选的公网 IPv6,只影响订阅:填了就多下发一条「展示名称-IPV6」 */
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
  /** 配置与备份放在内存文件系统里,磁盘上不留。机器重启后靠巡检重新下发 */
  config_in_ram: boolean
  arch: string
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
  display_name: string
  /** nginx 在中转主机上实际监听的端口 */
  listen_port: number
  /**
   * 客户端连接的公网端口。0 表示跟随 listen_port。
   *
   * 存 0 而不是把当时的 listen_port 写进去 —— 那样以后改监听端口,
   * 订阅条目会停在旧端口上,而管理员当初看到的是一个空输入框。
   */
  public_port: number
  target_kind: 'INBOUND' | 'EXTERNAL'
  /** 落地是自建节点【某一个入口】的 id,不是机器的 id */
  target_inbound_id: number
  target_external_id: number
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
  detail: string
}

export interface CreateNodeResult {
  node: Node
  bootstrap?: BootstrapResult
  bootstrap_error?: string
}

export interface PanelSettings {
  subscription_base_url: string
  config_base_url: string
  panel_public_key: string
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

export interface ConfigDiff {
  node_id: number
  revision: number
  desired_sha256: string
  remote_sha256: string
  in_sync: boolean
  desired_users: string[] | null
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

export interface UserTraffic {
  user_code: string
  used_uplink: number
  used_downlink: number
  used_total: number
  quota_bytes: number
  by_node: UserNodeTraffic[]
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
}

export interface TrafficStatus {
  last_run?: string
  failing_nodes: { node_id: number; error: string }[]
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
  category: 'user' | 'node'
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
  /** 订阅里会多出一条「展示名称-IPV6」;地址本身不下发给用户 */
  supports_ipv6: boolean
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
  /** 能不能用 nginx 透传到它。QUIC 系是纯 UDP,而 stream 这边只搬 TCP 字节 */
  relayable: boolean
  created_at: string
  updated_at: string
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
export interface NodeHealth {
  node_id: number
  node_name: string
  checked_at: string
  singbox: ServiceState
  singbox_detail: string
  nginx: ServiceState
  nginx_detail: string
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
  SHADOWROCKET: '只有规则,节点要另外用「通用订阅」地址添加',
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
  regenerateSubToken: (id: number) =>
    request<ProxyUser>(`/api/users/${id}/regenerate-sub-token`, { method: 'POST' }),
  userTraffic: (id: number, days = 30) =>
    request<UserTraffic>(`/api/users/${id}/traffic`, { query: { days } }),

  // 节点
  nodes: () => request<{ items: Node[] }>('/api/nodes'),
  node: (id: number) => request<Node>(`/api/nodes/${id}`),
  createNode: (body: Record<string, unknown>) =>
    request<CreateNodeResult>('/api/nodes', { method: 'POST', body }),
  bootstrapNode: (id: number, rootPassword: string) =>
    request<BootstrapResult>(`/api/nodes/${id}/bootstrap`, {
      method: 'POST',
      body: { root_password: rootPassword },
    }),
  uninstallNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}/uninstall`, { method: 'POST' }),
  panelKey: () => request<{ public_key: string }>('/api/panel-key'),
  updateNode: (id: number, body: Record<string, unknown>) =>
    request<{ node: Node; effect: NodeUpdateEffect }>(`/api/nodes/${id}`, { method: 'PUT', body }),
  deleteNode: (id: number) =>
    request<{ message: string }>(`/api/nodes/${id}`, { method: 'DELETE' }),
  setNodeEnabled: (id: number, enabled: boolean) =>
    request<{ message: string }>(`/api/nodes/${id}/enabled`, { method: 'POST', body: { enabled } }),
  testNodeSSH: (id: number) =>
    request<{ ok: boolean; uname: string; resolved_ip: string }>(
      `/api/nodes/${id}/test-ssh`,
      { method: 'POST' },
    ),
  probeNode: (id: number) => request<ProbeResult>(`/api/nodes/${id}/probe`, { method: 'POST' }),
  installNode: (id: number) =>
    request<{
      binary_path: string
      binary_sha256: string
      service_name: string
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
    }>(
      `/api/nodes/${id}/install`,
      { method: 'POST' },
    ),
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
  deployNode: (id: number) =>
    request<DeployResult>(`/api/nodes/${id}/deploy`, { method: 'POST' }),
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
  /** 实测握手目标并在通过后写入这个入口;不通过时拒绝保存。 */
  applyInboundDest: (id: number, dest: string) =>
    request<{ result: DestCheckResult; error?: string }>(`/api/inbounds/${id}/dest-check`, {
      method: 'POST',
      body: { dest, port: 443 },
    }),

  // 中转:转发规则的增删改只 reload nginx,不打断任何在途连接,
  // 因此这一组接口的操作摩擦比「部署」低一档。
  relays: () => request<{ items: NodeRelay[] }>('/api/relays'),
  nodeRelays: (nodeID: number) =>
    request<{ items: NodeRelay[] }>(`/api/nodes/${nodeID}/relays`),
  createRelay: (nodeID: number, body: Record<string, unknown>) =>
    request<{ relay: NodeRelay }>(`/api/nodes/${nodeID}/relays`, { method: 'POST', body }),
  updateRelay: (id: number, body: Record<string, unknown>) =>
    request<{ relay: NodeRelay }>(`/api/relays/${id}`, { method: 'PUT', body }),
  deleteRelay: (id: number) =>
    request<{ deleted: boolean }>(`/api/relays/${id}`, { method: 'DELETE' }),
  deployRelays: (nodeID: number) =>
    request<{ result: DeployResult; error?: string }>(`/api/nodes/${nodeID}/relays/deploy`, {
      method: 'POST',
    }),
  /** 只读探测,不会在节点上安装任何东西 */
  nodeNginx: (nodeID: number) => request<NginxFacts>(`/api/nodes/${nodeID}/nginx`),

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
  trafficStatus: () => request<TrafficStatus>('/api/traffic/status'),

  // 节点资源监控
  nodeMetricsLatest: () => request<{ items: NodeMetrics[] }>('/api/metrics/nodes-latest'),
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
