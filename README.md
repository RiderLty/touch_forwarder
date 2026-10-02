# touch_forwarder

在已 root 的 Android 设备上读取物理触摸屏事件，并通过 TCP 转发触点的 Go CLI。客户端连接时，程序尝试用 `EVIOCGRAB` 独占触摸屏；客户端断开后释放独占。没有客户端时，手机照常接收触摸。

目前提供 **Android arm64** 可执行文件。设备需要协议 B 多点触摸屏，以及访问 `/dev/input/event*` 的权限（通常通过 `su` 获取）。一个时刻只服务一个 TCP 客户端；新连接会替换旧连接。

## 下载并运行

在电脑上安装 [adb](https://developer.android.com/tools/adb) 和 `curl`，开启手机 USB 调试并连接设备。下面的命令可以逐行复制：

```sh
curl -fL --retry 3 -o touch_forwarder_arm64 \
  https://github.com/RiderLty/touch_forwarder/releases/latest/download/touch_forwarder_arm64
adb devices
adb push touch_forwarder_arm64 /data/local/tmp/touch_forwarder_arm64
adb shell chmod 755 /data/local/tmp/touch_forwarder_arm64
adb shell 'su -c "/data/local/tmp/touch_forwarder_arm64 -listen :6532"'
```

最后一条命令会持续运行并打印日志，另开一个终端连接客户端。通过 adb 隧道访问时，先在电脑上执行：

```sh
adb forward tcp:6532 tcp:6532
```

然后让接收端连接电脑的 `127.0.0.1:6532`。如果接收端与手机在同一网络，也可直接连接手机 IP 的 `6532` 端口；这种方式无需 `adb forward`。端口传输的是下述**原始 TCP 二进制帧**，不是 HTTP。

仅需查看触点数据时，支持 TELNET 协议的 `curl` 可以充当临时 TCP 客户端；触摸手机屏幕即可看到十六进制数据，按 Ctrl-C 断开：

```sh
curl --no-buffer telnet://127.0.0.1:6532 | xxd -g 1
```

如果手机没有 `su`，或程序无法打开触摸屏设备，就无法正常工作。连接前运行 `adb shell su -c id` 可检查 root 是否可用。若日志提示 `Grab 失败`，触点仍会转发，但手机系统也会同时收到触摸。

## 从源码构建

需要 Go 1.21 或更新版本。在仓库目录执行：

```sh
./build.sh
adb push bin/touch_forwarder_arm64 /data/local/tmp/touch_forwarder_arm64
adb shell chmod 755 /data/local/tmp/touch_forwarder_arm64
adb shell 'su -c "/data/local/tmp/touch_forwarder_arm64 -listen :6532"'
```

`build.sh` 使用 `GOOS=android GOARCH=arm64 CGO_ENABLED=0`，输出为 `bin/touch_forwarder_arm64`。

## 参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `:6532` | TCP 监听地址 |
| `-dev` | 自动发现 | 指定触摸屏设备，如 `/dev/input/event2` |
| `-v` | 关闭 | 打印每个发送的触点帧 |

排查设备选择时可追加 `-dev /dev/input/eventN -v`。自动发现会查找带 `ABS_MT_POSITION_X/Y` 和 `ABS_MT_TRACKING_ID` 的设备，并跳过坐标范围为 `0..32767` 的虚拟触屏。

## TCP 帧格式

每个触点事件发出一个 14 字节帧，客户端按字节流解析：

```text
55 AA 0B FF action id x0 x1 x2 x3 y0 y1 y2 y3
```

| 字段 | 含义 |
| --- | --- |
| `55 AA` | 帧头 |
| `0B` | 后续数据长度，11 字节 |
| `FF` | `PIO_CMD_TOUCH` 命令 |
| `action` | `1` 按下、`2` 移动、`3` 抬起 |
| `id` | 触点 ID，范围 1..255，循环使用 |
| `x`、`y` | little-endian 32 位整数；原始坐标缩放到 `0..0x7FFFFFFE` |

同一个 `SYN_REPORT` 内按抬起、按下、移动的顺序发送。程序不旋转或交换 X/Y 轴；接收端需要自行适配屏幕方向。

## 自动发布

每次向任意分支 push，GitHub Actions 都会构建 arm64 CLI，并用 `gh release create` 发布 `touch_forwarder_arm64` 和 `SHA256SUMS`。`main` 分支的构建标记为 Latest，供上面的 `curl` 命令下载；其他分支的构建标记为预发布版本。每次构建使用独立的 `build-<run-id>-<attempt>` 标签。

## 已知限制

- 只支持带 `ABS_MT_TRACKING_ID` 的协议 B 触摸屏和 Android arm64。
- 如果在手指已经按住屏幕时连接，Android 侧可能收不到该触点的抬起事件；先连接再触摸可避免这种情况。
- 程序只负责 TCP 触点帧，不实现接收端固件或触摸方向转换。
