#!/usr/bin/env bash
# 用 ad-hoc 签名（codesign -s -）给 abroad-vm 加上 com.apple.security.virtualization 授权。
# 不需要开发者账号，不改系统设置。用法：scripts/dev/sign.sh <二进制路径>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
bin="${1:?用法：sign.sh <二进制路径>}"
codesign -s - --force --entitlements "${here}/entitlements.plist" "${bin}"
codesign -d --entitlements - "${bin}" 2>/dev/null | grep -q com.apple.security.virtualization \
  || { echo "签名后没有看到虚拟化授权：${bin}" >&2; exit 1; }
echo "已签名：${bin}"
