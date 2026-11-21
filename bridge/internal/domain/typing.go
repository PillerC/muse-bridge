package domain

import (
	"context"
	"log"
	"sync"
	"time"
)

// Typing 维护"正在输入…"状态。
//
// 需求是消息一旦被认领就立刻在 Telegram 显示 typing，直到回复完成。
// 这和 SendReply 里那段短命保活不是一回事：那段只覆盖发送消息的几秒，
// 而 agent 真正耗时的是"读完消息到想好怎么答"这段时间，可能几十秒到几分钟。
//
// 四条约束决定了这里的实现：
//   - Telegram 的 typing 状态约 5 秒过期，所以要按 4.5 秒续发；
//   - 同一个任务只能有一个循环，重复 poll 不能叠加请求；
//   - 循环必须随租约结束，否则 agent 崩溃后 Telegram 会一直转圈；
//   - 必须用 Background 上下文。HTTP 请求一结束就取消的话，
//     保活会被立刻带走，等于没做。
type Typing struct {
	mu       sync.Mutex
	entries  map[int64]*typingEntry
	interval time.Duration
	maxFails int
	Enabled  bool
}

// typingEntry 是一个任务对应的保活循环。
//
// deadline 会被 Extend 修改，所以放在锁里读写；循环每轮取一次，
// 到点自己退出，不依赖外部通知。
type typingEntry struct {
	cancel   context.CancelFunc
	chatID   int64
	deadline int64 // unix 秒；0 表示不设上限
}

// NewTyping 构造保活管理器，默认开启。
func NewTyping() *Typing {
	return &Typing{
		entries:  map[int64]*typingEntry{},
		interval: 4500 * time.Millisecond,
		maxFails: 3,
		Enabled:  true,
	}
}

// Start 为一个任务开始续发 typing。
//
// 已经在这个任务上跑着循环时直接返回，避免重复 poll 把请求数翻倍。
// send 由调用方提供，这样这里不必知道桥的 token 和 Telegram 客户端。
func (t *Typing) Start(jobID, chatID, deadline int64, send func(context.Context) error) {
	if t == nil || !t.Enabled || chatID == 0 || send == nil {
		return
	}
	t.mu.Lock()
	if _, running := t.entries[jobID]; running {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &typingEntry{cancel: cancel, chatID: chatID, deadline: deadline}
	t.entries[jobID] = e
	t.mu.Unlock()

	go t.loop(ctx, jobID, e, send)
}

func (t *Typing) loop(ctx context.Context, jobID int64, e *typingEntry, send func(context.Context) error) {
	defer t.forget(jobID, e)

	// 立刻发一次，用户点开对话就能看到"输入中"，不用等第一个 tick。
	if err := send(ctx); err != nil && ctx.Err() != nil {
		return
	}

	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// cancel 之后 ticker 可能已经排了一次，select 会在两个就绪 case 里
			// 随机挑一个，所以这里必须再确认一次，否则停止后还会多发一次。
			if ctx.Err() != nil {
				return
			}
			t.mu.Lock()
			deadline := e.deadline
			t.mu.Unlock()
			// 租约到期就停：此时任务已被回收，再发就是骗用户。
			if deadline > 0 && time.Now().Unix() >= deadline {
				return
			}
			if err := send(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				fails++
				// 连续失败多半是机器人被踢出群或没有发消息权限，
				// 继续打只是浪费配额。这里留一条日志：typing 是纯装饰性请求，
				// 失败时用户只是看不到"输入中"，不记日志就完全无从排查。
				if fails >= t.maxFails {
					log.Printf("typing: job=%d chat=%d 连续 %d 次失败，停止保活: %v", jobID, e.chatID, fails, err)
					return
				}
				continue
			}
			fails = 0
		}
	}
}

// forget 只在 entries 里还是自己时才删，避免误删后来新建的循环。
func (t *Typing) forget(jobID int64, e *typingEntry) {
	t.mu.Lock()
	if cur, ok := t.entries[jobID]; ok && cur == e {
		delete(t.entries, jobID)
	}
	t.mu.Unlock()
}

// Stop 结束某个任务的 typing。重复调用是安全的。
func (t *Typing) Stop(jobID int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	e, ok := t.entries[jobID]
	if ok {
		delete(t.entries, jobID)
	}
	t.mu.Unlock()
	if ok {
		e.cancel()
	}
}

// Extend 把截止时间往后推，供 /extend 续租时调用。
func (t *Typing) Extend(jobID, deadline int64) {
	if t == nil || deadline <= 0 {
		return
	}
	t.mu.Lock()
	if e, ok := t.entries[jobID]; ok {
		e.deadline = deadline
	}
	t.mu.Unlock()
}

// StopAll 停掉全部循环，进程退出时调用。
func (t *Typing) StopAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	all := make([]*typingEntry, 0, len(t.entries))
	for id, e := range t.entries {
		all = append(all, e)
		delete(t.entries, id)
	}
	t.mu.Unlock()
	for _, e := range all {
		e.cancel()
	}
}

// Active 返回当前在续发 typing 的任务数，测试和诊断用。
func (t *Typing) Active() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
