# 虚拟机镜像

`manifest.json` 列出镜像的全部输入：Alpine netboot（内核和模块）、minirootfs、sing-box、mihomo。地址固定，SHA-256 取自官方发布页（Alpine 的 `.sha256` 文件、GitHub 发布接口的 `digest` 字段）。引擎版本只在这里维护。

## 构建

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent
go run ./cmd/abroad-imagebuild build -agent guest/out/abroad-agent
go run ./cmd/abroad-imagebuild verify
```

- 不需要 root，不用 Docker/Lima，只写 `guest/out/`（已在 .gitignore）。
- 第一次构建会下载约 360 MB（netboot 包 327 MB，里面还有用不到的 lts、rpi 内核），之后用 `guest/out/cache/` 里的缓存，校验通过就不再下载。
- 任何一个文件的 SHA-256 与清单不符都会停止，不会留下半成品。
- 下载不设总超时，连续 60 秒收不到数据才中止，可以随时 Ctrl-C。网络慢时也可以用别的工具把文件下到 `guest/out/cache/`，文件名与地址的最后一段相同，校验通过就直接用。

## 输出（`guest/out/`）

| 文件 | 内容 |
|---|---|
| `Image` | 未压缩的 arm64 内核（从 EFI zboot 格式的 `vmlinuz-virt` 解出） |
| `initramfs.gz` | abroad-agent 作为 `/init`，加上挂根盘需要的模块（virtio_blk、squashfs，取自 Alpine 的 initramfs 并解压成 `.ko`），以及 `/dev/console` |
| `rootfs.img` | 只读根镜像（squashfs，zlib 压缩）：minirootfs + sing-box + mihomo + abroad-agent，属主一律 root，`/etc/resolv.conf` 为空 |
| `modloop` | Alpine 的模块镜像（squashfs），作为第二块只读盘挂上，agent 从这里加载 vsock、virtio_net、overlay 等模块 |
| `SHA256SUMS`、`build.json` | 校验值；内核版本、启动模块、各输入的版本 |

## 更新版本

1. 到官方发布页取新版本的地址和 SHA-256，改 `manifest.json`。
2. 重新构建并执行 `verify`。
3. 许可证：引擎都以独立程序的形式在虚拟机里运行，见仓库根目录的 `NOTICE`。
