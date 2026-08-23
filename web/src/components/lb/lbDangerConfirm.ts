import { h } from 'vue'
import { Modal } from 'ant-design-vue'

/**
 * 危险确认 —— 影响范围永远是逐条列表,不写成一段话。
 *
 * 摩擦分档按「能不能撤回」定,不按操作听起来吓不吓人:
 *   普通确认        可逆且后果局限   停用用户 / 恢复
 *   lbDangerConfirm 可逆但影响面大   重启服务 / 部署 / 重新生成订阅地址
 *   LbNameConfirm   不可逆           删除用户 / 删除节点 / 卸载服务 / 重置主机密钥
 *
 * 为什么不把输入名称合进这里:一旦合并,重启这类可逆操作也要打字,
 * 管理员很快会变成无脑复制粘贴,真正不可逆的四个反而失去警示作用。
 */
export interface LbDangerConfirmOptions {
  title: string
  /**
   * 逐条影响。每条一句话,主语明确。
   *
   * 支持 `**加粗**`:这几段话往往有一句是「真正会发生什么」,
   * 其余是铺垫,而管理员在确认框上停留的时间以秒计。
   * **不接 v-html** —— 这里的文案有一部分带节点名与用户填的展示名,
   * 拼进 innerHTML 就是一条从数据库到 DOM 的注入路径,而这个弹窗恰好
   * 是管理员最不会怀疑的地方。所以只认这一种标记,自己切段成文本节点。
   */
  impacts: string[]
  okText?: string
  cancelText?: string
  /** 非破坏性动作(如停用)用 primary,破坏性用 danger */
  okType?: 'primary' | 'danger'
  /** 影响范围之后的补充说明 */
  footer?: string
  onOk: () => void | Promise<unknown>
}

export function lbDangerConfirm(o: LbDangerConfirmOptions) {
  const danger = (o.okType ?? 'danger') === 'danger'

  Modal.confirm({
    title: o.title,
    width: 460,
    okText: o.okText ?? '确认',
    cancelText: o.cancelText ?? '取消',
    okType: danger ? 'danger' : 'primary',
    // 危险确认打开时焦点落在「取消」,不落在破坏性主按钮。
    autoFocusButton: 'cancel',
    icon: null,
    content: () =>
      h('div', { style: 'display:flex;flex-direction:column;gap:10px' }, [
        h(
          'div',
          {
            style: danger
              ? 'background:#FDECEA;border:1px solid #F3CFC9;border-radius:6px;padding:10px 11px'
              : 'background:#FCF3E3;border:1px solid #EFDCB4;border-radius:6px;padding:10px 11px',
          },
          [
            h(
              'div',
              {
                style: `font-size:11.5px;font-weight:600;margin-bottom:6px;color:${danger ? '#8E2117' : '#5C4405'}`,
              },
              '影响范围',
            ),
            h(
              'div',
              { style: `font-size:11.5px;line-height:1.75;color:${danger ? '#8E2117' : '#5C4405'}` },
              o.impacts.map((t) => h('div', ['· ', ...emphasize(t)])),
            ),
          ],
        ),
        o.footer
          ? h('div', { style: 'font-size:12px;line-height:1.7;color:#576070' }, emphasize(o.footer))
          : null,
      ]),
    onOk: o.onOk,
  })
}

/**
 * 把 `**...**` 切成文本节点与 <b>,其余原样。
 *
 * 落单的 `**` 原样留着,不当成标记的开头 —— 吞掉它会让一句话在弹窗里
 * 少几个字符,而作者对着源码怎么看都看不出哪里少了。
 */
function emphasize(text: string) {
  const parts = text.split('**')
  // 奇数个分隔符意味着有一个没配对,那时整句按原文出。
  if (parts.length % 2 === 0) return [text]
  return parts.map((p, i) => (i % 2 === 1 ? h('b', p) : p))
}
