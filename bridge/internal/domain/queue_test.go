package domain

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"muse-bridge/internal/store"
)

func newTestQueue(t *testing.T) (*Queue, *store.Store) {
	t.Helper()
	dbPath := filepath.ToSlash(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mustExec(t, st, `INSERT INTO bridges (slug,name,bot_token_enc,webhook_secret,created_at,updated_at)
	                 VALUES ('s1','n1','x','w',0,0)`)
	return NewQueue(st, 180, 5), st
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB().Exec(q, args...); err != nil {
		t.Fatalf("执行 SQL 失败: %v\n%s", err, q)
	}
}

// seedMessage 建一条入站消息和对应的待办，返回 jobID。
func seedMessage(t *testing.T, st *store.Store, bridgeID int64, chatID int64, text string) int64 {
	t.Helper()
	mustExec(t, st, `INSERT INTO conversations (bridge_id,chat_id,chat_type,title,last_message_at,created_at)
	                 VALUES (?,?,'private','',0,0)`, bridgeID, chatID)
	var convID int64
	if err := st.DB().QueryRow(`SELECT id FROM conversations WHERE bridge_id=? AND chat_id=?`,
		bridgeID, chatID).Scan(&convID); err != nil {
		t.Fatalf("取会话 id 失败: %v", err)
	}
	mustExec(t, st, `INSERT INTO messages (bridge_id,conversation_id,direction,tg_message_id,text,content_kind,needs_reply,created_at)
	                 VALUES (?,?,'in',1,?,'text',1,0)`, bridgeID, convID, text)
	var msgID int64
	if err := st.DB().QueryRow(`SELECT id FROM messages WHERE conversation_id=? ORDER BY id DESC LIMIT 1`,
		convID).Scan(&msgID); err != nil {
		t.Fatalf("取消息 id 失败: %v", err)
	}
	mustExec(t, st, `INSERT INTO jobs (message_id,bridge_id,status,created_at,updated_at)
	                 VALUES (?,?,'pending',0,0)`, msgID, bridgeID)
	var jobID int64
	if err := st.DB().QueryRow(`SELECT id FROM jobs WHERE message_id=?`, msgID).Scan(&jobID); err != nil {
		t.Fatalf("取任务 id 失败: %v", err)
	}
	return jobID
}

func TestClaimAndDoubleClaim(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	seedMessage(t, st, 1, 100, "你好")

	got, lease, err := q.Claim(ctx, 1, 5, 180, 1000)
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	if len(got) != 1 || got[0].LeaseID != lease {
		t.Fatalf("应认领 1 条且租约一致，得到 %d 条 lease=%s", len(got), lease)
	}
	if got[0].Text != "你好" || got[0].ChatID != 100 {
		t.Fatalf("认领内容不对: %+v", got[0])
	}

	again, _, err := q.Claim(ctx, 1, 5, 180, 1000)
	if err != nil {
		t.Fatalf("二次认领失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("已认领的任务不应被再认领，得到 %d 条", len(again))
	}
}

// v1 的死锁：过期租约只在 /poll 里回收，而 /poll 只在 pending>0 时才被调用。
// 这里验证 Reap 能把卡住的任务放回队列，并且计入 attempts。
func TestReapReleasesExpiredLease(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	jobID := seedMessage(t, st, 1, 100, "hi")

	if _, _, err := q.Claim(ctx, 1, 5, 180, 1000); err != nil {
		t.Fatalf("认领失败: %v", err)
	}

	// 未过期：不该被回收
	if n, err := q.Reap(ctx, 1050); err != nil || n != 0 {
		t.Fatalf("未过期不应回收，n=%d err=%v", n, err)
	}
	// 过了 180 秒租约
	n, err := q.Reap(ctx, 1200)
	if err != nil || n != 1 {
		t.Fatalf("过期应回收 1 条，n=%d err=%v", n, err)
	}

	var status string
	var attempts int
	if err := st.DB().QueryRow(`SELECT status, attempts FROM jobs WHERE id=?`, jobID).
		Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 1 {
		t.Fatalf("回收后应为 pending/attempts=1，得到 %s/%d", status, attempts)
	}

	// 回收后能被重新认领
	got, _, err := q.Claim(ctx, 1, 5, 180, 1300)
	if err != nil || len(got) != 1 {
		t.Fatalf("回收后应可再认领，n=%d err=%v", len(got), err)
	}
}

func TestReapEventuallyMarksDead(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	jobID := seedMessage(t, st, 1, 100, "hi")

	now := int64(1000)
	for i := 0; i < 5; i++ {
		if _, _, err := q.Claim(ctx, 1, 5, 30, now); err != nil {
			t.Fatalf("第 %d 次认领失败: %v", i, err)
		}
		now += 100
		if _, err := q.Reap(ctx, now); err != nil {
			t.Fatalf("第 %d 次回收失败: %v", i, err)
		}
	}

	var status string
	if err := st.DB().QueryRow(`SELECT status FROM jobs WHERE id=?`, jobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dead" {
		t.Fatalf("反复过期的任务应进死信，得到 %s", status)
	}
}

func TestAckRejectsWrongLease(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	jobID := seedMessage(t, st, 1, 100, "hi")

	_, lease, _ := q.Claim(ctx, 1, 5, 180, 1000)
	if err := q.Ack(ctx, jobID, "wrong-lease", 1001); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("错误租约应被拒绝，得到 %v", err)
	}
	if err := q.Ack(ctx, jobID, lease, 1001); err != nil {
		t.Fatalf("正确租约应确认成功，得到 %v", err)
	}
	// 重复 ack 不再生效
	if err := q.Ack(ctx, jobID, lease, 1002); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("重复确认应失败，得到 %v", err)
	}
}

func TestFailBackoffSequence(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	jobID := seedMessage(t, st, 1, 100, "hi")

	want := []int64{10, 20, 40, 80}
	now := int64(1000)
	for i, w := range want {
		got, lease, err := q.Claim(ctx, 1, 5, 180, now)
		if err != nil || len(got) != 1 {
			t.Fatalf("第 %d 次认领失败: got=%d err=%v", i+1, len(got), err)
		}
		res, err := q.Fail(ctx, jobID, lease, "boom", now)
		if err != nil {
			t.Fatalf("第 %d 次失败记账出错: %v", i+1, err)
		}
		if res.Status != "pending" || res.RetryAfter != w {
			t.Fatalf("第 %d 次退避应为 %ds，得到 %+v", i+1, w, res)
		}
		now += w
	}

	got, lease, err := q.Claim(ctx, 1, 5, 180, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("第 5 次认领失败: got=%d err=%v", len(got), err)
	}
	res, err := q.Fail(ctx, jobID, lease, "boom", now)
	if err != nil {
		t.Fatalf("第 5 次失败记账出错: %v", err)
	}
	if res.Status != "dead" {
		t.Fatalf("第 5 次失败应进死信，得到 %+v", res)
	}
}

func TestPendingCountRespectsBackoff(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	jobID := seedMessage(t, st, 1, 100, "hi")

	if n, _ := q.PendingCount(ctx, 1, 1000); n != 1 {
		t.Fatalf("初始应有 1 条待办，得到 %d", n)
	}
	_, lease, _ := q.Claim(ctx, 1, 5, 180, 1000)
	if _, err := q.Fail(ctx, jobID, lease, "boom", 1000); err != nil {
		t.Fatal(err)
	}
	// 退避 10 秒内不可认领
	if n, _ := q.PendingCount(ctx, 1, 1005); n != 0 {
		t.Fatalf("退避期内不应计入待办，得到 %d", n)
	}
	if n, _ := q.PendingCount(ctx, 1, 1011); n != 1 {
		t.Fatalf("退避结束后应重新计入，得到 %d", n)
	}
}

func TestClaimIsolatedPerBridge(t *testing.T) {
	q, st := newTestQueue(t)
	ctx := context.Background()
	mustExec(t, st, `INSERT INTO bridges (slug,name,bot_token_enc,webhook_secret,created_at,updated_at)
	                 VALUES ('s2','n2','x','w',0,0)`)
	seedMessage(t, st, 1, 100, "bridge1")
	seedMessage(t, st, 2, 200, "bridge2")

	got, _, err := q.Claim(ctx, 2, 5, 180, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChatID != 200 {
		t.Fatalf("桥 2 只能认领自己的任务，得到 %+v", got)
	}
}
