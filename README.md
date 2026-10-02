# touch_forwarder

安卓端触摸转发器（Go 实现，纯标准库 + [go-evdev](https://github.com/holoplot/go-evdev)，无 cgo），
经 adb shell 运行在安卓手机上：

- 读取本机**真实触摸屏**（Linux evdev 协议 B 多点触摸）；
- 监听 TCP：**有客户端接入 → `EVIOCGRAB` 独占触摸屏**（安卓系统收不到触摸，
  相当于锁屏），并把触点编码成 55 AA 控制帧转发给客户端；
  **无连接 → 不加锁、不转发**，手机触摸完全正常；
- 一个时刻只服务一个客户端，后来者顶掉前者（旧连接被关闭，锁随之交接）；
- 客户端断开 → 立即 `Ungrab` 释放触摸屏。

## 线上协议（与固件 TCP 6532 控制帧对齐）

```
[0x55 0xAA][LEN=0x0B][CMD=0xFF][action(1)][id(1)][x(i32 LE)][y(i32 LE)]
```

| 字段 | 含义 |
|------|------|
| CMD `0xFF` | `PIO_CMD_TOUCH`（固件 `handle_control_frame.c`） |
| action | `1`=DOWN `2`=MOVE `3`=UP |
| id | 外部触点 ID（1..255 循环，固件映射到独占 HID 槽位） |
| x/y | 已从 evdev 原始量程缩放到 **0..0x7FFFFFFE**（固件 TOUCH_MAX 坐标空间） |

一帧触点的 UP→DOWN→MOVE 顺序与安卓多点触摸语义一致（同一 SYN_REPORT 内
先结束旧触点再开启新触点）。客户端接入瞬间会把**已按下的触点补发 DOWN**；
读取协程自启动起持续消费设备事件，不存在积压帧；曾有的"清积压"双读者设计会截胡第一帧，已删除）。

## 构建与部署

```bash
./build.sh                       # 产出 bin/touch_forwarder_arm64
adb push bin/touch_forwarder_arm64 /data/local/tmp/
adb shell chmod +x /data/local/tmp/touch_forwarder_arm64
adb shell su -c /data/local/tmp/touch_forwarder_arm64 -listen :6532
```

读 `/dev/input/event*` 与 `EVIOCGRAB` 都需要 root（`su -c`）或 input 组权限。
`-listen` 换成手机局域网 IP 可达的端口；若客户端走 adb 隧道，配合
`adb forward tcp:6532 tcp:6532` 后连 `127.0.0.1:6532`。

## 参数

| 参数 | 默认 | 说明 |
|------|------|------|
| `-listen` | `:6532` | TCP 监听地址 |
| `-dev` | 自动 | 指定 `/dev/input/eventN`；缺省扫描所有节点，取第一个具备 `ABS_MT_POSITION_X/Y + ABS_MT_TRACKING_ID` 的（协议 B 触摸屏） |
| `-v` | 关 | 打印每个发出的触点帧（排查用） |

## 已知边界

- **协议 B 专用**：不支持老式协议 A（无 tracking id 的 MT 流）；近十年的安卓
  触摸屏基本都是协议 B。
- **Grab 需要写权限**：部分 ROM 只读打开时 `Grab` 会失败，程序退化为
  "只转发不锁屏"（手机自身同时还会收到触摸），日志里有提示。
- **中途接入的进行中触点**：客户端接入时若有手指正按着，grab 后该触点在安卓
  侧没有 UP（系统视角"卡住"）——转发器会把在场触点对新客户端补发 DOWN，
  手指抬起后一切恢复正常；实践上先连后碰即可完全规避。
- **坐标直出，无旋转逻辑**：转发的就是触控控制器的原始坐标（仅做量程归一化到
  0..0x7FFFFFFE，X/Y 轴不交换、不旋转）。手机与接收端握持方向一致时坐标天然 1:1
  对应；方向不一致属于接收端的事——Pico 面板的屏幕方向设置即可适配，
  转发器不做（也不该做）任何旋转。
