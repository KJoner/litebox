import type { BootstrapResult } from '@/api/client'

/**
 * 把一次引导里「面板改了这台机器的 sshd 配置」的那几件事列成可以直接弹窗的句子。
 * 空数组表示这次引导除了装公钥什么都没动。
 *
 * 集中在一处:新建节点与「重新引导」两条路都要说同样的话,各写一遍迟早有一处
 * 漏掉口令登录那一条 —— 而那恰恰是面板放宽了别人机器安全策略的那一件,
 * 不说出来,管理员以后打开那个文件会以为被人动过手脚。
 */
export function bootstrapNotes(r: BootstrapResult | undefined): string[] {
  if (!r) return []
  const notes: string[] = []
  if (r.pubkey_auth_fixed) {
    notes.push(
      '这台机器原先关闭了 SSH 公钥认证(PubkeyAuthentication no),而面板此后只用公钥登录、不保存口令。' +
        '已在节点上写入一份配置把它打开并 reload 了 sshd,原有配置行一行没删。',
    )
  }
  if (r.password_auth_fixed) {
    notes.push(
      '这台机器原先不接受口令登录(PasswordAuthentication no 或 PermitRootLogin prohibit-password)。' +
        '按你的勾选,已在节点上写入一份配置把它打开并 reload 了 sshd,原有配置行一行没删。' +
        '判据是 sshd -T 读回的生效值 —— 面板手上没有口令,没有真的用口令登录一次。',
    )
  }
  if (r.password_auth_error) {
    notes.push('「顺带打开口令登录」没有成功(不影响接入,sshd 配置已恢复原样):' + r.password_auth_error)
  }
  if (notes.length) {
    // 详情里有备份文件的路径,以及「账户是锁着的」这类只有节点上才查得到的补充。
    notes.push('详情:' + r.detail)
  }
  return notes
}
