#!/usr/bin/env bash
#
# build.sh —— 构建脚本，编译前自动递增版本号尾数
#
# 约定：但凡涉及程序、需要重新编译的，每次编译前默认版本号尾数 +1。
# 本脚本把这条约定自动化，避免依赖记忆。
#
# 用法：
#   ./build.sh              递增版本号并编译（默认）
#   ./build.sh --no-bump   不递增版本号，直接编译（用于调试或回退）
#
# 版本号同时注入到二进制（-ldflags -X）与源码中的默认值，
# 保证「源码看到的版本」与「二进制里的版本」始终一致。

set -euo pipefail

cd "$(dirname "$0")"

BUMP=1
if [[ "${1:-}" == "--no-bump" ]]; then
	BUMP=0
fi

# --- 读取当前版本号 ---
# 匹配 cli.go 中的 version = "x.y.z"
CURRENT=$(grep -oE 'version[[:space:]]*=[[:space:]]*"[0-9]+\.[0-9]+\.[0-9]+"' cli.go \
	| head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')

if [[ -z "$CURRENT" ]]; then
	echo "错误: 无法从 cli.go 中解析版本号" >&2
	exit 1
fi

# --- 计算新版本号（尾数 +1）---
if [[ "$BUMP" == "1" ]]; then
	IFS='.' read -r MAJOR MINOR PATCH <<<"$CURRENT"
	NEW="$MAJOR.$MINOR.$((PATCH + 1))"
else
	NEW="$CURRENT"
fi

echo "版本号: $CURRENT → $NEW"

# --- 把新版本号写回源码，保持源码与二进制一致 ---
# 只替换 version 变量的那一行，不动 author
if [[ "$NEW" != "$CURRENT" ]]; then
	sed -i.bak -E "s|(version[[:space:]]*=[[:space:]]*)\"[0-9]+\.[0-9]+\.[0-9]+\"|\1\"$NEW\"|" cli.go
	rm -f cli.go.bak
fi

# --- 编译 ---
# -s -w 剥离符号表与调试信息，二进制体积减少约 32%
# -X 注入版本号，确保二进制里的版本与源码一致
go build -trimpath -ldflags "-s -w -X main.version=$NEW" -o pngopt.exe .

echo "构建完成: pngopt.exe ($(du -h pngopt.exe | cut -f1))"

# --- 校验实际生效的版本号 ---
ACTUAL=$(./pngopt.exe -v | head -1 | awk '{print $2}')
if [[ "$ACTUAL" != "$NEW" ]]; then
	echo "错误: 二进制版本号不一致 (期望 $NEW, 实际 $ACTUAL)" >&2
	exit 1
fi
echo "版本校验通过: $ACTUAL"
