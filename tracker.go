// 触点跟踪与帧编码：evdev 协议 B（MT slot + tracking id）→ 55 AA 触点帧流。
// 帧格式与固件 handle_control_frame.c 的 PIO_CMD_TOUCH 完全一致：
//
//	[0x55 0xAA][LEN=0x0B][CMD=0xFF][action][id][x:i32 LE][y:i32 LE]
//
// action: DOWN=1 / MOVE=2 / UP=3；id 为外部触点 ID（固件映射到 HID 槽位）；
// x/y 已缩放到固件 TOUCH_MAX（0x7FFFFFFE）。
package main

import (
	"encoding/binary"
	"log"
	"net"
	"sync"

	evdev "github.com/holoplot/go-evdev"
)

const (
	frameLenTouch = 0x0B // CMD(1) + action(1) + id(1) + x(4) + y(4)
	frameSize     = 3 + frameLenTouch

	actionDown = 1
	actionMove = 2
	actionUp   = 3
)

// touchRelay 当前客户端连接（无连接时不转发、不加锁——由 main 的 Grab 决定）
type touchRelay struct {
	mu   sync.Mutex
	conn net.Conn
}

// replace 顶替当前连接，返回旧连接（可为 nil）
func (r *touchRelay) replace(c net.Conn) net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.conn
	r.conn = c
	return old
}

// clearIf 仅当 c 仍是现任连接时清空并返回 true（防旧连接把新连接的 Grab 放掉）
func (r *touchRelay) clearIf(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != c {
		return false
	}
	r.conn = nil
	return true
}

func (r *touchRelay) send(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return
	}
	if _, err := r.conn.Write(b); err != nil {
		log.Printf("[forwarder] 发送失败，断开客户端: %v", err)
		r.conn.Close()
		r.conn = nil
	}
}

func (r *touchRelay) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conn != nil
}

// slotState 一个 MT 槽位的触点状态
type slotState struct {
	active bool
	extID  uint8 // 外部触点 ID（发出去的 id 字段）
	x, y   int32
	isNew  bool // 本帧要发 DOWN
	moved  bool // 本帧要发 MOVE
	dead   bool // tracking id 已回收，本帧要发 UP
}

// touchTracker 协议 B 状态机
type touchTracker struct {
	mu sync.Mutex

	relay *touchRelay

	slots map[int]*slotState
	cur   int   // 当前 MT slot（ABS_MT_SLOT）
	next  uint8 // 下一个外部触点 ID（1..255 循环）

	xMin, xMax, yMin, yMax int32
	verbose                bool
	resend                 bool // 新客户端接入：把在场触点全部补发 DOWN
}

func newTouchTracker(relay *touchRelay, x0, x1, y0, y1 int32, verbose bool) *touchTracker {
	return &touchTracker{
		relay: relay,
		slots: make(map[int]*slotState),
		xMin:  x0, xMax: x1, yMin: y0, yMax: y1,
		verbose: verbose,
	}
}

// resendActive 客户端接入后：下一帧把所有在场触点按 DOWN 补发
func (t *touchTracker) resendActive() {
	t.mu.Lock()
	t.resend = true
	t.mu.Unlock()
}

// dropAll 断开客户端后清空本地状态（物理触点后续的 UP 事件自然消化掉）
func (t *touchTracker) dropAll() {
	t.mu.Lock()
	t.slots = make(map[int]*slotState)
	t.resend = false
	t.mu.Unlock()
}

func (t *touchTracker) onEvent(ev evdev.InputEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch ev.Type {
	case evdev.EV_ABS:
		s := t.slotOf(t.cur)
		switch ev.Code {
		case evdev.ABS_MT_SLOT:
			t.cur = int(ev.Value)
		case evdev.ABS_MT_TRACKING_ID:
			if ev.Value >= 0 {
				// 新触点落槽（重复 DOWN 防御：已 active 就沿用原 ID）
				if !s.active {
					t.next++
					if t.next == 0 {
						t.next = 1
					}
					s.extID = t.next
					s.isNew = true
					s.dead = false
				}
				s.active = true
			} else {
				// 触点抬起
				if s.active {
					s.dead = true
				}
				s.active = false
			}
		case evdev.ABS_MT_POSITION_X:
			s.x = t.scale(ev.Value, t.xMin, t.xMax)
			if s.active && !s.isNew {
				s.moved = true
			}
		case evdev.ABS_MT_POSITION_Y:
			s.y = t.scale(ev.Value, t.yMin, t.yMax)
			if s.active && !s.isNew {
				s.moved = true
			}
		}
	case evdev.EV_SYN:
		if ev.Code == evdev.SYN_REPORT {
			t.flush()
		}
	}
}

// slotOf 取槽位状态（不存在则创建）
func (t *touchTracker) slotOf(idx int) *slotState {
	s, ok := t.slots[idx]
	if !ok {
		s = &slotState{}
		t.slots[idx] = s
	}
	return s
}

// scale 原始 evdev 坐标 → TOUCH_MAX 0..0x7FFFFFFE
func (t *touchTracker) scale(v, mn, mx int32) int32 {
	if mx <= mn {
		return 0
	}
	if v < mn {
		v = mn
	}
	if v > mx {
		v = mx
	}
	return int32((int64(v-mn) * touchCoordMax) / int64(mx-mn))
}

// flush SYN_REPORT：把本帧变化编码成帧流发给客户端（无客户端则只清状态）
func (t *touchTracker) flush() {
	forced := t.resend
	t.resend = false

	// 顺序：UP → DOWN → MOVE（与安卓多点触摸语义一致的拓扑序）
	for idx, s := range t.slots {
		if s.dead {
			t.emit(actionUp, s.extID, s.x, s.y)
			delete(t.slots, idx)
		}
	}
	for _, s := range t.slots {
		if s.active && (s.isNew || forced) {
			s.isNew = false
			s.moved = false
			t.emit(actionDown, s.extID, s.x, s.y)
		}
	}
	for _, s := range t.slots {
		if s.active && s.moved {
			s.moved = false
			t.emit(actionMove, s.extID, s.x, s.y)
		}
	}
}

// emit 编码并发送一帧
func (t *touchTracker) emit(action, id uint8, x, y int32) {
	if !t.relay.active() {
		return
	}
	var f [frameSize]byte
	f[0], f[1], f[2] = 0x55, 0xAA, frameLenTouch
	f[3] = 0xFF // PIO_CMD_TOUCH
	f[4] = action
	f[5] = id
	binary.LittleEndian.PutUint32(f[6:], uint32(x))
	binary.LittleEndian.PutUint32(f[10:], uint32(y))
	if t.verbose {
		log.Printf("[forwarder] action=%d id=%d x=%d y=%d", action, id, x, y)
	}
	t.relay.send(f[:])
}
