import { ref } from 'vue'
import { api, ApiError, type DeployResult, type Node, type NodeUpdateResponse } from '@/api/client'

/**
 * 管理地址变更后的全面重检(V20),列表页与详情页共用。
 *
 * 结果借部署进度弹窗(NodeOpProgressModal)显示:重检的步骤与部署记录同形
 * (连接 → 转发 → 服务 → 配置 → 端口 → 探测 → 流量采集 → 资源采集),
 * 每一步带状态、耗时与详情,失败在哪一步、下一步做什么都在里面。
 *
 * 两条入口:
 *   afterSave  表单保存后由 recheck_required 触发,先把依赖这台机器地址的
 *              中转 / 链式主机列出来 —— 不能更新本机之后就宣布全链路成功;
 *   start      菜单里的「全面重检」,随时手动点。
 */
export function useNodeRecheck(onDone: () => void) {
  const open = ref(false)
  const title = ref('')
  const running = ref('')
  const result = ref<DeployResult | null>(null)
  const error = ref('')
  const note = ref('')
  let needsReload = false

  async function run(n: Pick<Node, 'id' | 'name' | 'display_name'>, lead: string) {
    title.value = `全面重检 · ${n.display_name || n.name}`
    running.value = '正在用新的连接参数重新核验:连接、TCP 转发、服务、配置、端口、流量与资源采集'
    result.value = null
    error.value = ''
    note.value = lead
    open.value = true
    try {
      const r = await api.recheckNode(n.id)
      result.value = {
        node_id: r.node_id,
        revision: 0,
        config_sha256: '',
        status: r.ok ? 'SUCCESS' : 'FAILED',
        steps: r.steps,
        started_at: r.started_at,
        finished_at: r.finished_at,
      } as DeployResult
      if (!r.ok) {
        error.value = r.preflight.block_reason
          ? `重检未全部通过:${r.preflight.block_reason}`
          : '重检有未通过的项,见下面的步骤'
      } else if (!lead) {
        note.value = '新地址上的这台机器已重新核验,旧的巡检与采样数据不再作数。'
      }
    } catch (e) {
      error.value = e instanceof ApiError ? e.message : '重检失败'
    } finally {
      running.value = ''
      needsReload = true
    }
  }

  function afterSave(n: Pick<Node, 'id' | 'name' | 'display_name'> | null, saved: NodeUpdateResponse) {
    if (!n) return
    const parts: string[] = []
    if (!saved.verified) parts.push('这次是按「待验证」保存的:新参数当时连不上。')
    if (saved.host_key_changed) parts.push('已接受新的主机密钥:这是一台重装过或换过的机器,库里的「已安装 / 已部署」是旧机器的事实,按下面的结果决定要不要重新安装与下发。')
    if (saved.dependents.length) {
      parts.push(
        '依赖这台机器地址的主机已标脏,会按依赖顺序自动重新下发:' +
          saved.dependents.map((d) => `${d.name}(${d.kind})`).join('、'),
      )
    }
    void run(n, parts.join('\n'))
  }

  function start(n: Pick<Node, 'id' | 'name' | 'display_name'>) {
    void run(n, '')
  }

  /** 弹窗关掉时才重拉:页面 reload 会把这个结果连同面板一起卸掉。 */
  function close(v: boolean) {
    open.value = v
    if (!v && needsReload) {
      needsReload = false
      onDone()
    }
  }

  return { open, title, running, result, error, note, afterSave, start, close }
}
