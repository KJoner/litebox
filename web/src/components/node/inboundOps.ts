import { message } from 'ant-design-vue'
import { ApiError, PROTOCOL_SHORT, api, type Node, type NodeInbound } from '@/api/client'
import { lbDangerConfirm, type LbStatusMeta } from '@/components/lb'
import { color } from '@/theme/tokens'

/**
 * 入口(sing-box 入站)的共用显示与操作。
 *
 * 抽出来的理由只有一个:同一个入口现在有**两个**可以操作它的地方 ——
 * 节点详情的「入口」Tab,以及跨节点的「入口管理」页。两处各写一遍的话,
 * 迟早出现一处的确认文案漏了一条影响、或者删除的档次不一样,
 * 而管理员会按他上次看到的那一版去判断这一下要不要挑时机。
 *
 * 这里只放**与所在页面无关**的部分:文案、状态编码、危险操作的档次。
 * 表单字段与弹窗留在各自的组件里 —— 那部分本来就该跟着容器走。
 */

/**
 * 这个入口在订阅里到底会不会多出一条 IPv6 条目。
 *
 * 两个条件缺一不可:机器填了 IPv6 地址,而且这个入口开着 IPv6 条目。
 * 只看前者的话,管理员关掉某个入口的 IPv6 之后,列表仍然写着「IPv4 + IPv6」,
 * 而用户拉到的订阅里根本没有那一条。
 */
export function inboundHasIPv6Entry(i: NodeInbound, node: Node): boolean {
  return !!node.ipv6_address && i.ipv6_enabled
}

/**
 * 一行在订阅里占几个地址族。
 *
 * 两档都用中性色:双栈不是「更好」,只是「多一条地址」—— 用绿色会让单栈
 * 那些行看起来像出了问题,而机器有没有 IPv6 是商家给的,不是配错了。
 * 形状仍然区分,打印与色觉障碍下也读得出来。
 */
export function addressFamilyMeta(dual: boolean): LbStatusMeta {
  return dual
    ? {
        text: 'IPv4 + IPv6',
        shape: 'dot',
        fg: color.neutral,
        bg: color.neutralBg,
        bd: color.neutralBorder,
      }
    : {
        text: 'IPv4',
        shape: 'minus',
        fg: color.neutral,
        bg: color.neutralBg,
        bd: color.neutralBorder,
      }
}

/**
 * 协议列的状态编码。
 *
 * 显示的是 deployed_protocol(节点上真正在跑的),不是 protocol(期望值)。
 * 两者不同时明写「A → B 待部署」而不是直接显示 B:改协议到部署成功之间
 * 有一个窗口,可能是二十秒,也可能是部署失败自动回滚之后的**永远**,
 * 而这段时间里订阅下发的仍然是 A。
 */
export function inboundProtocolMeta(i: NodeInbound): LbStatusMeta {
  const p = i.deployed_protocol
  if (!p) {
    return {
      text: '未部署',
      shape: 'ring',
      fg: color.neutral,
      bg: color.neutralBg,
      bd: color.neutralBorder,
    }
  }
  const pending = p !== i.protocol
  return {
    text: pending ? `${PROTOCOL_SHORT[p]} → ${PROTOCOL_SHORT[i.protocol]} 待部署` : PROTOCOL_SHORT[p],
    shape: pending ? 'triangle' : 'check',
    fg: pending ? color.warning : color.success,
    bg: pending ? color.warningBg : color.successBg,
    bd: pending ? color.warningBorder : color.successBorder,
  }
}

export const inboundEnabledMeta: Record<'on' | 'off', LbStatusMeta> = {
  on: { text: '启用', shape: 'check', fg: color.success, bg: color.successBg, bd: color.successBorder },
  off: { text: '已停用', shape: 'minus', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder },
}

/** 一行的端口写法。两个端口相同时只写一个号码 —— 写两遍会让人以为配了转发。 */
export function portText(listen: number, pub: number): string {
  const p = pub || listen
  return p === listen ? `端口 ${listen}` : `公网 ${p} → 主机 ${listen}`
}

export function nodeLabelOf(n: Pick<Node, 'display_name' | 'name'>): string {
  return n.display_name || n.name
}

/**
 * 删除一个 sing-box 入口。
 *
 * 不可逆的只有四个操作(删用户、删节点、卸载服务、重置主机密钥),
 * 这一个可以重建,所以用 lbDangerConfirm 而不是打字确认 ——
 * 但它的影响面比删一条 nginx 转发大得多,必须逐条列出来。
 */
export function confirmRemoveInbound(
  i: NodeInbound,
  nodeLabel: string,
  run: (fn: () => Promise<void>) => void,
  onDone: () => void,
) {
  lbDangerConfirm({
    title: `删除入口「${i.display_name}」?`,
    impacts: [
      `${nodeLabel} 上监听 ${i.listen_port} 的 sing-box 入站会被撤掉`,
      '这一条会从所有用户的订阅里消失;只用这个入口的人会连不上',
      '下次部署这台机器时才真正生效 —— 在那之前它照常在跑',
      '部署会重启 sing-box,这台机器上【全部入口】的在线连接都会断开',
      '入口级的流量计数器不会复用,重建之后历史曲线接不上',
    ],
    okText: '删除',
    onOk: () => {
      run(async () => {
        try {
          await api.deleteInbound(i.id)
          onDone()
          message.success('已删除。下次部署这台机器时才真正从节点上撤掉')
        } catch (e) {
          message.error(e instanceof ApiError ? e.message : '删除失败')
        }
      })
    },
  })
}
