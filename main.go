// touch_forwarder：安卓端触摸转发器（adb shell 运行，纯 Go 无 cgo）
//
// 读取本机真实触摸屏（Linux evdev 协议 B 多点触摸），监听 TCP：
//   - 有客户端连接 → EVIOCGRAB 独占触摸屏（安卓系统不再收到触摸），
//     按 55 AA 控制帧把触摸转发给客户端（帧格式与固件 handle_control_frame.c
//     的 PIO_CMD_TOUCH 一致：55 AA | LEN=0x0B | 0xFF | action | id | x | y）；
//   - 无连接 → 不加锁、不转发，手机触摸完全正常。
//
// 坐标缩放：evdev 原始量程 → 固件 TOUCH_MAX 0..0x7FFFFFFE（外部触点坐标空间）。
// action：DOWN=1 / MOVE=2 / UP=3。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	evdev "github.com/holoplot/go-evdev"
)

// 触点坐标空间：固件引擎的 TOUCH_MAX（0x7FFFFFFE），不是 HID 描述符的 32767
// ——touchscreen_send 按这个空间做最终缩放，喂 32767 会把所有触点压到左上角。
const touchCoordMax = 0x7FFFFFFE

func main() {
	var (
		listenAddr = flag.String("listen", ":6532", "TCP 监听地址（客户端连进来才开始锁屏转发）")
		devPath    = flag.String("dev", "", "触摸屏 evdev 节点（缺省自动扫描 /dev/input/event*）")
		verbose    = flag.Bool("v", false, "打印每个发出的触点帧")
	)
	flag.Parse()

	// 自动发现多点触摸屏（协议 B：X/Y + TRACKING_ID）
	dev, err := findTouchscreen(*devPath)
	if err != nil {
		log.Fatalf("[forwarder] %v", err)
	}
	defer dev.Close()

	name, _ := dev.Name()
	log.Printf("[forwarder] 触摸屏: %s (%s)", name, *devPath)

	// 读取 X/Y 量程用于缩放（合成事件也要按同一坐标系转发）
	abs, err := dev.AbsInfos()
	if err != nil {
		log.Fatalf("[forwarder] AbsInfos 失败: %v", err)
	}
	xr, okx := abs[evdev.ABS_MT_POSITION_X]
	yr, oky := abs[evdev.ABS_MT_POSITION_Y]
	if !okx || !oky {
		log.Fatalf("[forwarder] 设备缺少 ABS_MT_POSITION_X/Y 量程")
	}
	log.Printf("[forwarder] 量程 X=[%d..%d] Y=[%d..%d] → 0..%d",
		xr.Minimum, xr.Maximum, yr.Minimum, yr.Maximum, touchCoordMax)

	// 客户端管理：一个时刻只服务一个客户端，后来者顶掉前者。
	// 顶替时旧连接由 accept 侧同步关闭（旧 goroutine 的 clearIf 会因现任
	// 已换人而失效，不会把新连接的 Grab 误放掉）。
	// relay 建在 tracker 之前并**注入 tracker**——emit 检查的就是这一份，
	// 之前 main 与 tracker 各建一份导致"连接在 main 侧、发射查 tracker 侧"，
	// 所有触点帧被静默丢弃（已修复的隔离测试抓到的 bug）。
	relay := &touchRelay{}
	tracker := newTouchTracker(relay, int32(xr.Minimum), int32(xr.Maximum),
		int32(yr.Minimum), int32(yr.Maximum), *verbose)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("[forwarder] 监听 %s 失败: %v", *listenAddr, err)
	}
	log.Printf("[forwarder] 监听 %s，等待客户端…", *listenAddr)

	// 设备事件读取循环：常驻读取（未连接时只更新状态不转发）
	go func() {
		for {
			ev, err := dev.ReadOne()
			if err != nil {
				log.Printf("[forwarder] 读取失败（设备拔出?）: %v", err)
				return
			}
			tracker.onEvent(*ev)
		}
	}()

	// 接受循环
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			old := relay.replace(conn)
			if old != nil {
				old.Close() // 旧连接的清理 goroutine 会因 clearIf 失效而不 Ungrab
			}
			if err := dev.Grab(); err != nil {
				// 独占失败通常是无权限（部分 ROM 需要写权限）；退化为
				// "只转发不锁屏"——手机自身仍会收到触摸
				log.Printf("[forwarder] Grab 失败（无写权限?），退化为只转发: %v", err)
			} else {
				log.Printf("[forwarder] 客户端 %s 接入 → 已独占触摸屏", conn.RemoteAddr())
			}
			tracker.resendActive() // 已按下的触点对新客户端补发 DOWN
			// 注意：这里**不能**再"清 grab 前积压帧"——读取协程从启动起就
			// 持续消费本 fd，内核队列里不存在积压；若在第二个协程里 ReadOne
			// 会变成两个读者瓜分事件流，接入后的第一帧触摸会被截胡丢弃，
			// 导致"锁屏成功但触摸没输出"。

			go func(c net.Conn) {
				// 读客户端来向数据（当前协议不需要，读到 EOF/错误即断开）
				buf := make([]byte, 256)
				for {
					if _, err := c.Read(buf); err != nil {
						break
					}
				}
				if relay.clearIf(c) {
					dev.Ungrab()
					tracker.dropAll()
					log.Printf("[forwarder] 客户端断开 → 释放触摸屏")
				}
				c.Close()
			}(conn)
		}
	}()

	// Ctrl-C：还原独占状态再退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("[forwarder] 退出")
	dev.Ungrab()
	os.Exit(0)
}

// findTouchscreen 自动扫描多点触摸屏；devPath 非空则直接打开指定节点
func findTouchscreen(devPath string) (*evdev.InputDevice, error) {
	if devPath != "" {
		return openDev(devPath)
	}
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, fmt.Errorf("列举 /dev/input 失败（需要 root 或 input 组）: %w", err)
	}
	var skipped []string
	for _, p := range paths {
		if !strings.HasPrefix(p.Path, "/dev/input/event") {
			continue
		}
		dev, err := openDev(p.Path)
		if err != nil {
			continue
		}
		caps := map[evdev.EvCode]bool{}
		for _, c := range dev.CapableEvents(evdev.EV_ABS) {
			caps[c] = true
		}
		if !caps[evdev.ABS_MT_POSITION_X] || !caps[evdev.ABS_MT_POSITION_Y] ||
			!caps[evdev.ABS_MT_TRACKING_ID] {
			dev.Close()
			continue
		}
		// 按分辨率跳过 Pico 虚拟触屏：固件触屏/触摸板 HID 描述符声明的是
		// 0..32767 全域量程，与本机真实触摸屏的面板分辨率相关量程不同。
		// （抓到虚拟设备会形成 Pico→手机→Pico 的触点回环，必须排除。）
		abs, err := dev.AbsInfos()
		if err != nil {
			dev.Close()
			continue
		}
		xr, okx := abs[evdev.ABS_MT_POSITION_X]
		yr, oky := abs[evdev.ABS_MT_POSITION_Y]
		if okx && oky &&
			xr.Minimum == 0 && xr.Maximum == 32767 &&
			yr.Minimum == 0 && yr.Maximum == 32767 {
			name, _ := dev.Name()
			skipped = append(skipped, fmt.Sprintf("%s(%s, %d×%d)",
				p.Path, name, xr.Maximum, yr.Maximum))
			dev.Close()
			continue
		}
		if len(skipped) > 0 {
			log.Printf("[forwarder] 已跳过 0..32767 全域虚拟设备: %s",
				strings.Join(skipped, ", "))
		}
		return dev, nil
	}
	return nil, fmt.Errorf("未发现多点触摸屏（协议 B）设备，可用 -dev 指定节点；已跳过虚拟设备: %s",
		strings.Join(skipped, ", "))
}

// openDev 先按常规 O_RDWR 打开（Grab 需要写权限），失败退化为只读
func openDev(path string) (*evdev.InputDevice, error) {
	dev, err := evdev.Open(path)
	if err == nil {
		return dev, nil
	}
	return evdev.OpenWithFlags(path, os.O_RDONLY)
}
