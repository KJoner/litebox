import { computed, ref } from 'vue'
import { api, ApiError, type ExpiryKind, type ExpiryProfilePayload, type ExpiryView } from '@/api/client'
import { blankExpiryForm, parseLeadDaysText, type ExpiryFormValue } from './expiryMeta'

/**
 * 编辑表单里的「供应商到期」分组。三个表单(节点 / 外部代理 / 代理源)共用:
 * 回填、脏判断、保存三件事只写一遍。
 *
 * 档案是独立接口,在主对象保存成功之后顺带保存 —— 它一个字节都不进节点配置,
 * 不需要部署、不标脏。失败只提示,不让主保存失败:对象已经存好了。
 */
export function useExpiryForm() {
  const form = ref<ExpiryFormValue>(blankExpiryForm())
  /** 外部代理:跟随来源(不单独设置)。其他对象恒为 false。 */
  const inherit = ref(false)
  let snapshot = ''

  function state() {
    return JSON.stringify({ f: form.value, i: inherit.value })
  }

  function fill(view: ExpiryView | null | undefined, canInherit = false) {
    if (!view || view.state === 'UNSET' && !view.has_profile && !view.inherited) {
      form.value = blankExpiryForm()
      // 没档案的外部代理默认跟随来源(有来源的话)。
      inherit.value = canInherit
    } else {
      form.value = {
        expires_at: view.expires_at,
        reminder_enabled: view.reminder_enabled,
        auto_renew: view.auto_renew,
        vendor_name: view.vendor_name,
        vendor_url: view.vendor_url,
        note: view.note,
        lead_days_text: view.lead_days.join(','),
      }
      inherit.value = canInherit && view.inherited
    }
    snapshot = state()
  }

  const dirty = computed(() => state() !== snapshot)

  function payload(): ExpiryProfilePayload {
    const f = form.value
    return {
      expires_at: f.expires_at,
      reminder_enabled: f.reminder_enabled,
      auto_renew: f.auto_renew,
      vendor_name: f.vendor_name,
      vendor_url: f.vendor_url,
      note: f.note,
      lead_days: parseLeadDaysText(f.lead_days_text),
    }
  }

  /** 保存;没改动就什么都不做。返回错误文案(空串 = 成功或无需保存)。 */
  async function save(kind: ExpiryKind, id: number): Promise<string> {
    if (!dirty.value) return ''
    try {
      if (inherit.value) {
        await api.clearExpiry(kind, id)
      } else {
        await api.saveExpiry(kind, id, payload())
      }
      snapshot = state()
      return ''
    } catch (err) {
      return err instanceof ApiError ? err.message : '到期档案保存失败'
    }
  }

  return { form, inherit, dirty, fill, save }
}
