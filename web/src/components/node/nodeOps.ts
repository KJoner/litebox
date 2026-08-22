import type { Node } from '@/api/client'
import { lbDangerConfirm } from '@/components/lb'

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
export function confirmDeployNode(n: Node, onOk: () => void) {
  lbDangerConfirm({
    title: `部署 rev ${n.config_revision + 1} 到 ${nodeLabel(n)}?`,
    okText: '部署',
    okType: 'primary',
    impacts: [
      '会重启 sing-box,断开这台机器上【全部入口】的在线连接',
      '部署前会强制同步一次流量,未落库的计数不会丢',
      `健康检查不通过时自动回滚到 rev ${n.config_revision}`,
    ],
    footer: '部署是可逆的(有自动回滚),所以不要求输入节点名称。',
    // 故意不 return onOk 的 Promise。Modal.confirm 只要拿到 Promise
    // 就会把自己留在屏幕上转圈等它 resolve —— 而部署要 15~25 秒,
    // 这期间进度弹窗已经打开,两个 Modal 同层叠在一起,后开的反而被压住。
    onOk: () => {
      onOk()
    },
  })
}

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
