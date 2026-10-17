<script setup lang="ts">
import { computed } from 'vue'
import { RouterView } from 'vue-router'
import zhCN from 'ant-design-vue/es/locale/zh_CN'
import { antdTheme } from '@/theme/antd'
import { useThemeStore } from '@/stores/theme'

const themeStore = useThemeStore()
// 按主题现算一份 ThemeConfig。深色时换 darkAlgorithm,基色仍来自 tokens.ts。
const antd = computed(() => antdTheme(themeStore.mode))
</script>

<template>
  <!--
    视觉全部通过 ConfigProvider :theme 下发,加一份 styles/antd-tune.css 补 Token
    够不到的几处(表格、下划线 Tabs、弹窗底栏、贴边抽屉、卡片吐司)。颜色、字号、圆角
    一律来自 styles/tokens.css 的 CSS 变量,组件的 scoped CSS 里不再出现散落的字面值。
  -->
  <!--
    auto-insert-space-in-button 关掉:AntD 默认在两个汉字的按钮里插一个空格(「刷 新」),
    与其它按钮的字距不一致,也不是中文排版的习惯。
  -->
  <a-config-provider :locale="zhCN" :theme="antd" :auto-insert-space-in-button="false">
    <a href="#lb-main" class="lb-skip-link">跳到主内容</a>
    <RouterView />
  </a-config-provider>
</template>
