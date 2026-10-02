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

	// 自动发现所有协议 B 多点触摸屏，并按坐标范围排除 Pico/虚拟触屏。
	devices, err := findTouchscreens(*devPath)
	if err != nil {
		log.Fatalf("[forwarder] %v", err)
	}
	for _, d := range devices {
		log.Printf("[forwarder] 候选触摸屏: %s (%s), X=[%d..%d] Y=[%d..%d]",
			d.path, d.name, d.xr.Minimum, d.xr.Maximum, d.yr.Minimum, d.yr.Maximum)
	}

	// 客户端管理：一个时刻只服务一个客户端，后来者顶掉前者。
	relay := &touchRelay{}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("[forwarder] 监听 %s 失败: %v", *listenAddr, err)
	}
	log.Printf("[forwarder] 监听 %s，等待客户端…", *listenAddr)

	// 等待第一个真实触摸屏发出完整 SYN_REPORT。其他设备不会参与事件转发，
	// 但启动阶段会同时读取它们，避免按 eventN 排序误选开发板或虚拟触屏。
	type selectionState struct {
		device  *touchscreenCandidate
		tracker *touchTracker
		err     error
	}
	selectionReady := make(chan struct{})
	var selection selectionState
	go func() {
		device, initial, selectErr := waitForFirstReport(devices)
		if selectErr != nil {
			selection.err = selectErr
			close(selectionReady)
			return
		}
		selection.device = device
		selection.tracker = newTouchTracker(relay,
			int32(device.xr.Minimum), int32(device.xr.Maximum),
			int32(device.yr.Minimum), int32(device.yr.Maximum), *verbose)
		for _, ev := range initial {
			selection.tracker.onEvent(ev)
		}
		log.Printf("[forwarder] 使用第一个收到报告的触摸屏: %s (%s)",
			device.path, device.name)
		log.Printf("[forwarder] 量程 X=[%d..%d] Y=[%d..%d] → 0..%d",
			device.xr.Minimum, device.xr.Maximum,
			device.yr.Minimum, device.yr.Maximum, touchCoordMax)
		close(selectionReady)

		// 选定设备后持续读取；其他设备已在 waitForFirstReport 中关闭。
		for {
			ev, readErr := device.dev.ReadOne()
			if readErr != nil {
				log.Printf("[forwarder] 读取失败（设备拔出?）: %v", readErr)
				return
			}
			selection.tracker.onEvent(*ev)
		}
	}()

	// 接受循环
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			<-selectionReady
			if selection.err != nil {
				log.Printf("[forwarder] 尚未选定触摸屏，关闭客户端: %v", selection.err)
				_ = conn.Close()
				continue
			}
			dev := selection.device.dev
			tracker := selection.tracker
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
	if selection.device != nil {
		selection.device.dev.Ungrab()
	}
	for _, d := range devices {
		_ = d.dev.Close()
	}
	os.Exit(0)
}

// touchscreenCandidate 是通过分辨率筛选后的协议 B 触摸设备。
type touchscreenCandidate struct {
	dev    *evdev.InputDevice
	path   string
	name   string
	xr, yr evdev.AbsInfo
}

// findTouchscreens 收集全部协议 B 触摸设备，再按坐标量程过滤虚拟触屏。
func findTouchscreens(devPath string) ([]*touchscreenCandidate, error) {
	if devPath != "" {
		dev, err := openDev(devPath)
		if err != nil {
			return nil, err
		}
		abs, err := dev.AbsInfos()
		if err != nil {
			_ = dev.Close()
			return nil, err
		}
		xr, okx := abs[evdev.ABS_MT_POSITION_X]
		yr, oky := abs[evdev.ABS_MT_POSITION_Y]
		if !okx || !oky {
			_ = dev.Close()
			return nil, fmt.Errorf("设备缺少 ABS_MT_POSITION_X/Y 量程: %s", devPath)
		}
		name, _ := dev.Name()
		return []*touchscreenCandidate{{dev: dev, path: devPath, name: name, xr: xr, yr: yr}}, nil
	}
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		return nil, fmt.Errorf("列举 /dev/input 失败: %w", err)
	}

	var candidates []*touchscreenCandidate
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
			_ = dev.Close()
			continue
		}
		abs, err := dev.AbsInfos()
		if err != nil {
			_ = dev.Close()
			continue
		}
		xr, okx := abs[evdev.ABS_MT_POSITION_X]
		yr, oky := abs[evdev.ABS_MT_POSITION_Y]
		if !okx || !oky {
			_ = dev.Close()
			continue
		}
		name, _ := dev.Name()
		candidates = append(candidates, &touchscreenCandidate{
			dev: dev, path: p.Path, name: name, xr: xr, yr: yr,
		})
	}

	var physical []*touchscreenCandidate
	var skipped []string
	for _, c := range candidates {
		log.Printf("[forwarder] 发现触摸设备: %s (%s), X=[%d..%d] Y=[%d..%d]",
			c.path, c.name, c.xr.Minimum, c.xr.Maximum, c.yr.Minimum, c.yr.Maximum)
		// Pico 的虚拟触屏使用 0..0x7FFFFFFE 坐标空间；真实面板通常
		// 使用实际分辨率，因此将这种设备从自动发现结果中排除。
		if c.xr.Minimum == 0 && c.yr.Minimum == 0 &&
			c.xr.Maximum == touchCoordMax && c.yr.Maximum == touchCoordMax {
			skipped = append(skipped, fmt.Sprintf("%s(%s, 0..0x%X)",
				c.path, c.name, touchCoordMax))
			_ = c.dev.Close()
			continue
		}
		physical = append(physical, c)
	}

	if len(physical) == 0 {
		return nil, fmt.Errorf("未发现可用的真实多点触摸屏；已跳过虚拟设备: %s",
			strings.Join(skipped, ", "))
	}
	if len(skipped) > 0 {
		log.Printf("[forwarder] 已按坐标范围跳过虚拟触屏: %s", strings.Join(skipped, ", "))
	}

	return physical, nil
}

type reportResult struct {
	device *touchscreenCandidate
	events []evdev.InputEvent
	err    error
}

// waitForFirstReport 并行读取所有候选设备，返回第一个完成 SYN_REPORT 的设备。
func waitForFirstReport(devices []*touchscreenCandidate) (*touchscreenCandidate, []evdev.InputEvent, error) {
	results := make(chan reportResult, len(devices))
	for _, device := range devices {
		go func(device *touchscreenCandidate) {
			var events []evdev.InputEvent
			for {
				ev, err := device.dev.ReadOne()
				if err != nil {
					results <- reportResult{device: device, err: err}
					return
				}
				events = append(events, *ev)
				if ev.Type == evdev.EV_SYN && ev.Code == evdev.SYN_REPORT {
					results <- reportResult{device: device, events: events}
					return
				}
			}
		}(device)
	}

	var lastErr error
	for remaining := len(devices); remaining > 0; remaining-- {
		result := <-results
		if result.err == nil {
			for _, device := range devices {
				if device != result.device {
					_ = device.dev.Close()
				}
			}
			return result.device, result.events, nil
		}
		lastErr = result.err
		log.Printf("[forwarder] 候选触摸屏 %s 读取失败: %v", result.device.path, result.err)
	}
	return nil, nil, fmt.Errorf("所有候选触摸屏都无法读取: %w", lastErr)
}

// openDev 先按常规 O_RDWR 打开（Grab 需要写权限），失败退化为只读
func openDev(path string) (*evdev.InputDevice, error) {
	dev, err := evdev.Open(path)
	if err == nil {
		return dev, nil
	}
	return evdev.OpenWithFlags(path, os.O_RDONLY)
}
