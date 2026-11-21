package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Conversation 是一段对话（私聊或群）。
type Conversation struct {
	ID            int64
	BridgeID      int64
	ChatID        int64
	ChatType      string
	Title         string
	LastMessageAt int64
	CreatedAt     int64
	MessageCount  int
	PendingCount  int
}

// Inbound 是一条待入库的入站消息。
type Inbound struct {
	BridgeID       int64
	ChatID         int64
	ChatType       string
	ChatTitle      string
	Direction      string // 固定 in
	TgMessageID    int64
	ReplyToTgID    int64
	SenderID       int64
	SenderName     string
	SenderUsername string
	Text           string
	ContentKind    string
	ContentHint    string
	MediaJSON      string
	UpdateID       int64
	IsMention      bool
	IsReplyToBot   bool
	NeedsReply     bool
	ReplyReason    string
	RawJSON        string
	CreatedAt      int64
}

// IngestInbound 在一个事务里完成三件事：定位/创建会话、写入消息、按需建任务。
//
// 幂等：同一座桥的同一个 update_id 只会入队一次，靠部分唯一索引兜底，
// 而不是 v1 的 INSERT OR IGNORE——后者会把 NOT NULL 失败也一并吞掉，
// 表现为"消息被当成重复而静默丢失"。
func (s *Store) IngestInbound(ctx context.Context, in *Inbound, now int64) (messageID int64, created bool, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var convID int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO conversations (bridge_id, chat_id, chat_type, title, last_message_at, created_at)
			 VALUES (?,?,?,?,?,?)
			 ON CONFLICT(bridge_id, chat_id) DO UPDATE SET
			   last_message_at = excluded.last_message_at,
			   title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE conversations.title END,
			   chat_type = excluded.chat_type
			 RETURNING id`,
			in.BridgeID, in.ChatID, in.ChatType, in.ChatTitle, in.CreatedAt, now).Scan(&convID)
		if err != nil {
			return err
		}

		err = tx.QueryRowContext(ctx,
			`INSERT INTO messages (bridge_id, conversation_id, direction, tg_message_id, reply_to_tg_id,
			                       sender_id, sender_name, sender_username, text, content_kind, content_hint,
			                       media_json, update_id, is_mention, is_reply_to_bot, needs_reply,
			                       reply_reason, raw_json, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			 RETURNING id`,
			in.BridgeID, convID, "in", in.TgMessageID, nullInt(in.ReplyToTgID),
			nullInt(in.SenderID), in.SenderName, in.SenderUsername, in.Text, in.ContentKind, in.ContentHint,
			nullStr(in.MediaJSON), nullInt(in.UpdateID), boolInt(in.IsMention), boolInt(in.IsReplyToBot),
			boolInt(in.NeedsReply), in.ReplyReason, nullStr(in.RawJSON), in.CreatedAt).Scan(&messageID)
		if err != nil {
			if isUniqueViolation(err) {
				// 重复投递：返回已存在的那条，调用方按"未新建"处理。
				created = false
				return tx.QueryRowContext(ctx,
					`SELECT id FROM messages WHERE bridge_id=? AND update_id=?`,
					in.BridgeID, in.UpdateID).Scan(&messageID)
			}
			return err
		}
		created = true

		if in.NeedsReply {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO jobs (message_id, bridge_id, status, created_at, updated_at)
				 VALUES (?,?,'pending',?,?)
				 ON CONFLICT(message_id) DO NOTHING`,
				messageID, in.BridgeID, now, now); err != nil {
				return err
			}
		}
		return nil
	})
	return messageID, created, err
}

// InsertOutbound 记录一条桥发出的消息。
func (s *Store) InsertOutbound(ctx context.Context, bridgeID, convID, tgMessageID, replyTo int64,
	text, kind, partsJSON string, idempotencyKey string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (bridge_id, conversation_id, direction, tg_message_id, reply_to_tg_id,
		                       sender_name, text, content_kind, tg_parts, idempotency_key, created_at)
		 VALUES (?,?,'out',?,?,?,?,?,?,?,?)`,
		bridgeID, convID, nullInt(tgMessageID), nullInt(replyTo),
		"bot", text, kind, nullStr(partsJSON), nullStr(idempotencyKey), now)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE conversations SET last_message_at=? WHERE id=?`, now, convID)
	return res.LastInsertId()
}

// ConversationByChat 找某座桥下某个 chat 的会话。
func (s *Store) ConversationByChat(ctx context.Context, bridgeID, chatID int64) (*Conversation, error) {
	var c Conversation
	err := s.db.QueryRowContext(ctx,
		`SELECT id, bridge_id, chat_id, chat_type, COALESCE(title,''), last_message_at, created_at
		   FROM conversations WHERE bridge_id=? AND chat_id=?`, bridgeID, chatID).Scan(
		&c.ID, &c.BridgeID, &c.ChatID, &c.ChatType, &c.Title, &c.LastMessageAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListConversations 列出某座桥（bridgeID=0 表示全部）的会话，最近活跃在前。
func (s *Store) ListConversations(ctx context.Context, bridgeID int64, limit int) ([]*Conversation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT c.id, c.bridge_id, c.chat_id, c.chat_type, COALESCE(c.title,''),
	             c.last_message_at, c.created_at,
	             (SELECT COUNT(*) FROM messages m WHERE m.conversation_id=c.id) AS cnt,
	             (SELECT COUNT(*) FROM jobs j JOIN messages m2 ON m2.id=j.message_id
	               WHERE m2.conversation_id=c.id AND j.status IN ('pending','processing')) AS pend
	        FROM conversations c`
	args := []any{}
	if bridgeID > 0 {
		q += ` WHERE c.bridge_id=?`
		args = append(args, bridgeID)
	}
	q += ` ORDER BY c.last_message_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.BridgeID, &c.ChatID, &c.ChatType, &c.Title,
			&c.LastMessageAt, &c.CreatedAt, &c.MessageCount, &c.PendingCount); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetConversation 按 id 取会话。
func (s *Store) GetConversation(ctx context.Context, id int64) (*Conversation, error) {
	var c Conversation
	err := s.db.QueryRowContext(ctx,
		`SELECT id, bridge_id, chat_id, chat_type, COALESCE(title,''), last_message_at, created_at
		   FROM conversations WHERE id=?`, id).Scan(
		&c.ID, &c.BridgeID, &c.ChatID, &c.ChatType, &c.Title, &c.LastMessageAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MessageRow 是后台聊天记录里的一行。
type MessageRow struct {
	ID             int64
	Direction      string
	TgMessageID    int64
	ReplyToTgID    int64
	SenderName     string
	SenderUsername string
	Text           string
	ContentKind    string
	ContentHint    string
	CreatedAt      int64
	JobStatus      string
	JobAttempts    int
	LastError      string
}

// ListMessages 按会话翻页，beforeID=0 表示最新一页。
func (s *Store) ListMessages(ctx context.Context, convID, beforeID int64, limit int) ([]*MessageRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT m.id, m.direction, COALESCE(m.tg_message_id,0), COALESCE(m.reply_to_tg_id,0),
	             COALESCE(m.sender_name,''), COALESCE(m.sender_username,''), m.text,
	             m.content_kind, COALESCE(m.content_hint,''), m.created_at,
	             COALESCE(j.status,''), COALESCE(j.attempts,0), COALESCE(j.last_error,'')
	        FROM messages m LEFT JOIN jobs j ON j.message_id=m.id
	       WHERE m.conversation_id=?`
	args := []any{convID}
	if beforeID > 0 {
		q += ` AND m.id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MessageRow
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.ID, &m.Direction, &m.TgMessageID, &m.ReplyToTgID,
			&m.SenderName, &m.SenderUsername, &m.Text, &m.ContentKind, &m.ContentHint,
			&m.CreatedAt, &m.JobStatus, &m.JobAttempts, &m.LastError); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// StatusCounts 返回某座桥的任务状态分布。
func (s *Store) StatusCounts(ctx context.Context, bridgeID int64) (map[string]int, error) {
	q := `SELECT status, COUNT(*) FROM jobs`
	args := []any{}
	if bridgeID > 0 {
		q += ` WHERE bridge_id=?`
		args = append(args, bridgeID)
	}
	q += ` GROUP BY status`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// DeadJobs 返回最近的死信，供后台重放。
type DeadJob struct {
	JobID     int64
	MessageID int64
	BridgeID  int64
	ChatID    int64
	ChatTitle string
	Text      string
	Attempts  int
	LastError string
	UpdatedAt int64
}

// ListDeadJobs 列出死信。
func (s *Store) ListDeadJobs(ctx context.Context, bridgeID int64, limit int) ([]*DeadJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT j.id, j.message_id, j.bridge_id, c.chat_id, COALESCE(c.title,''),
	             m.text, j.attempts, COALESCE(j.last_error,''), j.updated_at
	        FROM jobs j
	        JOIN messages m ON m.id=j.message_id
	        JOIN conversations c ON c.id=m.conversation_id
	       WHERE j.status='dead'`
	args := []any{}
	if bridgeID > 0 {
		q += ` AND j.bridge_id=?`
		args = append(args, bridgeID)
	}
	q += ` ORDER BY j.updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeadJob
	for rows.Next() {
		var d DeadJob
		if err := rows.Scan(&d.JobID, &d.MessageID, &d.BridgeID, &d.ChatID, &d.ChatTitle,
			&d.Text, &d.Attempts, &d.LastError, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ExportItem 是导出用的扁平行。
type ExportItem struct {
	CreatedAt      int64
	Direction      string
	BridgeName     string
	BridgeSlug     string
	ChatID         int64
	ChatType       string
	ChatTitle      string
	SenderName     string
	SenderUsername string
	Text           string
	ContentKind    string
	TgMessageID    int64
}

// ExportMessages 导出聊天记录。bridgeID 或 convID 为 0 表示不按它过滤。
func (s *Store) ExportMessages(ctx context.Context, bridgeID, convID, from, to int64) ([]*ExportItem, error) {
	q := `SELECT m.created_at, m.direction, b.name, b.slug, c.chat_id, c.chat_type,
	             COALESCE(c.title,''), COALESCE(m.sender_name,''), COALESCE(m.sender_username,''),
	             m.text, m.content_kind, COALESCE(m.tg_message_id,0)
	        FROM messages m
	        JOIN conversations c ON c.id=m.conversation_id
	        JOIN bridges b ON b.id=m.bridge_id
	       WHERE 1=1`
	args := []any{}
	if bridgeID > 0 {
		q += ` AND m.bridge_id=?`
		args = append(args, bridgeID)
	}
	if convID > 0 {
		q += ` AND m.conversation_id=?`
		args = append(args, convID)
	}
	if from > 0 {
		q += ` AND m.created_at>=?`
		args = append(args, from)
	}
	if to > 0 {
		q += ` AND m.created_at<=?`
		args = append(args, to)
	}
	q += ` ORDER BY m.id ASC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ExportItem
	for rows.Next() {
		var it ExportItem
		if err := rows.Scan(&it.CreatedAt, &it.Direction, &it.BridgeName, &it.BridgeSlug,
			&it.ChatID, &it.ChatType, &it.ChatTitle, &it.SenderName, &it.SenderUsername,
			&it.Text, &it.ContentKind, &it.TgMessageID); err != nil {
			return nil, err
		}
		out = append(out, &it)
	}
	return out, rows.Err()
}

// SearchMessages 全文检索，供后台搜索框使用。
func (s *Store) SearchMessages(ctx context.Context, bridgeID int64, keyword string, limit int) ([]*MessageRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	kw := "%" + strings.ReplaceAll(strings.ReplaceAll(keyword, "%", `\%`), "_", `\_`) + "%"
	q := `SELECT m.id, m.conversation_id, m.direction, COALESCE(m.tg_message_id,0),
	             COALESCE(m.sender_name,''), m.text, m.created_at
	        FROM messages m WHERE m.text LIKE ? ESCAPE '\'`
	args := []any{kw}
	if bridgeID > 0 {
		q += ` AND m.bridge_id=?`
		args = append(args, bridgeID)
	}
	q += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MessageRow
	for rows.Next() {
		var m MessageRow
		var convID int64
		if err := rows.Scan(&m.ID, &convID, &m.Direction, &m.TgMessageID, &m.SenderName,
			&m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
