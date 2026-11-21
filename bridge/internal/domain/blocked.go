package domain

import (
	"sync"

	"muse-bridge/internal/store"
	"muse-bridge/internal/tg"
)

// BlockedNotice 记录某座桥最近一次被白名单挡下来的消息。
//
// 只存在内存里，重启即失效，这是刻意的：它唯一的用途是让人发现自己
// 把用户 ID 填错了（常见的是填成机器人的 ID，或者填了另一个账号的），
// 那种情况下后台一片空白，看起来像桥坏了。这些消息本身不该进聊天记录，
// 所以也不落库。
type BlockedNotice struct {
	UserID    int64
	Name      string
	Username  string
	ChatType  string
	ChatTitle string
	At        int64
}

// blockedLog 是每座桥最新一条被拒记录。桥的数量是个位数，
// 用 map + 一把锁足够，不必上 LRU。
type blockedLog struct {
	mu   sync.Mutex
	last map[int64]BlockedNotice
}

func newBlockedLog() *blockedLog {
	return &blockedLog{last: map[int64]BlockedNotice{}}
}

func (l *blockedLog) note(bridgeID int64, n BlockedNotice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 防御：正常不会涨这么多，万一被大量建桥刷过就整体清一次，
	// 丢掉的只是提示信息，不影响任何业务状态。
	if len(l.last) > 1000 {
		l.last = map[int64]BlockedNotice{}
	}
	l.last[bridgeID] = n
}

func (l *blockedLog) get(bridgeID int64) (BlockedNotice, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.last[bridgeID]
	return n, ok
}

func (l *blockedLog) forget(bridgeID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, bridgeID)
}

// noteBlocked 记住一次被拒的发送者，供后台提示。
func (s *Service) noteBlocked(b *store.Bridge, m *tg.Message, now int64) {
	n := BlockedNotice{ChatType: m.Chat.Type, ChatTitle: m.Chat.Title, At: now}
	if m.From != nil {
		n.UserID = m.From.ID
		n.Name = m.From.DisplayName()
		n.Username = m.From.Username
	}
	if n.ChatTitle == "" {
		n.ChatTitle = n.Name
	}
	s.blocked.note(b.ID, n)
}

// BlockedNotice 返回某座桥最近一次被拦下的发送者。
func (s *Service) BlockedNotice(bridgeID int64) (BlockedNotice, bool) {
	return s.blocked.get(bridgeID)
}

// ForgetBlocked 在删除桥时清掉残留，避免 ID 被复用时串到新桥上。
func (s *Service) ForgetBlocked(bridgeID int64) {
	s.blocked.forget(bridgeID)
}
