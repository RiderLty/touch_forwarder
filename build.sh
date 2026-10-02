#!/bin/sh
# 交叉编译安卓可执行文件（adb push 到 /data/local/tmp 运行）
set -e
cd "$(dirname "$0")"

export CGO_ENABLED=0
mkdir -p bin

GOOS=android GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/touch_forwarder_arm64 .
echo "OK: bin/touch_forwarder_arm64"
