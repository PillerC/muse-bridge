package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"muse-bridge/internal/domain"
	"muse-bridge/internal/tg"
)

// Public 返回公网路由：Telegram webhook + Muse 接入 API + 管理后台。
//
// 后台就在根路径上：直接打开域名首页就是总览（没登录会跳 /login），
// 不再需要 /admin 前缀。放开的前提是后台本身有三道闸：
// 会话 cookie 校验、登录失败限流、以及下方 securityHeaders 给的 CSP
// 与 frame-ancestors（脚本只允许同源的复制按钮那一份）。
func (s *Server) Public() http.Handler {
	mux := http.NewServeMux()
	s.registerPublic(mux)
	// 后台和 API 挂在同一个 mux 上，而不是按前缀分流：
	// 根路径用 {$} 精确匹配（只吃 "/"），页面路径都带具体名字
	// （/bridges、/export、/login），和 /poll、/reply、/tg/{slug} 不重叠。
	// 这样既没有前缀分流那一层，也不会出现"新页面忘了同步注册"。
	s.registerAdmin(mux)
	return securityHeaders(mux)
}

// registerPublic 注册 Telegram webhook 与 Muse 接入 API。
func (s *Server) registerPublic(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "muse-bridge"})
	})

	// 以下四个端点保持与 v1 Worker 完全一致的路径和响应字段，
	// Muse 侧的 connector 只需把 host 换掉即可继续工作。
	//
	// /pending-count 是唯一允许匿名访问的端点：hook 脚本没有令牌，
	// 只读一个数字，和 v1 的行为一致。带令牌时只统计该桥。
	mux.HandleFunc("GET /pending-count", s.handlePendingCount)
	mux.HandleFunc("GET /poll", s.withBridge(s.handlePoll))
	mux.HandleFunc("POST /ack", s.withBridge(s.handleAck))
	mux.HandleFunc("POST /fail", s.withBridge(s.handleFail))
	mux.HandleFunc("POST /extend", s.withBridge(s.handleExtend))
	mux.HandleFunc("GET /stats", s.withBridge(s.handleStats))

	// v2 新增：回复经桥转发，既拿到完整出站记录，也让 Muse 永远看不到 bot token。
	mux.HandleFunc("POST /reply", s.withBridge(s.handleReply))

	// 主动推送：不依赖入站任务，直接发到某个会话。见 handlePush。
	mux.HandleFunc("POST /push", s.withBridge(s.handlePush))

	// Telegram webhook：URL 里带 slug，secret 走请求头。
	mux.HandleFunc("POST /tg/{slug}", s.handleWebhook)
}

// securityHeaders 给后台页面加上防点击劫持、防 MIME 嗅探和严格 CSP。
//
// script-src 只放开同源：后台带着令牌明文，页面里出现任何第三方或内联脚本
// 都等于把令牌交给它。所以脚本只有一个自带的 /static/copy.js（复制按钮），
// 模板里依旧不允许写内联 <script>，也没有 'unsafe-inline'。
// 样式用的是内联 style 属性，故 style-src 保留 'unsafe-inline'。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; script-src-attr 'none'; "+
				"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// handlePendingCount 返回可认领的消息数。
//
// 两种调用方式，按优先级：
//  1. 带 Bearer 令牌：只算令牌所属的桥；
//  2. 不带令牌但带 ?bridge=<slug>：只算这一座桥。hook 用这个。
//     slug 不是凭据——它本来就印在详情页上，只能读到一个数字。
//
// 两者都没有时直接报错，**不再返回所有桥的合计**。
// 那个兜底曾经让所有 agent 共用同一个"有活"信号：任何一座桥来消息，
// 所有装了 hook 的 agent 都会被叫醒，醒来 poll 却只领得到自己那座桥的
// 消息，等于白烧一轮推理。让它明确失败，比给一个会误导人的数字好：
// hook 配错了会立刻暴露，而不是安静地多跑几千次。
//
// 每次先回收过期租约：v1 的死锁就是因为回收只藏在 /poll 里，
// 而 /poll 只在 pending>0 时才被调用，agent 崩溃后队列永远不清零。
func (s *Server) handlePendingCount(w http.ResponseWriter, r *http.Request) {
	now := s.svc.Now()
	_, _ = s.svc.Queue.Reap(r.Context(), now)

	var (
		n   int
		err error
	)
	token := bearer(r)
	switch slug := r.URL.Query().Get("bridge"); {
	case token != "":
		b, _, rerr := s.st.ResolveToken(r.Context(), hashToken(token))
		if rerr != nil {
			writeErr(w, http.StatusUnauthorized, "令牌无效或已吊销")
			return
		}
		n, err = s.svc.Queue.PendingCount(r.Context(), b.ID, now)
	case slug != "":
		b, berr := s.st.GetBridgeBySlug(r.Context(), slug)
		if berr != nil {
			// 404 而不是 0：把 slug 写错时 hook 能立刻看出来，不会被
			// 一个永远为 0 的数字蒙过去。
			writeErr(w, http.StatusNotFound, "bridge not found")
			return
		}
		// 暂停的桥不发任务，按 0 报，免得 hook 空转。
		if b.Status == "active" {
			n, err = s.svc.Queue.PendingCount(r.Context(), b.ID, now)
		}
	default:
		// 既没令牌也没 slug：明确报错。
		// 返回 400 而不是 401，是因为这不是鉴权问题——带上 bridge 参数
		// 就能用，不需要任何凭据。
		writeErr(w, http.StatusBadRequest,
			"必须指定桥：加 ?bridge=<桥标识> 参数，或带上 Bearer 令牌")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": n})
}

// handlePoll 认领一批消息。
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	now := s.svc.Now()
	_, _ = s.svc.Queue.Reap(r.Context(), now)

	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 5)
	lease := int64(intParam(q.Get("lease_seconds"), s.svc.Queue.DefaultLeaseSeconds()))

	msgs, leaseID, err := s.svc.Queue.Claim(r.Context(), b.ID, int64(limit), lease, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]map[string]any, 0, len(msgs))

	// 认领即开始"输入中"：用户按下发送后立刻能看到机器人在打字，
	// 而不是等 agent 想完才弹出一整条回复。收不到客户端只是少个提示，不影响认领。
	if len(msgs) > 0 {
		if client, cerr := s.svc.Client(r.Context(), b); cerr == nil {
			deadline := now + lease
			for _, m := range msgs {
				chatID := m.ChatID
				s.svc.Typing.Start(m.JobID, chatID, deadline, func(ctx context.Context) error {
					return client.SendChatAction(ctx, chatID, "typing")
				})
			}
		}
	}

	for _, m := range msgs {
		out = append(out, map[string]any{
			"id":                  m.JobID,
			"message_id":          m.TgMessageID,
			"chat_id":             m.ChatID,
			"chat_type":           m.ChatType,
			"chat_title":          m.ChatTitle,
			"from_id":             m.FromID,
			"from_username":       m.FromUsername,
			"from_name":           m.FromName,
			"text":                m.Text,
			"content_kind":        m.ContentKind,
			"content_hint":        m.ContentHint,
			"reply_to_message_id": m.ReplyToTgID,
			"is_mention":          m.IsMention,
			"is_reply_to_bot":     m.IsReplyToBot,
			"attempts":            m.Attempts,
			"created_at":          m.CreatedAt,
			// v2 在每条消息上也放一份 lease_id，
			// 避免两批消息混在一个数组里时 ack 用错租约。
			"lease_id": m.LeaseID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"lease_id": leaseID,
		"messages": out,
	})
}

// handleAck 标记完成。ids 是 /poll 返回的 id（任务 id）。
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	var body struct {
		IDs     []int64 `json:"ids"`
		LeaseID string  `json:"lease_id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {ids:[], lease_id:''}")
		return
	}
	now := s.svc.Now()
	acked := 0
	for _, id := range body.IDs {
		job, err := s.st.GetJob(r.Context(), id)
		if err != nil || job.BridgeID != b.ID {
			continue
		}
		if err := s.svc.Queue.Ack(r.Context(), id, body.LeaseID, now); err == nil {
			acked++
			// 回复已送达，立刻收掉"输入中"，别让用户继续看机器人转圈。
			s.svc.Typing.Stop(id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked})
}

// handleFail 记录失败并按退避重试。
func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	var body struct {
		ID      int64  `json:"id"`
		LeaseID string `json:"lease_id"`
		Error   string `json:"error"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {id, lease_id, error}")
		return
	}
	job, err := s.st.GetJob(r.Context(), body.ID)
	if err != nil || job.BridgeID != b.ID {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	res, err := s.svc.Queue.Fail(r.Context(), body.ID, body.LeaseID, body.Error, s.svc.Now())
	if errors.Is(err, domain.ErrLeaseMismatch) {
		writeErr(w, http.StatusConflict, "lease_mismatch")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 本轮处理到此为止：要么重排队，要么进死信，都不该继续显示"输入中"。
	s.svc.Typing.Stop(body.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": res.Status, "attempts": res.Attempts, "retry_in_sec": res.RetryAfter,
	})
}

// handleExtend 续租。
func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	var body struct {
		ID           int64  `json:"id"`
		LeaseID      string `json:"lease_id"`
		LeaseSeconds int64  `json:"lease_seconds"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {id, lease_id, lease_seconds}")
		return
	}
	job, err := s.st.GetJob(r.Context(), body.ID)
	if err != nil || job.BridgeID != b.ID {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	now := s.svc.Now()
	if err := s.svc.Queue.Extend(r.Context(), body.ID, body.LeaseID, body.LeaseSeconds, now); err != nil {
		if errors.Is(err, domain.ErrLeaseMismatch) {
			writeErr(w, http.StatusConflict, "lease_mismatch")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// typing 的自动停止时间要跟着租约一起往后推，否则续租之后
	// 用户看到的是"正在输入"突然消失。
	s.svc.Typing.Extend(body.ID, now+body.LeaseSeconds)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "lease_id": body.LeaseID, "expires_at": now + body.LeaseSeconds,
	})
}

// handleStats 队列概况，供 Muse 自检和后台展示。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	now := s.svc.Now()
	_, _ = s.svc.Queue.Reap(r.Context(), now)

	counts, err := s.st.StatusCounts(r.Context(), b.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dead, err := s.st.ListDeadJobs(r.Context(), b.ID, 10)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recent := make([]map[string]any, 0, len(dead))
	for _, d := range dead {
		recent = append(recent, map[string]any{
			"id": d.JobID, "chat_id": d.ChatID, "text": truncate(d.Text, 200),
			"attempts": d.Attempts, "error": d.LastError, "updated_at": d.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "counts": counts, "recent_dead": recent,
	})
}

// handleReply 发送或编辑回复。
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	var body struct {
		ID        int64  `json:"id"`
		LeaseID   string `json:"lease_id"`
		Text      string `json:"text"`
		Mode      string `json:"mode"`
		ParseMode string `json:"parse_mode"`
		Ack       *bool  `json:"ack"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {id, lease_id, text, mode}")
		return
	}
	// readJSON 开了 DisallowUnknownFields：字段名写错（比如 job_id）会直接 400，
	// 比默默当成 id=0 去查一个不存在的任务更好排查。
	autoAck := true
	if body.Ack != nil {
		autoAck = *body.Ack
	}
	res, err := s.svc.SendReply(r.Context(), b, body.ID, body.LeaseID, body.Text, body.Mode, body.ParseMode, autoAck)
	if errors.Is(err, domain.ErrLeaseMismatch) {
		writeErr(w, http.StatusConflict, "lease_mismatch：租约已过期或消息已被重新认领")
		return
	}
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 只发了 draft 说明还在想，继续转圈；final 才算说完。
	// 这里不用等 ack：回复已经送到用户眼前，提示就该收。
	if body.Mode == "" || body.Mode == "final" {
		s.svc.Typing.Stop(body.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "tg_message_id": res.TgMessageID, "parts": res.Parts,
		"draft": res.Draft, "sent_ids": res.SentIDs,
	})
}

// handlePush 主动推送一条消息，不依赖入站任务。
//
// 请求体 {chat_id, text, parse_mode}。chat_id 可省略（0）：
// 省略时发到这座桥最近活跃的会话；这座桥还没有会话时返回 404。
// 带 chat_id 时会话必须已存在于这座桥下，同样不存在返回 404。
// 分段、429 退避、解析失败回退纯文本和出站落库都在 domain 层完成。
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	b := bridgeFrom(r.Context())
	var body struct {
		ChatID    int64  `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 {chat_id, text, parse_mode}")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	res, err := s.svc.SendPush(r.Context(), b, body.ChatID, body.Text, body.ParseMode)
	if isNotFound(err) {
		writeErr(w, http.StatusNotFound, "会话不存在：先让用户给机器人发一条消息，或带上已存在的 chat_id")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "chat_id": res.ChatID, "tg_message_id": res.TgMessageID,
		"parts": res.Parts, "sent_ids": res.SentIDs,
	})
}

// handleWebhook 接收 Telegram 投递。
//
// 两道校验：路径里的 slug 定位桥，请求头里的 secret 证明来源。
// secret 不匹配直接 403，不像 v1 那样在未配置时放行。
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	b, err := s.st.GetBridgeBySlug(r.Context(), slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, "bridge not found")
		return
	}
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(b.WebhookSecret)) != 1 {
		writeErr(w, http.StatusForbidden, "secret mismatch")
		return
	}
	if b.Status != "active" {
		// 暂停的桥也返回 200，让 Telegram 不要再重投，只是不入库。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": "paused"})
		return
	}

	var up tg.Update
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&up); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid update")
		return
	}
	res, err := s.svc.IngestUpdate(r.Context(), b, &up, s.svc.Now())
	if err != nil {
		// 返回 5xx 让 Telegram 重投，靠 update_id 去重保证不会重复入队。
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// reason 是给人排查用的：被白名单挡下的消息 message_id 为 0，
	// 光看 ok=true 会以为入库成功。Telegram 不读这个字段，多给一个没有副作用。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message_id": res.MessageID, "queued": res.Queued, "reason": res.Reason,
	})
}

func intParam(s string, def int) int64 {
	if s == "" {
		return int64(def)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return int64(def)
	}
	return int64(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
