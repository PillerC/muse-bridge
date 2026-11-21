package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"muse-bridge/internal/store"
)

// ErrLeaseMismatch 表示租约不存在、已过期，或这条任务已被别人认领。
var ErrLeaseMismatch = errors.New("lease mismatch")

// ClaimedMessage 是认领成功的一条待办，附带回复所需的全部上下文。
type ClaimedMessage struct {
	JobID        int64  `json:"job_id"`
	MessageID    int64  `json:"message_id"`
	LeaseID      string `json:"lease_id"`
	ChatID       int64  `json:"chat_id"`
	ChatType     string `json:"chat_type"`
	ChatTitle    string `json:"chat_title"`
	TgMessageID  int64  `json:"tg_message_id"`
	FromID       int64  `json:"from_id"`
	FromUsername string `json:"from_username"`
	FromName     string `json:"from_name"`
	Text         string `json:"text"`
	ContentKind  string `json:"content_kind"`
	ContentHint  string `json:"content_hint"`
	ReplyToTgID  int64  `json:"reply_to_tg_id"`
	IsMention    bool   `json:"is_mention"`
	IsReplyToBot bool   `json:"is_reply_to_bot"`
	Attempts     int    `json:"attempts"`
	CreatedAt    int64  `json:"created_at"`
}

// Queue 是任务队列的状态机。
//
// 全部租约逻辑只在这一个文件里实现，Webhook、长轮询、Muse API 和后台巡检
// 都调用同一份 Reap——v1 只有 /poll 会回收过期租约，而 /poll 又只在
// pending-count>0 时被调用，于是 agent 崩溃后消息永久卡在 processing。
type Queue struct {
	st          *store.Store
	leaseSecs   int
	maxAttempts int
}

// NewQueue 构造队列。leaseSecs 与 maxAttempts 来自配置。
func NewQueue(st *store.Store, leaseSecs, maxAttempts int) *Queue {
	if leaseSecs <= 0 {
		leaseSecs = 180
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &Queue{st: st, leaseSecs: leaseSecs, maxAttempts: maxAttempts}
}

// DefaultLeaseSeconds 返回配置的默认租约时长。
func (q *Queue) DefaultLeaseSeconds() int { return q.leaseSecs }

// Reap 回收已过期的租约。
//
// 回收会把 attempts 加一，所以一条能让 agent 反复崩溃的消息最终会进死信，
// 而不是无限重跑。这件事必须由所有入口共同触发，否则又会退回到 v1 的死锁。
func (q *Queue) Reap(ctx context.Context, now int64) (int, error) {
	var released int
	err := q.st.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET status='pending', lease_id=NULL, claimed_at=NULL,
			        attempts=attempts+1, updated_at=?,
			        last_error=COALESCE(last_error, 'lease expired')
			  WHERE status='processing'
			    AND claimed_at IS NOT NULL
			    AND claimed_at + lease_seconds < ?`, now, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		released = int(n)

		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs
			    SET status='dead', lease_id=NULL, claimed_at=NULL, updated_at=?,
			        last_error=COALESCE(last_error,'lease expired too many times')
			  WHERE status='pending' AND attempts >= ?`, now, q.maxAttempts); err != nil {
			return err
		}
		return nil
	})
	return released, err
}

// PendingCount 返回某座桥当前可认领的任务数。
func (q *Queue) PendingCount(ctx context.Context, bridgeID, now int64) (int, error) {
	var n int
	err := q.st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE bridge_id=? AND status='pending' AND next_retry_at<=?`,
		bridgeID, now).Scan(&n)
	return n, err
}

// Claim 认领某座桥最早的一批待办，返回带租约的消息列表。
// 返回空列表表示没有可做的工作，不是错误。
func (q *Queue) Claim(ctx context.Context, bridgeID int64, limit, leaseSeconds, now int64) ([]ClaimedMessage, string, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	if leaseSeconds <= 0 {
		leaseSeconds = int64(q.leaseSecs)
	}
	if leaseSeconds < 30 {
		leaseSeconds = 30
	}
	if leaseSeconds > 900 {
		leaseSeconds = 900
	}
	leaseID := newLeaseID()

	var out []ClaimedMessage
	err := q.st.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM jobs
			  WHERE bridge_id=? AND status='pending' AND next_retry_at<=?
			  ORDER BY id LIMIT ?`, bridgeID, now, limit)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}

		// 逐条认领：带 status='pending' 守卫，避免与并发认领重复。
		for _, id := range ids {
			res, err := tx.ExecContext(ctx,
				`UPDATE jobs SET status='processing', lease_id=?, claimed_at=?,
				        lease_seconds=?, updated_at=?
				  WHERE id=? AND status='pending'`, leaseID, now, leaseSeconds, now, id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			cm, err := loadClaimed(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, cm)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, leaseID, nil
}

func loadClaimed(ctx context.Context, tx *sql.Tx, jobID int64) (ClaimedMessage, error) {
	var cm ClaimedMessage
	err := tx.QueryRowContext(ctx,
		`SELECT j.id, j.message_id, j.attempts, j.lease_id,
		        m.tg_message_id, COALESCE(m.sender_id,0), COALESCE(m.sender_name,''),
		        m.text, m.content_kind, COALESCE(m.content_hint,''),
		        COALESCE(m.reply_to_tg_id,0), m.created_at, m.is_mention, m.is_reply_to_bot,
		        c.chat_id, c.chat_type, COALESCE(c.title,''), COALESCE(m.sender_username,'')
		   FROM jobs j
		   JOIN messages m ON m.id = j.message_id
		   JOIN conversations c ON c.id = m.conversation_id
		  WHERE j.id=?`, jobID).Scan(
		&cm.JobID, &cm.MessageID, &cm.Attempts, &cm.LeaseID,
		&cm.TgMessageID, &cm.FromID, &cm.FromName,
		&cm.Text, &cm.ContentKind, &cm.ContentHint,
		&cm.ReplyToTgID, &cm.CreatedAt, &cm.IsMention, &cm.IsReplyToBot,
		&cm.ChatID, &cm.ChatType, &cm.ChatTitle, &cm.FromUsername)
	return cm, err
}

// Extend 续租。返回 ErrLeaseMismatch 表示租约已失效。
func (q *Queue) Extend(ctx context.Context, jobID int64, leaseID string, leaseSeconds, now int64) error {
	if leaseSeconds < 30 {
		leaseSeconds = 30
	}
	if leaseSeconds > 900 {
		leaseSeconds = 900
	}
	res, err := q.st.DB().ExecContext(ctx,
		`UPDATE jobs SET claimed_at=?, lease_seconds=?, updated_at=?
		  WHERE id=? AND lease_id=? AND status='processing'`,
		now, leaseSeconds, now, jobID, leaseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseMismatch
	}
	return nil
}

// Ack 标记完成。
func (q *Queue) Ack(ctx context.Context, jobID int64, leaseID string, now int64) error {
	res, err := q.st.DB().ExecContext(ctx,
		`UPDATE jobs SET status='done', lease_id=NULL, claimed_at=NULL, updated_at=?
		  WHERE id=? AND lease_id=? AND status='processing'`, now, jobID, leaseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseMismatch
	}
	return nil
}

// FailResult 描述一次失败的处置结果。
type FailResult struct {
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	RetryAfter int64  `json:"retry_in_sec"`
}

// Fail 记录失败：指数退避重试，超过上限进死信。
func (q *Queue) Fail(ctx context.Context, jobID int64, leaseID, reason string, now int64) (FailResult, error) {
	var out FailResult
	err := q.st.Tx(ctx, func(tx *sql.Tx) error {
		var attempts int
		err := tx.QueryRowContext(ctx,
			`SELECT attempts FROM jobs WHERE id=? AND lease_id=? AND status='processing'`,
			jobID, leaseID).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseMismatch
		}
		if err != nil {
			return err
		}
		attempts++
		if len(reason) > 500 {
			reason = reason[:500]
		}
		if attempts >= q.maxAttempts {
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET status='dead', attempts=?, last_error=?,
				        lease_id=NULL, claimed_at=NULL, updated_at=? WHERE id=?`,
				attempts, reason, now, jobID); err != nil {
				return err
			}
			out = FailResult{Status: "dead", Attempts: attempts}
			return nil
		}
		backoff := int64(10) << (attempts - 1)
		if backoff > 600 {
			backoff = 600
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET status='pending', attempts=?, last_error=?,
			        next_retry_at=?, lease_id=NULL, claimed_at=NULL, updated_at=?
			  WHERE id=?`, attempts, reason, now+backoff, now, jobID); err != nil {
			return err
		}
		out = FailResult{Status: "pending", Attempts: attempts, RetryAfter: backoff}
		return nil
	})
	return out, err
}

// Replay 把死信放回队列。
func (q *Queue) Replay(ctx context.Context, jobID, now int64) error {
	res, err := q.st.DB().ExecContext(ctx,
		`UPDATE jobs SET status='pending', attempts=0, next_retry_at=0,
		        last_error=NULL, updated_at=? WHERE id=? AND status='dead'`, now, jobID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("任务 %d 不在死信状态", jobID)
	}
	return nil
}

func newLeaseID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano()) + randomHex(16)
}
