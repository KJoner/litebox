import type { CloudNodeView } from '@/api/client'
import type { LbStatusMeta } from '@/components/lb/statusMeta'
import { color } from '@/theme/tokens'

/**
 * 云实例状态的三重编码(形状 + 文案 + 颜色),与节点状态同一套规矩。
 *
 * 「已停机」按谁停的分两档:面板停的用暂停形状(那是一个刻意的决定,
 * 与「节点停发订阅」同类),别人停的用方框 —— 管理员看到方框要去阿里云
 * 控制台看一眼为什么。取值是阿里云的原文,文案由后端翻译好(status_label)。
 */
export function cloudStatusMeta(v: CloudNodeView): LbStatusMeta {
  switch (v.instance_status) {
    case 'Running':
      return { text: '运行中', shape: 'dot', fg: color.success, bg: color.successBg, bd: color.successBorder }
    case 'Stopped':
      if (v.stopped_by) {
        return {
          text: '已停机',
          shape: 'pause',
          fg: color.maintenance,
          bg: color.maintenanceBg,
          bd: color.maintenanceBorder,
        }
      }
      return { text: '已停止', shape: 'square', fg: color.danger, bg: color.dangerBg, bd: color.dangerBorder }
    case 'Starting':
    case 'Stopping':
    case 'Pending':
      return { text: v.status_label, shape: 'spinner', fg: color.brand, bg: color.brandBg, bd: color.brandBorder }
    default:
      return { text: '未查询', shape: 'ring', fg: color.neutral, bg: color.neutralBg, bd: color.neutralBorder }
  }
}

/** 池子用量的告警档,与节点额度的分档一致(80% 黄、阈值红、100% 用尽)。 */
export function cloudUsageLevel(v: {
  usage_percent: number | null
  over: boolean
  quota_bytes: number
}): 'UNLIMITED' | 'NORMAL' | 'WARNING' | 'DANGER' | 'EXCEEDED' {
  if (v.quota_bytes <= 0) return 'UNLIMITED'
  if (v.usage_percent === null) return 'NORMAL'
  if (v.usage_percent >= 100) return 'EXCEEDED'
  if (v.over) return 'DANGER'
  if (v.usage_percent >= 80) return 'WARNING'
  return 'NORMAL'
}

/** 阿里云常用区域,给表单的下拉提示用;不在列表里的照样能手填。 */
export const CLOUD_REGIONS: { value: string; label: string }[] = [
  { value: 'cn-hongkong', label: '中国香港' },
  { value: 'ap-southeast-1', label: '新加坡' },
  { value: 'ap-northeast-1', label: '日本(东京)' },
  { value: 'ap-northeast-2', label: '韩国(首尔)' },
  { value: 'us-west-1', label: '美国(硅谷)' },
  { value: 'us-east-1', label: '美国(弗吉尼亚)' },
  { value: 'eu-central-1', label: '德国(法兰克福)' },
  { value: 'eu-west-1', label: '英国(伦敦)' },
  { value: 'cn-hangzhou', label: '华东 1(杭州)' },
  { value: 'cn-shanghai', label: '华东 2(上海)' },
  { value: 'cn-beijing', label: '华北 2(北京)' },
  { value: 'cn-shenzhen', label: '华南 1(深圳)' },
]

export const GIB = 1024 * 1024 * 1024
