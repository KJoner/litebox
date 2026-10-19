<script setup lang="ts">
import type { BoceEstimate } from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import { points, yuan } from './boceText'

/**
 * 费用估算(数字全部来自 /api/boce/estimate,与调度器用同一个函数数时间槽、按真实日历月算)。
 *
 * 面板自己的估算与 Boce 账户的真实余额分开写:余额可能被面板以外的调用消耗,
 * 「还能跑几天」只是按全站日均除出来的参考。
 */
defineProps<{ estimate: BoceEstimate | null; loading?: boolean; error?: string; targets?: number }>()
</script>

<template>
  <div class="bep" data-testid="boce-estimate" aria-live="polite">
    <div class="bep__head">
      <span class="bep__title">
        预计费用
        <LbInfoTip
          :width="340"
          text="按当前 TCPing 定价估算:每个拨测点每次约 1 波点、1 波点约 ¥0.002(两个数在系统设置里改)。按真实日历月计算,不固定为 30 天。这只是估算,实际扣费、套餐与优惠以 Boce 的记录为准;网页版每日免费次数不计入 API。"
        />
      </span>
      <span v-if="loading" class="bep__muted">计算中…</span>
    </div>
    <div v-if="error" class="lb-error-strip">{{ error }}</div>
    <template v-else-if="estimate">
      <div class="bep__grid lb-tabular">
        <div>
          <span>每轮</span><b>{{ points(estimate.points_per_round * (targets ?? 1)) }}</b>
        </div>
        <div>
          <span>今日 {{ estimate.rounds_today }} 轮</span><b>{{ points(estimate.points_today) }}</b>
        </div>
        <div>
          <span>本月({{ estimate.month }},{{ estimate.days_in_month }} 天){{ estimate.rounds_month }} 轮</span>
          <b>{{ points(estimate.points_month) }} · {{ yuan(estimate.yuan_month) }}</b>
        </div>
        <div>
          <span>日均</span><b>{{ points(Math.round(estimate.daily_avg_points * 10) / 10) }}</b>
        </div>
        <div>
          <span>全站合计(含其余在跑的任务)</span>
          <b>{{ points(estimate.total_month_points) }} / 月 · {{ yuan(estimate.total_yuan_month) }}</b>
        </div>
        <div>
          <span>账户剩余(Boce 实际余额)</span>
          <b>
            {{ estimate.balance_points == null ? '未查询' : points(estimate.balance_points) }}
            <template v-if="estimate.days_runnable != null">· 约可运行 {{ Math.floor(estimate.days_runnable) }} 天</template>
          </b>
        </div>
      </div>
      <div v-if="estimate.over_run_limit" class="bep__warn">
        每轮 {{ estimate.points_per_round }} 波点超过了单次任务上限 {{ estimate.run_limit }}:每一轮都会被拦下(记「未执行」),减少拨测点或提高上限。
      </div>
      <div v-if="estimate.over_daily" class="bep__warn">
        加上其余任务,轮数最多的那一天约 {{ estimate.peak_day_points }}+ 波点,可能超过每日上限 {{ estimate.daily_limit }}:超出的轮次会被拦下。
      </div>
      <div v-if="estimate.over_monthly" class="bep__warn">
        全站本月约 {{ estimate.total_month_points }} 波点,超过每月上限 {{ estimate.monthly_limit }}:月底前会有一部分轮次被拦下。
      </div>
      <div v-if="!estimate.daily_limit || !estimate.monthly_limit" class="bep__warn">
        还没有设置每日 / 每月波点上限:没有预算时不会创建任何付费检测。
      </div>
    </template>
    <div v-else class="bep__muted">选好拨测点与时间表后显示</div>
  </div>
</template>

<style scoped>
.bep {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border-radius: var(--r-group);
  background: var(--surface2);
}
.bep__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.bep__title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text2);
}
.bep__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(170px, 1fr));
  gap: var(--sp-2);
}
.bep__grid > div {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.bep__grid span {
  font-size: var(--fs-2xs);
  color: var(--text3);
}
.bep__grid b {
  font-size: var(--fs-sm);
  color: var(--text);
}
.bep__warn {
  font-size: var(--fs-xs);
  color: var(--warn);
}
.bep__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
</style>
