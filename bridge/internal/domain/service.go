package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"muse-bridge/internal/secret"
	"muse-bridge/internal/store"
	"muse-bridge/internal/tg"
)

// Service 把存储、队列和 Telegram 客户端组装在一起，
// HTTP 层只负责解析请求，业务判断都落在这里。
type Service struct {
	Store  *store.Store
	Queue  *Queue
	Typing *Typing
	Box    *secret.Box
	TG     *tg.Factory
	Now    func() int64
	Public string // 形如 https://bridge.example.com，用于拼 webhook 地址
}

// NewService 构造服务。
func NewService(st *store.Store, q *Queue, box *secret.Box, f *tg.Factory, publicURL string) *Service {
	return &Service{Store: st, Queue: q, Typing: NewTyping(), Box: box, TG: f, Public: publicURL,
		Now: func() int64 { return time.Now().Unix() }}
}

// Client 解出某座桥的机器人 token 并返回客户端。明文只在内存中存在。
func (s *Service) Client(ctx context.Context, b *store.Bridge) (*tg.Client, error) {
	token, err := s.Box.Open(b.BotTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("桥 %s 的机器人 token 无法解密: %w", b.Slug, err)
	}
	return s.TG.For(token), nil
}

// IngestResult 描述一条更新的落库结果。
type IngestResult struct {
	MessageID int64  `json:"message_id"`
	Queued    bool   `json:"queued"`
	Duplicate bool   `json:"duplicate"`
	Reason    string `json:"reason"`
	IsMention bool   `json:"is_mention"`
	IsReplyTo bool   `json:"is_reply_to_bot"`
}

// IngestUpdate 把一条 Telegram 更新写进聊天流水，并按需建任务。
//
// 即使不需要回复也照常入库（needs_reply=0），这样后台能看到完整对话，
// 也能直接回答"这条消息为什么没触发"。
func (s *Service) IngestUpdate(ctx context.Context, b *store.Bridge, up *tg.Update, now int64) (IngestResult, error) {
	m := up.Message
	if m == nil {
		m = up.EditedMessage
	}
	if m == nil {
		return IngestResult{Reason: "no_message"}, nil
	}

	verdict := Classify(m, Bot{ID: b.BotID, Username: b.BotUsername}, parseIDs(b.AllowedChatIDs))
	text, kind, hint := m.Content()

	title := m.Chat.Title
	if title == "" {
		title = m.From.DisplayName()
	}

	in := &store.Inbound{
		BridgeID:       b.ID,
		ChatID:         m.Chat.ID,
		ChatType:       m.Chat.Type,
		ChatTitle:      title,
		Direction:      "in",
		TgMessageID:    m.MessageID,
		ReplyToTgID:    replyToID(m),
		SenderID:       senderID(m),
		SenderName:     m.From.DisplayName(),
		SenderUsername: senderUsername(m),
		Text:           text,
		ContentKind:    kind,
		ContentHint:    hint,
		MediaJSON:      mediaJSON(m),
		UpdateID:       up.UpdateID,
		IsMention:      verdict.IsMention,
		IsReplyToBot:   verdict.IsReplyToBot,
		NeedsReply:     verdict.ShouldReply,
		ReplyReason:    verdict.Reason,
		CreatedAt:      m.Date,
	}
	if in.CreatedAt == 0 {
		in.CreatedAt = now
	}

	id, created, err := s.Store.IngestInbound(ctx, in, now)
	if err != nil {
		return IngestResult{}, err
	}
	return IngestResult{
		MessageID: id,
		Queued:    created && verdict.ShouldReply,
		Duplicate: !created,
		Reason:    verdict.Reason,
		IsMention: verdict.IsMention,
		IsReplyTo: verdict.IsReplyToBot,
	}, nil
}

// ReplyResult 是一次回复的结果。
type ReplyResult struct {
	TgMessageID int64   `json:"tg_message_id"`
	Parts       int     `json:"parts"`
	Draft       bool    `json:"draft"`
	SentIDs     []int64 `json:"sent_ids"`
}

// SendReply 把一段文本发回 Telegram。
//
// mode=draft：创建或编辑一条占位消息，用于"思考中…"和渐进输出。
// mode=final：定稿。第一次调用会创建消息，之后分段发送，最后把任务标为 done。
//
// 分段、429 退避、typing 保活、续租和 ack 都在这里完成，
// 不再依赖 Muse 的 prompt 一步步自觉执行。
func (s *Service) SendReply(ctx context.Context, b *store.Bridge, jobID int64, leaseID, text, mode, parseMode string, autoAck bool) (*ReplyResult, error) {
	if mode == "" {
		mode = "final"
	}
	if mode != "draft" && mode != "final" {
		return nil, fmt.Errorf("mode 只能是 draft 或 final")
	}
	job, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.BridgeID != b.ID {
		return nil, store.ErrNotFound
	}
	if job.Status != "processing" || job.LeaseID != leaseID {
		return nil, ErrLeaseMismatch
	}
	if strings.TrimSpace(text) == "" {
		text = "…"
	}
	// Telegram 的老式 Markdown 只认单个 * 表示加粗，模型却习惯输出 **粗体**。
	// 不归一化的话 * 会被当成一对空实体吞掉，结果是文字不加粗、也不报错。
	if parseMode == "Markdown" {
		text = normalizeLegacyMarkdown(text)
	}

	client, err := s.Client(ctx, b)
	if err != nil {
		return nil, err
	}

	// 这里的 typing 由 Typing 管理器负责（认领时启动、final/ack 时停止），
	// 不再在发送过程中另开一个循环，否则同一任务会有两个保活互相叠加。

	limit := 3800
	parts := splitText(text, limit)
	res := &ReplyResult{Parts: len(parts), Draft: mode == "draft"}

	send := func(body string, replyTo int64) (int64, error) {
		sent, err := callWithRetry(ctx, func() (*tg.SentMessage, error) {
			return client.SendMessage(ctx, job.ChatID, body, replyTo, parseMode)
		})
		if err != nil {
			// 解析失败时退回纯文本：agent 输出 Markdown 常带不配对的下划线，
			// 与其让整条回复失败，不如去掉格式照发。
			if parseMode != "" && isParseError(err) {
				sent, err = client.SendMessage(ctx, job.ChatID, body, replyTo, "")
			}
		}
		if err != nil {
			return 0, err
		}
		return sent.MessageID, nil
	}

	edit := func(msgID int64, body string) error {
		err := callWithRetryErr(ctx, func() error {
			return client.EditMessageText(ctx, job.ChatID, msgID, body, parseMode)
		})
		if err != nil && parseMode != "" && isParseError(err) {
			return client.EditMessageText(ctx, job.ChatID, msgID, body, "")
		}
		return err
	}

	first := parts[0]
	if job.ReplyTgID == 0 {
		id, err := send(first, job.ReplyToTgID)
		if err != nil {
			return nil, err
		}
		job.ReplyTgID = id
		s.Store.SetReplyTgID(ctx, job.JobID, id, s.Now())
	} else {
		if err := edit(job.ReplyTgID, first); err != nil {
			return nil, err
		}
	}
	res.TgMessageID = job.ReplyTgID
	res.SentIDs = append(res.SentIDs, job.ReplyTgID)

	if mode == "draft" {
		return res, nil
	}

	var partsJSON []map[string]any
	for i, body := range parts {
		msgID := job.ReplyTgID
		if i > 0 {
			id, err := send(body, 0)
			if err != nil {
				return nil, err
			}
			msgID = id
			res.SentIDs = append(res.SentIDs, id)
		}
		partsJSON = append(partsJSON, map[string]any{"index": i, "message_id": msgID, "text": body})
	}
	blob, _ := json.Marshal(partsJSON)
	if _, err := s.Store.InsertOutbound(ctx, b.ID, job.ConversationID, job.ReplyTgID,
		job.TgMessageID, text, "text", string(blob), "", s.Now()); err != nil {
		return nil, err
	}

	if autoAck {
		if err := s.Queue.Ack(ctx, job.JobID, leaseID, s.Now()); err != nil {
			return res, err
		}
	}
	return res, nil
}

// callWithRetry 处理 429：按 Telegram 的 retry_after 等待后重试，最多 3 次。
func callWithRetry(ctx context.Context, fn func() (*tg.SentMessage, error)) (*tg.SentMessage, error) {
	var last error
	for i := 0; i < 3; i++ {
		out, err := fn()
		if err == nil {
			return out, nil
		}
		last = err
		if !shouldRetry(ctx, err) {
			return nil, err
		}
		if !sleepCtx(ctx, retryDelay(err, i)) {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func callWithRetryErr(ctx context.Context, fn func() error) error {
	var last error
	for i := 0; i < 3; i++ {
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		if !shouldRetry(ctx, err) {
			return err
		}
		if !sleepCtx(ctx, retryDelay(err, i)) {
			return ctx.Err()
		}
	}
	return last
}

func shouldRetry(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 网络错误没有 APIError，视为可重试；Telegram 的永久错误不重试。
	var apiErr *tg.APIError
	if errors.As(err, &apiErr) {
		return !apiErr.Permanent()
	}
	return true
}

func retryDelay(err error, attempt int) time.Duration {
	if sec := tg.RetryAfter(err); sec > 0 {
		if sec > 30 {
			sec = 30
		}
		return time.Duration(sec) * time.Second
	}
	return time.Duration(1<<attempt) * 500 * time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isParseError(err error) bool {
	var apiErr *tg.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	d := strings.ToLower(apiErr.Description)
	return strings.Contains(d, "parse entities") || strings.Contains(d, "can't find end") ||
		strings.Contains(d, "unsupported start tag") || strings.Contains(d, "unmatched")
}

// splitText 按长度切分，优先在换行处断开，并尽量不把代码块切坏。
func splitText(text string, limit int) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) <= limit {
		return []string{text}
	}
	var parts []string
	rest := text
	openFence := ""
	for len(rest) > limit {
		cut := limit
		// 先找段落边界，再找换行，最后才硬切。
		if i := strings.LastIndex(rest[:limit], "\n\n"); i > limit/2 {
			cut = i
		} else if i := strings.LastIndex(rest[:limit], "\n"); i > limit/2 {
			cut = i
		}
		chunk := rest[:cut]
		rest = strings.TrimLeft(rest[cut:], "\n")

		if strings.Count(chunk, "```")%2 == 1 {
			chunk += "\n```"
			openFence = "```\n"
		}
		if openFence != "" {
			chunk = openFence + chunk
			if strings.Count(chunk, "```")%2 == 1 {
				openFence = "```\n"
			} else {
				openFence = ""
			}
		}
		parts = append(parts, chunk)
	}
	if openFence != "" {
		rest = openFence + rest
	}
	if strings.TrimSpace(rest) != "" {
		parts = append(parts, rest)
	}
	return parts
}

func parseIDs(csv string) []int64 {
	var out []int64
	for _, part := range strings.Split(csv, ",") {
		if v, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func replyToID(m *tg.Message) int64 {
	if m.ReplyTo == nil {
		return 0
	}
	return m.ReplyTo.MessageID
}

func senderID(m *tg.Message) int64 {
	if m.From == nil {
		return 0
	}
	return m.From.ID
}

func senderUsername(m *tg.Message) string {
	if m.From == nil {
		return ""
	}
	return m.From.Username
}

// mediaJSON 只保留文件 id 与尺寸这类有用信息，不存整个原始更新——
// 1GB 的盘经不起把每条 update 原文都存一遍。
func mediaJSON(m *tg.Message) string {
	payload := map[string]any{}
	if len(m.Photo) > 0 {
		big := m.Photo[len(m.Photo)-1]
		payload["photo"] = map[string]any{"file_id": big.FileID, "width": big.Width, "height": big.Height}
	}
	if m.Voice != nil {
		payload["voice"] = map[string]any{"file_id": m.Voice.FileID, "duration": m.Voice.Duration}
	}
	if m.Audio != nil {
		payload["audio"] = map[string]any{"file_id": m.Audio.FileID, "duration": m.Audio.Duration, "title": m.Audio.Title}
	}
	if m.Video != nil {
		payload["video"] = map[string]any{"file_id": m.Video.FileID, "duration": m.Video.Duration}
	}
	if m.Document != nil {
		payload["document"] = map[string]any{"file_id": m.Document.FileID, "file_name": m.Document.FileName}
	}
	if m.Sticker != nil {
		payload["sticker"] = map[string]any{"file_id": m.Sticker.FileID, "emoji": m.Sticker.Emoji}
	}
	if len(payload) == 0 {
		return ""
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// normalizeLegacyMarkdown 把标准 Markdown 的强调写法压成 Telegram 老式 Markdown。
//
// 老式 Markdown（parse_mode=Markdown）里 * 是加粗、_ 是斜体，都是单字符定界。
// 模型输出的是 **粗体**、__强调__，在老式解析器眼里就成了两个空实体，
// 星号被吞掉、文字也不加粗——线上实测就是这样。
//
// 只压连续的 * 和 _，不动其它字符；MarkdownV2 本来就是 ** 语义，不经过这里。
func normalizeLegacyMarkdown(s string) string {
	if !strings.ContainsAny(s, "*_") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '*' && c != '_' {
			b.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == c {
			j++
		}
		// 一串连续的定界符压成一个：***x*** 与 **x** 都变成 *x*。
		b.WriteByte(c)
		i = j
	}
	return b.String()
}
