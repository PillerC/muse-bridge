// Package poller 为配置成长轮询的桥启动 getUpdates 循环。
//
// 它不是默认路径（默认走 webhook），存在的意义是：没有域名、证书或
// 不想开放 443 的部署，仍能收到消息。每 20 秒对账一次，
// 新建的桥不需要重启进程就会开始拉消息。
package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"muse-bridge/internal/domain"
	"muse-bridge/internal/store"
)

// Manager 管理所有桥的拉取循环。
type Manager struct {
	st      *store.Store
	svc     *domain.Service
	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

// New 构造管理器。
func New(st *store.Store, svc *domain.Service) *Manager {
	return &Manager{st: st, svc: svc, running: map[int64]context.CancelFunc{}}
}

// Run 阻塞运行直到 ctx 结束。
func (m *Manager) Run(ctx context.Context) {
	m.reconcile(ctx)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	bridges, err := m.st.ListBridges(ctx)
	if err != nil {
		log.Printf("poller: 读取桥列表失败: %v", err)
		return
	}
	want := map[int64]bool{}
	for _, b := range bridges {
		if b.Status != "active" || b.Mode != "poll" {
			continue
		}
		want[b.ID] = true
		m.mu.Lock()
		_, ok := m.running[b.ID]
		m.mu.Unlock()
		if ok {
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		m.mu.Lock()
		m.running[b.ID] = cancel
		m.mu.Unlock()
		go m.loop(runCtx, b.ID)
		log.Printf("poller: 桥 %s 开始长轮询", b.Slug)
	}
	m.mu.Lock()
	for id, cancel := range m.running {
		if !want[id] {
			cancel()
			delete(m.running, id)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.running {
		cancel()
		delete(m.running, id)
	}
}

func (m *Manager) loop(ctx context.Context, bridgeID int64) {
	defer func() {
		m.mu.Lock()
		delete(m.running, bridgeID)
		m.mu.Unlock()
	}()

	backoff := 3 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		b, err := m.st.GetBridgeByID(ctx, bridgeID)
		if err != nil {
			return
		}
		if b.Status != "active" || b.Mode != "poll" {
			return
		}
		client, err := m.svc.Client(ctx, b)
		if err != nil {
			log.Printf("poller: 桥 %s 无法构造客户端: %v", b.Slug, err)
			return
		}
		ups, err := client.GetUpdates(ctx, b.LastUpdateID+1, 25)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("poller: 桥 %s getUpdates 失败: %v", b.Slug, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 3 * time.Second

		for i := range ups {
			up := &ups[i]
			if _, err := m.svc.IngestUpdate(ctx, b, up, m.svc.Now()); err != nil {
				log.Printf("poller: 桥 %s 入库失败 update=%d: %v", b.Slug, up.UpdateID, err)
				continue
			}
			if up.UpdateID > b.LastUpdateID {
				b.LastUpdateID = up.UpdateID
				_ = m.st.SetLastUpdateID(ctx, b.ID, up.UpdateID)
			}
		}
	}
}
