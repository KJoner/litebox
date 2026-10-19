<script setup lang="ts">
import { computed } from 'vue'
import type { BoceScheduleSpec } from '@/api/client'
import { LbInfoTip } from '@/components/lb'
import { COMMON_TIMEZONES } from './boceText'

/**
 * Boce 任务的时间表:固定间隔(+ 运行时段)或固定时间点,加运行日期与时区。
 *
 * 只编辑,不数轮数 —— 每天几轮、每月多少波点由后端按同一个调度函数算(估算面板),
 * 前端自己数会与引擎实际执行的对不上(典型的是 24:00 那一轮)。
 */
const props = defineProps<{ modelValue: BoceScheduleSpec }>()
const emit = defineEmits<{ (e: 'update:modelValue', v: BoceScheduleSpec): void }>()

const s = computed(() => props.modelValue)
function patch(p: Partial<BoceScheduleSpec>) {
  emit('update:modelValue', { ...s.value, ...p })
}

const INTERVALS = [30, 60, 120, 180, 240, 360, 720]

function clocks(withEnd: boolean): string[] {
  const out: string[] = []
  for (let m = withEnd ? 30 : 0; m <= (withEnd ? 1440 : 1410); m += 30) {
    out.push(`${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`)
  }
  return out
}
const startOptions = computed(() => {
  const list = clocks(false)
  return list.includes(s.value.window_start) ? list : [s.value.window_start, ...list]
})
const endOptions = computed(() => {
  const list = clocks(true)
  return list.includes(s.value.window_end) ? list : [...list, s.value.window_end]
})
const hourOptions = computed(() => clocks(false))
const crossMidnight = computed(
  () => s.value.schedule_mode === 'INTERVAL' && s.value.window_end !== '24:00' && s.value.window_end <= s.value.window_start,
)

const tzOptions = computed(() =>
  COMMON_TIMEZONES.includes(s.value.timezone) ? COMMON_TIMEZONES : [s.value.timezone, ...COMMON_TIMEZONES],
)

const WEEKDAYS = ['一', '二', '三', '四', '五', '六', '日']
function toggleDay(i: number) {
  const next = s.value.weekdays ^ (1 << i)
  if (next === 0) return // 至少留一天
  patch({ weekdays: next })
}

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/
function onTimes(v: unknown) {
  const list = (Array.isArray(v) ? v : []).map((x) => String(x).trim()).filter((x) => HHMM.test(x))
  patch({ times: [...new Set(list)].sort() })
}
</script>

<template>
  <div class="bse" data-testid="boce-schedule-editor">
    <div class="bse__row">
      <span class="bse__k">调度方式</span>
      <div class="lb-seg lb-seg--sm" role="radiogroup" aria-label="调度方式">
        <button
          type="button"
          role="radio"
          class="lb-seg__item"
          :class="{ 'lb-seg__item--on': s.schedule_mode === 'INTERVAL' }"
          :aria-checked="s.schedule_mode === 'INTERVAL'"
          @click="patch({ schedule_mode: 'INTERVAL' })"
        >
          固定间隔
        </button>
        <button
          type="button"
          role="radio"
          class="lb-seg__item"
          :class="{ 'lb-seg__item--on': s.schedule_mode === 'TIMES' }"
          :aria-checked="s.schedule_mode === 'TIMES'"
          @click="patch({ schedule_mode: 'TIMES', times: s.times.length ? s.times : ['08:00', '12:00', '18:00', '22:00'] })"
        >
          固定时间点
        </button>
      </div>
    </div>

    <template v-if="s.schedule_mode === 'INTERVAL'">
      <div class="bse__row">
        <span class="bse__k">检测频率</span>
        <a-select
          :value="INTERVALS.includes(s.interval_min) ? s.interval_min : -1"
          class="bse__sel"
          aria-label="检测频率"
          @update:value="(v: unknown) => Number(v) > 0 && patch({ interval_min: Number(v) })"
        >
          <a-select-option v-for="m in INTERVALS" :key="m" :value="m">每 {{ m }} 分钟</a-select-option>
          <a-select-option :value="-1">自定义</a-select-option>
        </a-select>
        <a-input-number
          :value="s.interval_min"
          :min="10"
          :max="1440"
          :precision="0"
          size="small"
          addon-after="分钟"
          class="bse__num"
          aria-label="自定义间隔(分钟)"
          @update:value="(v: number | null) => v && patch({ interval_min: v })"
        />
      </div>
      <div class="bse__row">
        <span class="bse__k">
          运行时段
          <LbInfoTip
            :width="320"
            text="从起点对齐:08:00–24:00 每 60 分钟 = 08:00、09:00 … 23:00,24:00 不执行。终点不晚于起点表示跨午夜(例如 22:00–06:00,那一段算起点那一天)。时段外不会创建新的付费检测,已经建了的照常取结果。"
          />
        </span>
        <a-select :value="s.window_start" class="bse__sel bse__sel--time" aria-label="时段起点" show-search @update:value="(v: unknown) => patch({ window_start: String(v) })">
          <a-select-option v-for="c in startOptions" :key="c" :value="c">{{ c }}</a-select-option>
        </a-select>
        <span class="bse__muted">至</span>
        <a-select :value="s.window_end" class="bse__sel bse__sel--time" aria-label="时段终点" show-search @update:value="(v: unknown) => patch({ window_end: String(v) })">
          <a-select-option v-for="c in endOptions" :key="c" :value="c">{{ c }}</a-select-option>
        </a-select>
        <span v-if="crossMidnight" class="bse__muted">跨午夜</span>
      </div>
    </template>
    <div v-else class="bse__row">
      <span class="bse__k">时间点</span>
      <a-select
        :value="s.times"
        mode="tags"
        class="bse__times"
        aria-label="固定时间点"
        placeholder="选择或输入 HH:MM"
        :token-separators="[',', ' ']"
        @update:value="onTimes"
      >
        <a-select-option v-for="c in hourOptions" :key="c" :value="c">{{ c }}</a-select-option>
      </a-select>
    </div>

    <div class="bse__row">
      <span class="bse__k">运行日期</span>
      <div class="bse__days" role="group" aria-label="运行日期">
        <button
          v-for="(d, i) in WEEKDAYS"
          :key="d"
          type="button"
          class="bse__day"
          :class="{ 'bse__day--on': s.weekdays & (1 << i) }"
          :aria-pressed="!!(s.weekdays & (1 << i))"
          :aria-label="`周${d}`"
          @click="toggleDay(i)"
        >
          {{ d }}
        </button>
      </div>
      <button type="button" class="lb-linkbtn" @click="patch({ weekdays: 127 })">每天</button>
      <button type="button" class="lb-linkbtn" @click="patch({ weekdays: 31 })">工作日</button>
      <button type="button" class="lb-linkbtn" @click="patch({ weekdays: 96 })">周末</button>
    </div>
    <div class="bse__row">
      <span class="bse__k">时区</span>
      <a-select :value="s.timezone" class="bse__sel" aria-label="时区" show-search @update:value="(v: unknown) => patch({ timezone: String(v) })">
        <a-select-option v-for="z in tzOptions" :key="z" :value="z">{{ z }}</a-select-option>
      </a-select>
    </div>
  </div>
</template>

<style scoped>
.bse {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.bse__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
}
.bse__k {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  width: 84px;
  flex: none;
  font-size: var(--fs-sm);
  color: var(--text2);
}
.bse__sel {
  width: 150px;
}
.bse__sel--time {
  width: 96px;
}
.bse__num {
  width: 130px;
}
.bse__times {
  flex: 1 1 260px;
  min-width: 0;
}
.bse__days {
  display: inline-flex;
  gap: 4px;
}
.bse__day {
  width: 28px;
  height: 28px;
  border: 1px solid var(--sep);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--text2);
  cursor: pointer;
  font-size: var(--fs-xs);
}
.bse__day--on {
  border-color: var(--brand);
  background: var(--brand-bg);
  color: var(--brand);
  font-weight: 600;
}
.bse__muted {
  font-size: var(--fs-xs);
  color: var(--text3);
}
@media (max-width: 767px) {
  .bse__k {
    width: 100%;
  }
}
</style>
