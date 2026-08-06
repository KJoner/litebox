/*
 * 自托管字体。面板要 embed 进单个二进制,不能引 CDN ——
 * 内网部署与断网环境下 CDN 字体只会静默回落到系统字体栈。
 *
 * 拉丁与数字用 IBM Plex(三档 Sans + 一档 Mono),中文用 Noto Sans SC 两档。
 * 中文引的是带编号的分片版(`400.css`/`500.css`)而不是整包 `chinese-simplified`:
 * 前者每个 @font-face 带 unicode-range,浏览器只下载页面上真正出现的那几片
 * (通常十来片、几百 KB);后者是一整个 1.1MB 的文件,一进页面就得全拉。
 * 代价是产物里多出两百来个 woff2 —— 磁盘换传输,对一台 VPS 上的面板是划算的。
 */

// 拉丁只取 latin 子集:Noto 也自带拉丁字形,但字体栈里 Plex 在前,轮不到它。
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-mono/latin-400.css'

import '@fontsource/noto-sans-sc/400.css'
import '@fontsource/noto-sans-sc/500.css'
