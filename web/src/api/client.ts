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

export interface Node {
  id: number
  /** 内部名称,只在管理后台出现 */
  name: string
  /** 对用户与订阅展示的名称 */
  display_name: string
  access_tier_id: number
  access_tier_code: string
  access_tier_name: string
  access_tier_level: number
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
   * IPv6 条目在订阅里用的公网端口。0 表示跟随 proxy_port。
   *
   * 存 0 而不是把当时的 proxy_port 写进去 —— 那样以后改 IPv4 公网端口,
   * IPv6 条目会停在旧端口上,而管理员当初看到的是一个空输入框。
   */
  ipv6_proxy_port: number
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
  /** 客户端连接的公网端口,写进订阅 */
  proxy_port: number
  /** sing-box 在节点上监听的端口,NAT / nginx 转发时与 proxy_port 不同 */
  listen_port: number
  api_port: number
  arch: string
  singbox_version: string
  singbox_build_tags: string
  reality_dest: string
  reality_dest_port: number
  reality_public_key: string
  reality_short_id: string
  handshake_max_record_size: number
  handshake_checked_at: string | null
  status: NodeStatus
  last_heartbeat_at: string | null
  config_revision: number
  deployed_config_sha256: string
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

export type NodeConfigState =
  | 'NEVER_DEPLOYED'
  | 'IN_SYNC'
  | 'PENDING'
  | 'DEPLOY_FAILED'
  | 'UNKNOWN'

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
  nodes: () => request<{ items: PortalNode[] }>('/api/portal/nodes'),
  traffic: (days = 30) => request<PortalTraffic>('/api/portal/traffic', { query: { days } }),
  subscription: () => request<PortalSubscription>('/api/portal/subscription'),
  regenerateSubscription: () =>
    request<PortalSubscription>('/api/portal/subscription/regenerate', { method: 'POST' }),
  adjustments: () => request<{ items: PublicAdjustment[] }>('/api/portal/adjustments'),
}

// ---------- 接口 ----------

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
    request<{ ok: boolean; uname: string }>(`/api/nodes/${id}/test-ssh`, { method: 'POST' }),
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
  checkNodeDest: (id: number, dest: string, apply = false) =>
    request<DestCheckResult>(`/api/nodes/${id}/dest-check`, {
      method: 'POST',
      body: { dest, port: 443, apply },
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
    request<{ node_id: number; cycle: NodeCycleUsage; daily: DailyPoint[] }>(
      `/api/nodes/${id}/traffic`,
      { query: { days } },
    ),
  destCandidates: () =>
    request<{ items: string[]; max_record_size: number }>('/api/dest-candidates'),

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
