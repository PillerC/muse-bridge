package domain

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// 认领后必须立刻发一次 typing，不能等第一个 tick。
func TestTypingSendsImmediately(t *testing.T) {
	ty := NewTyping()
	ty.interval = time.Hour // 关掉续发，只看首帧
	var n atomic.Int64
	ty.Start(1, 100, 0, func(context.Context) error { n.Add(1); return nil })
	defer ty.Stop(1)

	if !waitFor(func() bool { return n.Load() == 1 }, 2*time.Second) {
		t.Fatalf("应立即发送一次 typing，实际 %d 次", n.Load())
	}
	if ty.Active() != 1 {
		t.Fatalf("应记录 1 个活跃任务，实际 %d", ty.Active())
	}
}

// 停止之后不能再发：回复已经结束还转圈是在骗用户。
func TestTypingStopsOnStop(t *testing.T) {
	ty := NewTyping()
	ty.interval = 20 * time.Millisecond
	var n atomic.Int64
	ty.Start(7, 100, 0, func(context.Context) error { n.Add(1); return nil })

	if !waitFor(func() bool { return n.Load() >= 3 }, 2*time.Second) {
		t.Fatalf("应持续续发，实际只发了 %d 次", n.Load())
	}
	ty.Stop(7)
	time.Sleep(50 * time.Millisecond)
	after := n.Load()
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != after {
		t.Fatalf("停止后不应再发，停止后多发了 %d 次", got-after)
	}
	if ty.Active() != 0 {
		t.Fatalf("停止后活跃数应为 0，实际 %d", ty.Active())
	}
}

// 重复认领同一任务不能叠加循环，否则请求数翻倍且 Stop 只能停掉其中一个。
func TestTypingDeduplicates(t *testing.T) {
	ty := NewTyping()
	ty.interval = time.Hour
	var n atomic.Int64
	send := func(context.Context) error { n.Add(1); return nil }
	ty.Start(3, 100, 0, send)
	ty.Start(3, 100, 0, send)
	ty.Start(3, 100, 0, send)
	defer ty.Stop(3)

	if !waitFor(func() bool { return n.Load() >= 1 }, 2*time.Second) {
		t.Fatal("应至少发送一次")
	}
	time.Sleep(120 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("三次认领应只发一次，实际 %d 次", got)
	}
	if ty.Active() != 1 {
		t.Fatalf("活跃数应为 1，实际 %d", ty.Active())
	}
}

// 租约到期必须自动停：agent 崩溃时没人会调 Stop，
// 否则用户会一直看到"正在输入"直到重启。
func TestTypingStopsAtDeadline(t *testing.T) {
	ty := NewTyping()
	ty.interval = 20 * time.Millisecond
	deadline := time.Now().Add(300 * time.Millisecond).Unix()
	// 用 Unix 秒会向下取整，可能落在过去，这里补一秒保证能跑起来。
	deadline = time.Now().Add(time.Second).Unix()

	var n atomic.Int64
	ty.Start(9, 100, deadline, func(context.Context) error { n.Add(1); return nil })

	if !waitFor(func() bool { return ty.Active() == 0 }, 4*time.Second) {
		t.Fatal("到达租约截止时间后应自动停止")
	}
	// 截止后不应再增长。
	after := n.Load()
	time.Sleep(100 * time.Millisecond)
	if got := n.Load(); got != after {
		t.Fatalf("到期后仍在续发，多发了 %d 次", got-after)
	}
}

// 续租要把截止时间往后推，否则续了租但提示先消失。
func TestTypingExtend(t *testing.T) {
	ty := NewTyping()
	ty.interval = 20 * time.Millisecond
	var n atomic.Int64
	ty.Start(11, 100, time.Now().Add(300*time.Millisecond).Unix(),
		func(context.Context) error { n.Add(1); return nil })
	defer ty.Stop(11)

	ty.Extend(11, time.Now().Add(2*time.Second).Unix())
	if !waitFor(func() bool { return n.Load() > 5 }, 3*time.Second) {
		t.Fatalf("续租后应继续续发，实际只发了 %d 次", n.Load())
	}
	if ty.Active() != 1 {
		t.Fatalf("续租后仍应活跃，实际 %d", ty.Active())
	}
}

// 机器人被踢出群时 sendChatAction 会持续失败，打几次就该放弃。
func TestTypingStopsAfterFailures(t *testing.T) {
	ty := NewTyping()
	ty.interval = 10 * time.Millisecond
	var n atomic.Int64
	ty.Start(13, 100, 0, func(context.Context) error {
		n.Add(1)
		return errors.New("Forbidden: bot was kicked")
	})

	if !waitFor(func() bool { return ty.Active() == 0 }, 2*time.Second) {
		t.Fatal("连续失败后应停止")
	}
	if got := n.Load(); got > 5 {
		t.Fatalf("失败后应尽快放弃，实际重试了 %d 次", got)
	}
}

// StopAll 用于进程退出，之后所有循环都不应再发。
func TestTypingStopAll(t *testing.T) {
	ty := NewTyping()
	ty.interval = 20 * time.Millisecond
	var n atomic.Int64
	send := func(context.Context) error { n.Add(1); return nil }
	ty.Start(21, 100, 0, send)
	ty.Start(22, 200, 0, send)

	if !waitFor(func() bool { return ty.Active() == 2 }, 2*time.Second) {
		t.Fatalf("应有 2 个活跃任务，实际 %d", ty.Active())
	}
	ty.StopAll()
	if ty.Active() != 0 {
		t.Fatalf("StopAll 后应为 0，实际 %d", ty.Active())
	}
	// 已经发出去的那一次请求无法撤回，给它一个 tick 的时间落地。
	time.Sleep(60 * time.Millisecond)
	after := n.Load()
	time.Sleep(100 * time.Millisecond)
	if got := n.Load(); got != after {
		t.Fatalf("StopAll 后不应再发，多发了 %d 次", got-after)
	}
}

// 关掉开关时完全不发，给"嫌吵"的场景留退路。
func TestTypingDisabled(t *testing.T) {
	ty := NewTyping()
	ty.Enabled = false
	var n atomic.Int64
	ty.Start(31, 100, 0, func(context.Context) error { n.Add(1); return nil })
	time.Sleep(80 * time.Millisecond)
	if n.Load() != 0 || ty.Active() != 0 {
		t.Fatalf("关闭时不应发送，实际 %d 次 / 活跃 %d", n.Load(), ty.Active())
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
