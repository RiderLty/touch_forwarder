# touch_forwarder

Android arm64 触摸转发 CLI：读取手机物理触摸屏，将多点触摸事件通过 TCP 发送给客户端。客户端连接时独占触摸屏，断开后释放；没有客户端时不影响手机触摸。

## 下载并运行

电脑连接手机并开启 USB 调试，然后执行：

```sh
curl -fL --retry 3 -o touch_forwarder_arm64 \
  "https://github.com/RiderLty/touch_forwarder/releases/latest/download/touch_forwarder_arm64?ts=$(date +%s)"
adb push touch_forwarder_arm64 /data/local/tmp/touch_forwarder_arm64
adb shell chmod 755 /data/local/tmp/touch_forwarder_arm64
adb shell /data/local/tmp/touch_forwarder_arm64 -listen :6532
```

最后一条命令持续运行。Pico 通过 USB 网卡连接安卓后，直接连接安卓 USB 网卡的 IP 地址和 `6532` 端口即可接收触摸帧。端口使用原始 TCP 二进制数据，不是 HTTP。

## 从源码构建

需要 Go 1.23 或更新版本：

```sh
./build.sh
adb push bin/touch_forwarder_arm64 /data/local/tmp/touch_forwarder_arm64
adb shell chmod 755 /data/local/tmp/touch_forwarder_arm64
adb shell /data/local/tmp/touch_forwarder_arm64 -listen :6532
```

可用 `-dev /dev/input/eventN` 指定触摸屏设备，`-v` 打印发送的触点帧。默认自动查找触摸屏并监听 `:6532`。
