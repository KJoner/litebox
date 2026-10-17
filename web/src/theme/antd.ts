import { theme } from 'ant-design-vue'
import type { ThemeConfig } from 'ant-design-vue/es/config-provider/context'
import { controlHeight, font, fontSize, palette, radius, type ThemeMode } from './tokens'

/**
 * 通过 ConfigProvider :theme 下发。按主题现算 —— 深色时换 darkAlgorithm,
 * 同时把 tokens.ts 里那组深色实值灌进去(算法只负责派生,基色仍由我们定)。
 *
 * 注意版本差异:ant-design-vue 4.2.6 的 component token 还停在 AntD v5.0 那一代命名,
 * AntD React 新版的键(Table.headerBg、Menu.itemSelectedBg…)在这里既过不了类型检查、
 * 运行期也被静默忽略。所以这里一律用本版本真正认的键。
 *
 * 还有一类键这个版本根本不给覆盖:Table 与 Card 在 styleFn 内部用 mergeToken
 * 重算了一遍自己的派生 token,components.Table 传进去的值会被当场盖掉。
 * 它们改走 alias token(见 colorFillAlter)或 styles/antd-tune.css。
 *
 * UI 2.0:按钮不再是胶囊(全局 borderRadius 8 就是按钮圆角),标签的胶囊在 Tag 上单独给。
 */
export function antdTheme(mode: ThemeMode): ThemeConfig {
  const p = palette[mode]
  return {
    algorithm: mode === 'dark' ? theme.darkAlgorithm : theme.defaultAlgorithm,
    token: {
      colorPrimary: p.brand,
      colorInfo: p.brand,
      colorSuccess: p.ok,
      colorWarning: p.warn,
      colorError: p.bad,
      colorLink: p.brand,
      colorLinkHover: p.brandHover,

      colorBgLayout: p.bg,
      colorBgContainer: p.surface,
      colorBgElevated: p.surface,
      colorBgMask: p.overlay,
      colorBorder: p.sep,
      colorBorderSecondary: p.sep2,
      colorSplit: p.sep2,

      colorText: p.text,
      colorTextSecondary: p.text2,
      colorTextTertiary: p.text3,
      colorTextQuaternary: p.text3,
      colorTextPlaceholder: p.text3,
      colorTextDescription: p.text3,
      colorTextHeading: p.text,

      /**
       * 表头底色、行 hover 都由它派生 —— UI 2.0 两者要的正好是同一个 --surface2,
       * 所以 V18 在 antd-tune.css 里那条「行 hover 单独盖」的规则不再需要。
       */
      colorFillAlter: p.surface2,
      colorFillContent: p.fill,
      colorFillSecondary: p.fill2,
      colorFillTertiary: p.fill,
      colorFillQuaternary: p.fill,

      borderRadius: radius.btn,
      borderRadiusLG: radius.card,
      borderRadiusSM: radius.sm,
      borderRadiusXS: radius.xs,

      fontFamily: font.sans,
      fontSize: fontSize.base,
      fontSizeSM: fontSize.xs,
      fontSizeLG: fontSize.lg,
      fontSizeHeading5: fontSize.lg,
      fontSizeHeading4: fontSize.xl,
      fontSizeHeading3: fontSize['2xl'],

      controlHeight: controlHeight.base,
      controlHeightSM: controlHeight.sm,
      controlHeightLG: controlHeight.lg,
      controlOutlineWidth: 3,
      controlOutline: p.brandBg,
      controlItemBgActive: p.brandBg,
      controlItemBgHover: p.fill,

      boxShadow: p.shadowLg,
      boxShadowSecondary: p.shadowLg,
      boxShadowTertiary: p.shadow,

      wireframe: false,
      motionDurationFast: '0.12s',
      motionDurationMid: '0.18s',
      motionDurationSlow: '0.24s',
      motionEaseInOut: 'cubic-bezier(.2,.8,.2,1)',
    },
    components: {
      Layout: {
        colorBgHeader: p.surface,
        colorBgBody: p.bg,
      },
      Menu: {
        colorItemBg: 'transparent',
        colorSubItemBg: 'transparent',
        colorItemBgSelected: p.brandBg,
        colorItemTextSelected: p.brand,
        colorItemBgHover: p.fill,
        radiusItem: radius.sm,
        itemMarginInline: 4,
      },
      Button: {
        borderRadius: radius.btn,
        borderRadiusSM: radius.sm,
        borderRadiusLG: radius.btn,
        controlHeight: controlHeight.base,
        controlHeightSM: controlHeight.sm,
        controlHeightLG: controlHeight.lg,
        fontSize: fontSize.base,
        fontSizeSM: fontSize.sm,
        fontSizeLG: fontSize.md,
        paddingContentHorizontal: 14,
      },
      Tag: {
        borderRadiusSM: radius.pill,
      },
      Modal: {
        borderRadiusLG: radius.sheet,
      },
      Drawer: {
        borderRadiusLG: 0,
      },
      Segmented: {
        borderRadius: radius.seg,
        borderRadiusSM: radius.sm,
        colorBgLayout: p.fill,
      },
      Tooltip: {
        borderRadius: radius.sm,
        colorBgDefault: p.tooltipBg,
        colorTextLightSolid: p.textInverse,
      },
      Card: {
        borderRadiusLG: radius.card,
        boxShadowTertiary: p.shadow,
      },
    },
  }
}
