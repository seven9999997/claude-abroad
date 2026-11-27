# scripts/m6/layer4：第四层的真机实测（R21、R22、R23）

> **暂不测（用户 2026-11-27 决定）**：写入托管设置期间这台 Mac 上所有 Claude Code 都改连本机代理端口，会影响平时的用法。收尾时再由你决定补测还是定为不测；以后补测时按下面的注意事项做。R23 已挪进端到端（`scripts/m6/e2e` 的 CC1），这里的 T5 仍保留。

验证“回国后 Claude Code 也连不上”的第四层：Claude Code 的托管设置把它指向本机代理端口 `127.0.0.1:47821`，
abroadd 在已启用时按规则转发，其余时候一律 403。对应 [docs/fallback-plan.md](../../../docs/fallback-plan.md) 第 6 节实测 1–4。

> **写入托管设置的那一刻起，这台 Mac 上所有 Claude Code（含正在运行的会话）都经本机代理端口联网**，
> 已启用时照常能用；脚本结束时删掉分片后恢复。在这台 Mac 上做（用户 2026-11-27 答复），先读下面的四点。
> 会用你的账号让 Claude Code 回几句很短的话。整个过程在**已启用**状态下做：即使 Claude Code 绕开了代理，
> 也在接管下经主代理出去，不会暴露本机 IP。

## 在这台日常用的 Mac 上做之前要知道的四点（architect 2026-11-27；用户决定在这台 Mac 上做）

排在端到端**之后**：端到端先把安装、启用、回国、重启、卸载整条路走通，这份再单独装一次。

1. **做之前关掉所有 Claude Code 会话**（包括正在帮你开发的那个）。写入托管设置的那一刻起，这台 Mac 上所有 Claude Code
   都改连本机代理端口，正在运行的会话也会重新加载这份设置；而启用的前提检查本来就要求先关掉别的代理软件的隧道和系统代理，
   那时会话本来也连不上。整个实测由你自己在普通终端里做。
2. **实测期间 Claude Code 的出口是 Claude 出国的主代理**，不是你平时用的线路：脚本用你的账号让 Claude Code 回几句话，
   这几次请求对外是主代理出口的 IP。请先确认这个出口是你愿意让账号出现的地方；不愿意就不要在登录着常用账号时做。
3. **T2a、T2b 两步是故意让 Claude Code 连不上的**（指向没人监听的端口、一律回 403 的端口）。那时连接失败是预期的结果。
4. **做完确认恢复干净，再恢复平时的用法**：脚本结束时删掉托管设置的分片（死人开关到点也会删）；之后按下面的流程回国、
   执行卸载脚本，确认托管设置的分片目录里没有我们的文件，再恢复平时的代理、打开 Claude Code。中途出问题时
   `sudo scripts/m6/layer4/restore.sh` 不需要网络。

## 独立的流程（端到端最后会卸载，所以这份要单独装一次）

1. 开发安装：`sudo scripts/m6/install/install.sh`（见 [scripts/m6/install](../install/README.md)）；
2. 以你自己的身份打开应用，在安装引导里配好主代理，点“启用”；
3. 本脚本：`sudo -E scripts/m6/layer4/run.sh`；
4. 在应用里点“一键回国”；
5. 卸载：`sudo scripts/m6/install/uninstall.sh`（和安装前的快照逐项对照）。

在这台日常用的 Mac 上做：写入托管设置期间正在运行的 Claude Code 会话也经本机代理端口联网，T2a/b 会让它也连不上，
直到脚本删掉分片——所以先关掉所有会话（上面第 1 点）。

## 前提

1. 开发安装已经装好（`scripts/m6/install`），在应用里配好主代理并**启用**。回国拦截必须是测试段（开发安装默认；M6-11b）：
   用的是真实的 Anthropic 地址段时前提检查拒绝——那样回国后这台 Mac 直连 Anthropic 会被拒，正在运行的 Claude Code 会断开。
2. 这台 Mac 上有 `claude` 命令（找不到时设 `M6L_CLAUDE=<路径>`）。
3. `scripts/m6/layer4/run.sh --check-only`

## 运行

```sh
sudo -E scripts/m6/layer4/run.sh      # 开始前要输入 yes
```

| 编号 | 做什么 | 算通过 |
|---|---|---|
| T4 | 写入分片，核对普通用户可读，记下 Claude Code 版本 | 可读（是否被加载以 T0 为准） |
| T0 | 不带代理变量启动 `claude -p` | 本机代理端口有新连接 |
| T1a/b/c | 启动环境带 `HTTPS_PROXY`、`https_proxy`（指向脚本的诱饵端口 47899）、`no_proxy=*` | 仍经本机代理端口，诱饵端口没有连接（R21） |
| T2a/b | 分片改指向没人监听的 47898、一律 403 的 47897 | Claude Code 失败；成功了就是绕开了代理（R22） |
| T3 | （可选）在 Claude 桌面版里开 Code 会话 | 本机代理端口有新连接 |
| T5 | 普通终端 `curl -x http://127.0.0.1:47821 …` | 状态里识别出 curl 的程序、不按 Claude 处理（R23） |

核对依据是 abroadd 状态里的 `claude_code.accepted`、`refused`、`recent`（最近 16 条连接：识别出的程序、是否按
Claude 处理、去向；只有域名，不含地址）。注意：同一时间别的 Claude Code 会话也在联网的话，`recent` 的第一条可能是它的。

结束时脚本删掉分片（内容和写入的一致才删）、结束自己的监听；abroadd 还在已启用状态，之后在应用里自己回国或停用。
结果在 `scripts/m6/layer4/out/m6l-results-*.txt`，请连同同目录的输出一起贴回。

## 应急

`sudo scripts/m6/layer4/restore.sh`：删分片、结束本脚本的监听、取消死人开关（默认 1800 秒后自动执行）。不动 abroadd 和网络。

## 会话里能做的

```sh
DRY_RUN=1 scripts/m6/layer4/run.sh >/tmp/l1; scripts/m6/layer4/check-commands.sh /tmp/l1
scripts/m6/layer4/selftest.sh
```
