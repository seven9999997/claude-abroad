# 地区命令的输出样本

- 2026-11-20 在开发机上以普通用户只读执行（m4-plan v2.4 决定 8）。**样本里的语言和地区格式已换成通用值**（`zh-Hans-CN`、`en-US`、`zh-Hans_CN`；m4-plan v2.5 建议改 2：原值带着用户所在的国家），格式（带引号的数组、带文字代码的地区格式）保持原样：
  - `defaults-read-applelanguages.txt`：`/usr/bin/defaults read -g AppleLanguages`
  - `defaults-read-applelocale.txt`：`/usr/bin/defaults read -g AppleLocale`
  - `defaults-read-missing.stderr`：`/usr/bin/defaults read -g NoSuchKeyAbroadTest` 的标准错误（退出码 1）
  - `defaults-read-timezone-auto.txt`：`/usr/bin/defaults read /Library/Preferences/com.apple.timezone.auto Active`
  - `systemsetup-nonroot.txt`：普通用户执行 `/usr/sbin/systemsetup -gettimezone`（**退出码 0**，只打这一句）
- 构造的样本（文件名带 `.constructed`，需要 root，会话里不执行；真机格式由 M4-9 核对）：
  - `systemsetup-gettimezone.constructed.txt`：root 执行 `/usr/sbin/systemsetup -gettimezone`
