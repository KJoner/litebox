import { adminOnly, type ApiMock } from './mock'

/**
 * 链路画布的虚构拓扑(v23.3),功能用例与截图基线共用。地址全部是文档地址段(RFC 5737 / 3849)
 * 与 example.com,不对应任何真实机器。
 */

export const EP = (id: number, address: string, port: number, family: 'V4' | 'V6' = 'V4', referenced = 0) => ({
  id,
  family,
  address,
  port,
  label: '',
  referenced,
})

function entry(over: Record<string, unknown>) {
  return {
    region: '',
    enabled: true,
    deployed: true,
    pending: '',
    tier_id: 1,
    access_mode: 'TIER',
    subscription_enabled: true,
    direct_users: 2,
    direct_access: true,
    endpoints: [],
    egress: 'DIRECT',
    referenced: 0,
    sort_order: 0,
    problems: [],
    ...over,
  }
}

export const SNAP = {
  revision: 'rev-0001',
  generated_at: '2026-11-14T00:00:00Z',
  hosts: [
    { id: 1, name: 'hk-1', display_name: '香港一号', region: 'HK', status: 'ONLINE', role: 'LANDING', sort_order: 1, config_state: 'IN_SYNC' },
    { id: 2, name: 'jp-1', display_name: '东京中转', region: 'JP', status: 'ONLINE', role: 'RELAY', sort_order: 2, config_state: 'NOT_APPLICABLE' },
    { id: 3, name: 'sg-1', display_name: '新加坡落地', region: 'SG', status: 'ONLINE', role: 'LANDING', sort_order: 3, config_state: 'IN_SYNC' },
  ],
  tiers: [
    { id: 1, code: 'normal', name: '普通组', level: 1 },
    { id: 2, code: 'vip', name: 'VIP', level: 2 },
  ],
  entries: [
    entry({
      key: 'SINGBOX:1', kind: 'SINGBOX', id: 1, node_id: 1, node_name: '香港一号', name: '香港入口A', protocol: 'VLESS_REALITY',
      region: 'HK', sort_order: 1,
      endpoints: [EP(11, '192.0.2.10', 443), EP(12, '198.51.100.20', 2443), EP(13, '2001:db8::20', 3443, 'V6')],
      next: { target: 'NGINX:5', endpoint_id: 51, addr_mode: 'ENDPOINT', dial: '203.0.113.50:30443' }, egress: 'NEXT',
    }),
    entry({
      key: 'SINGBOX:3', kind: 'SINGBOX', id: 3, node_id: 1, node_name: '香港一号', name: '香港入口C', protocol: 'SHADOWSOCKS',
      region: 'HK', sort_order: 2, tier_id: 2,
      endpoints: [EP(31, '192.0.2.10', 8443)],
      next: { target: 'NGINX:5', endpoint_id: 51, addr_mode: 'ENDPOINT', dial: '203.0.113.50:30443' }, egress: 'NEXT',
    }),
    entry({
      key: 'NGINX:5', kind: 'NGINX', id: 5, node_id: 2, node_name: '东京中转', name: '东京转发R', transparent: true,
      region: 'JP', referenced: 2, direct_access: false,
      endpoints: [EP(51, '203.0.113.50', 30443, 'V4', 2)],
      next: { target: 'SINGBOX:2', endpoint_id: 21, addr_mode: 'ENDPOINT', dial: '192.0.2.30:443' }, egress: 'NEXT',
    }),
    entry({
      key: 'SINGBOX:2', kind: 'SINGBOX', id: 2, node_id: 3, node_name: '新加坡落地', name: '新加坡落地B', protocol: 'VLESS_REALITY',
      region: 'SG', referenced: 2, endpoints: [EP(21, '192.0.2.30', 443, 'V4', 2)],
    }),
    entry({
      key: 'REALM:6', kind: 'REALM', id: 6, node_id: 2, node_name: '东京中转', name: '纯端口转发N', transparent: true, region: 'JP',
      endpoints: [{ id: 0, family: 'V4', address: '203.0.113.50', port: 31443, label: '默认地址(管理地址)', default: true, referenced: 0 }],
      next: { target: 'ADDRESS:6', dial: '198.51.100.77:8443' }, egress: 'NEXT', direct_access: false,
      note: '落地是指定地址:不进订阅,面板不知道它背后是什么',
    }),
    entry({
      key: 'MIERU:7', kind: 'MIERU', id: 7, node_id: 3, node_name: '新加坡落地', name: '新加坡Mieru', protocol: 'MIERU', region: 'SG',
      endpoints: [{ ...EP(71, '192.0.2.30', 31000), port_end: 31004 }],
    }),
    entry({
      key: 'EXTERNAL:9', kind: 'EXTERNAL', id: 9, name: '机场线路X', protocol: 'TROJAN', source: '示例机场', egress: 'EXTERNAL',
      endpoints: [{ id: 0, family: 'V4', address: 'proxy.example.com', port: 443, label: '外部代理地址', default: true, referenced: 0 }],
      note: '之后的路径由外部服务决定',
    }),
    entry({
      key: 'SINGBOX:4', kind: 'SINGBOX', id: 4, node_id: 1, node_name: '香港一号', name: '停用的旧入口D', protocol: 'VLESS_REALITY',
      region: 'HK', enabled: false, sort_order: 3, direct_access: false,
      endpoints: [EP(41, '192.0.2.10', 9443)],
      next: { target: 'SINGBOX:2', addr_mode: 'LEGACY', dial: '198.51.100.30:443' }, egress: 'NEXT',
      problems: ['待确认地址选择:升级前实际连的是 198.51.100.30:443,目标入口上没有唯一对应的地址条目', '已停用'],
    }),
  ],
  edges: [
    { id: 'e:SINGBOX:1', from: 'SINGBOX:1', to: 'NGINX:5', kind: 'PROXY', endpoint_id: 51, addr_mode: 'ENDPOINT', dial: '203.0.113.50:30443', state: 'OK' },
    { id: 'e:SINGBOX:3', from: 'SINGBOX:3', to: 'NGINX:5', kind: 'PROXY', endpoint_id: 51, addr_mode: 'ENDPOINT', dial: '203.0.113.50:30443', state: 'OK' },
    { id: 'e:NGINX:5', from: 'NGINX:5', to: 'SINGBOX:2', kind: 'FORWARD', endpoint_id: 21, addr_mode: 'ENDPOINT', dial: '192.0.2.30:443', state: 'OK' },
    { id: 'e:REALM:6', from: 'REALM:6', to: 'ADDRESS:6', kind: 'FORWARD', dial: '198.51.100.77:8443', state: 'UNKNOWN_BACKEND' },
    { id: 'e:SINGBOX:4', from: 'SINGBOX:4', to: 'SINGBOX:2', kind: 'PROXY', addr_mode: 'LEGACY', dial: '198.51.100.30:443', state: 'LEGACY' },
  ],
  aux: [{ key: 'ADDRESS:6', kind: 'ADDRESS', label: '198.51.100.77:8443', detail: '指定地址终点:后端未知' }],
  problems: [
    { kind: 'LEGACY_ADDR', entry: 'SINGBOX:4', edge: 'e:SINGBOX:4', message: '「香港一号 / 停用的旧入口D」的连接地址 198.51.100.30:443 待确认' },
  ],
  counts: { entries: 8, edges: 5, hosts: 3, problems: 1 },
}

export function mockCanvas(api: ApiMock) {
  adminOnly(api)
  api.on('GET', '/api/topology', { json: SNAP })
  api.on('GET', /^\/api\/chain-path\//, {
    json: { hops: [], egress: '从 新加坡落地 直连出网(按配置推导,不是实测的出口 IP)', dns: '', loop: false, problems: [], notes: [] },
  })
}

// —— 统一的下一跳选择器(节点详情里「出口」打开的那个)——

export const CANDS = {
  from: { ...SNAP.entries[0], key: 'SINGBOX:1', id: 1 },
  revision: 'rev-0001',
  groups: [
    {
      host_id: 3,
      host_name: '新加坡落地',
      region: 'SG',
      candidates: [
        {
          ...SNAP.entries[3],
          endpoints: [EP(21, '192.0.2.30', 443), EP(22, '198.51.100.20', 2443), EP(23, '2001:db8::30', 3443, 'V6')],
          disabled: false,
        },
        { ...SNAP.entries[5], disabled: true, reason: 'Mieru 入口不能当任何下一跳' },
      ],
    },
  ],
}
export const PREVIEW = {
  from: 'SINGBOX:1',
  dial: '198.51.100.20:2443',
  auth: '新加坡落地 / 新加坡落地B',
  transparent: [],
  path: { hops: [], egress: '从 新加坡落地 直连出网', dns: '', loop: false, problems: [], notes: [] },
  affected: [],
  deploys: [{ node_id: 3, node_name: 'sg-1', component: 'sing-box', why: '认证终点:写入这条链路的凭据' }],
  warnings: ['sing-box 重启会断开这台机器上【全部入口】的在线连接'],
  problems: [],
  notes: ['链路拨测只验 TCP;UDP 与真实出口 IP 都没有验证'],
  revision: 'rev-0002',
}

