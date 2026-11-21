package store

import (
	"context"
	"database/sql"
	"errors"
)

// JobContext 是回复一条消息所需的全部上下文。
type JobContext struct {
	JobID          int64
	MessageID      int64
	BridgeID       int64
	ConversationID int64
	Status         string
	Attempts       int
	LeaseID        string
	ReplyTgID      int64
	ChatID         int64
	ChatType       string
	ChatTitle      string
	TgMessageID    int64
	ReplyToTgID    int64
	Text           string
	ContentKind    string
	ContentHint    string
	SenderName     string
	CreatedAt      int64
}

const jobSelect = `SELECT j.id, j.message_id, j.bridge_id, m.conversation_id, j.status, j.attempts,
       COALESCE(j.lease_id,''), j.reply_tg_id, c.chat_id, c.chat_type, COALESCE(c.title,''),
       COALESCE(m.tg_message_id,0), COALESCE(m.reply_to_tg_id,0), m.text, m.content_kind,
       COALESCE(m.content_hint,''), COALESCE(m.sender_name,''), m.created_at
  FROM jobs j
  JOIN messages m ON m.id=j.message_id
  JOIN conversations c ON c.id=m.conversation_id`

// GetJob 按任务 id 取上下文。
func (s *Store) GetJob(ctx context.Context, jobID int64) (*JobContext, error) {
	var j JobContext
	err := s.db.QueryRowContext(ctx, jobSelect+` WHERE j.id=?`, jobID).Scan(
		&j.JobID, &j.MessageID, &j.BridgeID, &j.ConversationID, &j.Status, &j.Attempts,
		&j.LeaseID, &j.ReplyTgID, &j.ChatID, &j.ChatType, &j.ChatTitle,
		&j.TgMessageID, &j.ReplyToTgID, &j.Text, &j.ContentKind,
		&j.ContentHint, &j.SenderName, &j.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// SetReplyTgID 记住占位消息在 Telegram 里的 id，后续 draft 编辑都复用它。
func (s *Store) SetReplyTgID(ctx context.Context, jobID, tgID, now int64) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE jobs SET reply_tg_id=?, updated_at=? WHERE id=?`, tgID, now, jobID)
}

// CountJobs 返回某座桥某状态的任务数。
func (s *Store) CountJobs(ctx context.Context, bridgeID int64, status string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE bridge_id=? AND status=?`, bridgeID, status).Scan(&n)
	return n, err
}
