import type { Node } from '@/api/client'
import { lbDangerConfirm, type LbConfirmOption } from '@/components/lb'

/**
 * 机器级操作的确认文案。
 *
 * 只抽文案与档次,不抽执行与进度 —— 后者各页面差别很大(节点详情有完整的
 * 步骤时间线,入口管理页只要一个结果摘要),硬凑到一起只会让两边都别扭。
 *
 * 但**影响清单必须单一来源**。它是管理员判断"这一下要不要挑时机"的全部依据,
 * 两处各写一份的话,某天在一处补了一条(比如「会踢掉全部入口的连接」),
 * 另一处仍然是旧的 —— 而他会按上次看到的那一版做决定。
 */

export function nodeLabel(n: Pick<Node, 'display_name' | 'name'>): string {
  return n.display_name || n.name
}

/**
 * 部署一台机器。
 *
 * 可逆(健康检查不通过会自动回滚),所以是 lbDangerConfirm 而不是打字确认 ——
 * 摩擦按"能不能撤回"分档,给可逆操作也加打字确认的话,管理员很快会变成
 * 无脑复制粘贴,真正不可逆的那四个反而失去警示作用。
 */
export function confirmDeployNode(n: Node, onOk: (opts: DeployChoice) => void) {
  lbDangerConfirm({
    title: `部署 rev ${n.config_revision + 1} 到 ${nodeLabel(n)}?`,
    okText: '部署',
    okType: 'primary',
    impacts: [
      '先做前置检查,并自动补齐缺失的条件:没装 sing-box 就装面板分发的那一版,sshd 没开 TCP 转发就打开它(只加一行,reload 不 restart)',
      '配置已一致且服务在跑时【不重启】,直接返回「配置已一致」;要强制重新应用用「强制重新下发」',
      '有变更时会重启 sing-box,断开这台机器上【全部入口】的在线连接',
      '重启前会强制同步一次流量,未落库的计数不会丢;服务本来没在跑则跳过同步',
      `健康检查不通过时自动回滚到 rev ${n.config_revision}`,
    ],
    footer: '部署是可逆的(有自动回滚),所以不要求输入节点名称。',
    option: DIAL_OPTION,
    // 故意不 return onOk 的 Promise。Modal.confirm 只要拿到 Promise
    // 就会把自己留在屏幕上转圈等它 resolve —— 而部署要 15~25 秒,
    // 这期间进度弹窗已经打开,两个 Modal 同层叠在一起,后开的反而被压住。
    onOk: (dial) => {
      onOk({ skipDial: !dial })
    },
  })
}

/** 下发确认框交回来的选择。 */
export interface DeployChoice {
  /** 管理员关掉了「下发后做真实拨测」 */
  skipDial: boolean
}

/**
 * 「下发后做真实拨测」开关,sing-box 与 Mieru 两种下发共用一份文案。
 *
 * 默认开着:拨测是唯一能发现「服务在跑、端口在听,但谁都连不上」的一步(flow 写错、凭据不对)。
 * 关掉的用途是入口机或它的落地挂了来源白名单 —— 拨测那条路走不通,而管理员打算自己用客户端去测。
 */
export const DIAL_OPTION: LbConfirmOption = {
  label: '下发后做真实拨测',
  defaultValue: true,
  onHint: '在节点上用真实用户的凭据连一次入口并访问一次拨测地址,不通就自动回滚。',
  offHint:
    '只查服务状态与端口监听,**不验证这个入口能不能连上**;拨测那一步在部署记录里记为「已跳过」。' +
    '下发完请用客户端自己测一次。只对这一次下发有效 —— 改用户、改等级触发的自动下发仍然会拨测。',
}

/** 跳过拨测的下发成功之后,结果里补的那一句。「下发成功」在这里不等于入口能用。 */
export const DIAL_SKIPPED_NOTE =
  '这次没有做拨测:服务在跑、端口在听,但面板没有验证入口能不能连上。请用客户端自己测一次。'

/**
 * 直接重启一台机器的 sing-box。
 *
 * 与部署不同:它**不先同步流量**,所以常规的用户/入口变更一律走部署。
 * 这句话要写在影响里 —— 两个按钮挨在一起,而"重启"听起来比"部署"轻。
 */
export function confirmRestartNode(n: Node, onOk: () => void) {
  lbDangerConfirm({
    title: `重启 ${nodeLabel(n)} 的服务?`,
    okText: '重启',
    impacts: [
      '断开这台机器上【全部入口】的在线连接',
      `配置不变,重启后仍是 rev ${n.config_revision}`,
      '这是运维用的直接重启,不会先同步流量 —— 常规的用户变更请用「部署」',
    ],
    onOk: () => {
      onOk()
    },
  })
}
