/*
 * 自托管字体。面板要 embed 进单个二进制,不能引 CDN ——
 * 内网部署与断网环境下 CDN 字体只会静默回落到系统字体栈。
 *
 * V18 起拉丁与数字走系统栈(-apple-system / SF Pro / Helvetica Neue),
 * 不再加载 IBM Plex。中文仍需要自托管:Windows / Linux 上没有 PingFang,
 * 系统栈会落到微软雅黑或 WenQuanYi,与 SF 风格的拉丁字形不搭。
 *
 * 引的是带编号的分片版(`400.css`/`500.css`/`700.css`)而不是整包
 * `chinese-simplified`:前者每个 @font-face 带 unicode-range,浏览器只下载
 * 页面上真正出现的那几片(通常十来片、几百 KB);后者是一整个 1.1MB 的文件,
 * 一进页面就得全拉。700 是给 34px 的页面大标题用的。
 */
import '@fontsource/noto-sans-sc/400.css'
import '@fontsource/noto-sans-sc/500.css'
import '@fontsource/noto-sans-sc/700.css'
