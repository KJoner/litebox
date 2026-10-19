# TwemojiCountryFlags.woff2 的来源与许可

资产卡片左上角的地区旗帜用这个字体渲染。Windows 自带的字体里没有国旗表情,
同一个地区代码在那里只会显示成两个字母;这个字体只含国旗那一段字符(约 78 KB),
`unicode-range` 限定之后,页面上不出现国旗就不会下载它。

- 文件取自 npm 包 `country-flag-emoji-polyfill@0.1.10` 的 `dist/TwemojiCountryFlags.woff2`,
  原样复制,没有修改。SHA-256:
  `9f04f14429bb6a9f415c7a4dd902a918d7e81a4f7526c415496fdb063954e3b8`
- 那个包的代码以 MIT 许可发布(Copyright (c) 2022 TalkJS);面板只用字体文件,没有用它的代码。
- 字体里的图形来自 Twemoji,按 CC-BY-4.0 使用与再发布:

  > The Emoji art in the "Twemoji Country Flags" font comes from [Twemoji](https://twitter.github.io/twemoji),
  > and is used and redistributed under the CC-BY-4.0 [license terms](https://github.com/twitter/twemoji#license)
  > offered by the Twemoji project.
  >
  > Copyright 2020 Twitter, Inc and other contributors. Graphics licensed under CC-BY 4.0:
  > https://creativecommons.org/licenses/by/4.0/

更新时从同一个包取新版本,改掉上面的版本号与 SHA-256。
