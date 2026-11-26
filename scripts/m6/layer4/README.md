# scripts/m6/layer4：第四层的真机实测（R21、R22、R23）

验证“回国后 Claude Code 也连不上”的第四层：Claude Code 的托管设置把它指向本机代理端口 `127.0.0.1:47821`，
abroadd 在已启用时按规则转发，其余时候一律 403。对应 [docs/fallback-plan.md](../../../docs/fallback-plan.md) 第 6 节实测 1–4。

> **写入托管设置的那一刻起，这台 Mac 上所有 Claude Code（含正在运行的会话）都经本机代理端口联网**，
> 已启用时照常能用；脚本结束时删掉分片后恢复。要在你选定的那台 Mac 上做（设计 M6-10 清单第 3 节第 7 条）。
> 会用你的账号让 Claude Code 回几句很短的话。整个过程在**已启用**状态下做：即使 Claude Code 绕开了代理，
> 也在接管下经主代理出去，不会暴露本机 IP。

## 独立的流程（端到端最后会卸载，所以这份要单独装一次）

1. 开发安装：`sudo scripts/m6/install/install.sh`（见 [scripts/m6/install](../install/README.md)）；
2. 以你自己的身份打开应用，在安装引导里配好主代理，点“启用”；
3. 本脚本：`sudo -E scripts/m6/layer4/run.sh`；
4. 在应用里点“一键回国”；
5. 卸载：`sudo scripts/m6/install/uninstall.sh`（和安装前的快照逐项对照）。

在你选定的那台 Mac 上做。在这台日常用的 Mac 上做时，写入托管设置期间正在运行的 Claude Code 会话也经本机代理端口联网，
T2a/b 会让它也连不上，直到脚本删掉分片。

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
