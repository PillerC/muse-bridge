package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"muse-bridge/internal/secret"
	"muse-bridge/internal/store"
)

const sessionCookie = "mb_admin"
const sessionTTL = 12 * time.Hour

//go:embed templates/*.html
var tplFS embed.FS

// Admin 返回后台路由。这个 handler 只挂在 127.0.0.1 上。
func (s *Server) Admin() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /admin/login", s.handleLoginPage)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)

	mux.HandleFunc("GET /admin", s.withAdmin(s.handleDashboard))
	mux.HandleFunc("GET /admin/bridges/new", s.withAdmin(s.handleNewBridgePage))
	mux.HandleFunc("POST /admin/bridges", s.withAdmin(s.handleCreateBridge))
	mux.HandleFunc("GET /admin/bridges/{id}", s.withAdmin(s.handleBridgeDetail))
	mux.HandleFunc("POST /admin/bridges/{id}/settings", s.withAdmin(s.handleBridgeSettings))
	mux.HandleFunc("POST /admin/bridges/{id}/status", s.withAdmin(s.handleBridgeStatus))
	mux.HandleFunc("POST /admin/bridges/{id}/tokens", s.withAdmin(s.handleNewToken))
	mux.HandleFunc("POST /admin/bridges/{id}/tokens/{tid}/revoke", s.withAdmin(s.handleRevokeToken))
	mux.HandleFunc("POST /admin/bridges/{id}/webhook", s.withAdmin(s.handleWebhookAdmin))
	mux.HandleFunc("POST /admin/bridges/{id}/delete", s.withAdmin(s.handleDeleteBridge))

	mux.HandleFunc("GET /admin/conversations/{id}", s.withAdmin(s.handleConversation))
	mux.HandleFunc("GET /admin/export", s.withAdmin(s.handleExportPage))
	mux.HandleFunc("POST /admin/jobs/{id}/replay", s.withAdmin(s.handleReplay))

	return mux
}

// ---------- 渲染 ----------

type pageData struct {
	Title     string
	Active    string
	Admin     bool
	Flash     string
	FlashKind string
	Error     string
	Data      any
}

var funcs = template.FuncMap{
	"ts": func(v int64) string {
		if v == 0 {
			return "-"
		}
		return time.Unix(v, 0).Local().Format("01-02 15:04")
	},
	"truncate": func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, d pageData) {
	if d.Flash == "" {
		d.Flash, d.FlashKind = flashFrom(r)
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		http.Error(w, "模板解析失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 先渲染到缓冲区：模板出错时还能返回一个正常的 500，
	// 而不是把 200 加上半截 HTML 发给浏览器。
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, page, d); err != nil {
		log.Printf("渲染 %s 失败: %v", page, err)
		http.Error(w, "页面渲染失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// ---------- 登录 ----------

type loginLimiter struct {
	mu      sync.Mutex
	attempt map[string][]time.Time
}

var limiter = &loginLimiter{attempt: map[string][]time.Time{}}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	var keep []time.Time
	for _, t := range l.attempt[ip] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	l.attempt[ip] = keep
	return len(keep) < 10
}

func (l *loginLimiter) hit(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempt[ip] = append(l.attempt[ip], time.Now())
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", pageData{Title: "登录"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := r.RemoteAddr
	if !limiter.allow(ip) {
		s.render(w, r, "login.html", pageData{Title: "登录", Error: "尝试次数过多，请 5 分钟后再试"})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, r, "login.html", pageData{Title: "登录", Error: "请求格式错误"})
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	admin, err := s.st.GetAdmin(r.Context(), username)
	if err != nil || !secret.VerifyPassword(admin.PasswordHash, password) {
		limiter.hit(ip)
		s.render(w, r, "login.html", pageData{Title: "登录", Error: "用户名或密码不正确"})
		return
	}

	token := secret.NewToken()
	now := s.svc.Now()
	if err := s.st.CreateSession(r.Context(), secret.HashToken(token), now+int64(sessionTTL.Seconds()), now); err != nil {
		s.render(w, r, "login.html", pageData{Title: "登录", Error: "创建会话失败"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.DeleteSession(r.Context(), secret.HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ---------- 总览 ----------

type bridgeRow struct {
	*store.Bridge
	Pending       int
	Conversations int
}

type dashboardData struct {
	Bridges       []bridgeRow
	Pending       int
	Processing    int
	Dead          int
	TodayMessages int
	DeadJobs      []*store.DeadJob
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bridges, err := s.st.ListBridges(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	data := dashboardData{}
	for _, b := range bridges {
		pending, _ := s.svc.Queue.PendingCount(ctx, b.ID, s.svc.Now())
		convs, _ := s.st.ListConversations(ctx, b.ID, 500)
		data.Bridges = append(data.Bridges, bridgeRow{Bridge: b, Pending: pending, Conversations: len(convs)})
		data.Pending += pending
	}
	counts, _ := s.st.StatusCounts(ctx, 0)
	data.Processing = counts["processing"]
	data.Dead = counts["dead"]
	data.TodayMessages = s.todayMessages(ctx)
	data.DeadJobs, _ = s.st.ListDeadJobs(ctx, 0, 20)

	s.render(w, r, "dashboard.html", pageData{Title: "总览", Active: "dashboard", Admin: true, Data: data})
}

func (s *Server) todayMessages(ctx context.Context) int {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var n int
	_ = s.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE created_at>=?`, start).Scan(&n)
	return n
}

// ---------- 新建桥 ----------

type newBridgeData struct {
	DefaultMode string
	PublicURL   string
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

func (s *Server) handleNewBridgePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "bridge_new.html", pageData{
		Title: "新建桥", Active: "new", Admin: true,
		Data: newBridgeData{DefaultMode: s.cfg.DefaultMode, PublicURL: s.cfg.PublicURL},
	})
}

func (s *Server) handleCreateBridge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "err", "请求格式错误")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	botToken := strings.TrimSpace(r.PostFormValue("bot_token"))
	allowed := strings.TrimSpace(r.PostFormValue("allowed_chat_ids"))
	mode := r.PostFormValue("mode")
	if mode != "poll" {
		mode = "webhook"
	}
	if name == "" || !slugRe.MatchString(slug) || botToken == "" {
		s.flash(w, r, "err", "名字、slug、机器人 token 都必须填写；slug 只能是小写字母、数字和连字符")
		return
	}

	client := s.svc.TG.For(botToken)
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := client.GetMe(callCtx)
	if err != nil {
		s.flash(w, r, "err", "Token 校验失败："+err.Error())
		return
	}

	enc, err := s.svc.Box.Seal(botToken)
	if err != nil {
		s.flash(w, r, "err", "加密机器人 token 失败："+err.Error())
		return
	}
	now := s.svc.Now()
	b := &store.Bridge{
		Slug: slug, Name: name, BotTokenEnc: enc, BotID: info.ID, BotUsername: info.Username,
		WebhookSecret: secret.RandomHex(24), AllowedChatIDs: allowed, Mode: mode,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	id, err := s.st.CreateBridge(ctx, b)
	if err != nil {
		s.flash(w, r, "err", "创建失败："+err.Error())
		return
	}
	b.ID = id

	token := secret.NewToken()
	if _, err := s.st.CreateToken(ctx, id, "默认", secret.HashToken(token), secret.TokenPrefix(token), now); err != nil {
		s.flash(w, r, "err", "创建令牌失败："+err.Error())
		return
	}

	notice := ""
	if mode == "webhook" && s.cfg.PublicURL != "" {
		whCtx, whCancel := context.WithTimeout(ctx, 20*time.Second)
		defer whCancel()
		if err := client.SetWebhook(whCtx, s.cfg.PublicURL+"/tg/"+slug, b.WebhookSecret); err != nil {
			notice = "webhook 注册失败（" + err.Error() + "），可在详情页重试；也可以先用长轮询。"
		}
	}
	s.renderBridge(w, r, b, token, notice)
}

// ---------- 桥详情 ----------

type bridgeDetailData struct {
	Bridge        *store.Bridge
	Tokens        []*store.Token
	Conversations []*store.Conversation
	DeadJobs      []*store.DeadJob
	BaseURL       string
	WebhookURL    string
	NewToken      string
}

func (s *Server) handleBridgeDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderBridge(w, r, b, "", "")
}

func (s *Server) renderBridge(w http.ResponseWriter, r *http.Request, b *store.Bridge, newToken, flashMsg string) {
	ctx := r.Context()
	tokens, _ := s.st.ListTokens(ctx, b.ID)
	convs, _ := s.st.ListConversations(ctx, b.ID, 100)
	dead, _ := s.st.ListDeadJobs(ctx, b.ID, 20)

	base := s.cfg.PublicURL
	if base == "" {
		base = "http://" + s.cfg.Addr
	}
	wh := ""
	if s.cfg.PublicURL != "" {
		wh = s.cfg.PublicURL + "/tg/" + b.Slug
	}
	s.render(w, r, "bridge_detail.html", pageData{
		Title: b.Name, Active: "dashboard", Admin: true,
		Flash: flashMsg, FlashKind: "err",
		Data: bridgeDetailData{
			Bridge: b, Tokens: tokens, Conversations: convs, DeadJobs: dead,
			BaseURL: base, WebhookURL: wh, NewToken: newToken,
		},
	})
}

func (s *Server) handleBridgeSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	name := strings.TrimSpace(r.PostFormValue("name"))
	allowed := strings.TrimSpace(r.PostFormValue("allowed_chat_ids"))
	mode := r.PostFormValue("mode")
	if mode != "poll" {
		mode = "webhook"
	}
	if name == "" {
		s.flashToBridge(w, r, id, "名字不能为空")
		return
	}
	if err := s.st.UpdateBridgeSettings(r.Context(), id, name, allowed, mode, s.svc.Now()); err != nil {
		s.flashToBridge(w, r, id, "保存失败："+err.Error())
		return
	}
	// 收消息方式变了就同步 Telegram，否则从 webhook 切到长轮询会一直 409 Conflict。
	if b, err := s.st.GetBridgeByID(r.Context(), id); err == nil {
		if client, err := s.svc.Client(r.Context(), b); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			switch {
			case mode == "poll":
				_ = client.DeleteWebhook(ctx)
			case mode == "webhook" && s.cfg.PublicURL != "":
				_ = client.SetWebhook(ctx, s.cfg.PublicURL+"/tg/"+b.Slug, b.WebhookSecret)
			}
		}
	}
	http.Redirect(w, r, "/admin/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	status := r.PostFormValue("status")
	if status != "active" && status != "paused" {
		status = "paused"
	}
	_ = s.st.SetBridgeStatus(r.Context(), id, status, s.svc.Now())
	http.Redirect(w, r, "/admin/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleNewToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	token := secret.NewToken()
	now := s.svc.Now()
	if _, err := s.st.CreateToken(r.Context(), id, "手动签发", secret.HashToken(token), secret.TokenPrefix(token), now); err != nil {
		s.flashToBridge(w, r, id, "签发失败："+err.Error())
		return
	}
	s.renderBridge(w, r, b, token, "")
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tid, _ := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	_ = s.st.RevokeToken(r.Context(), id, tid, s.svc.Now())
	http.Redirect(w, r, "/admin/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleWebhookAdmin(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	client, err := s.svc.Client(r.Context(), b)
	if err != nil {
		s.flashToBridge(w, r, id, "无法解密 token："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if r.PostFormValue("action") == "delete" {
		err = client.DeleteWebhook(ctx)
	} else {
		if s.cfg.PublicURL == "" {
			s.flashToBridge(w, r, id, "未配置 BRIDGE_PUBLIC_URL，无法注册 webhook")
			return
		}
		err = client.SetWebhook(ctx, s.cfg.PublicURL+"/tg/"+b.Slug, b.WebhookSecret)
	}
	if err != nil {
		s.flashToBridge(w, r, id, "操作失败："+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDeleteBridge(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteBridge(r.Context(), id); err != nil {
		s.flashToBridge(w, r, id, "删除失败："+err.Error())
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---------- 会话与导出 ----------

type conversationData struct {
	Bridge       *store.Bridge
	Conversation *store.Conversation
	Messages     []*store.MessageRow
	PrevID       int64
	NextID       bool
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	conv, err := s.st.GetConversation(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := s.st.GetBridgeByID(r.Context(), conv.BridgeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := s.st.ListMessages(r.Context(), id, before, 80)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// 查出来是新到旧，反转成时间顺序。
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	var prev int64
	if len(rows) > 0 {
		prev = rows[0].ID
	}
	s.render(w, r, "conversation.html", pageData{
		Title: conv.Title, Active: "dashboard", Admin: true,
		Data: conversationData{Bridge: b, Conversation: conv, Messages: rows, PrevID: prev, NextID: before > 0},
	})
}

func (s *Server) handleExportPage(w http.ResponseWriter, r *http.Request) {
	bridges, _ := s.st.ListBridges(r.Context())
	if r.URL.Query().Get("format") != "" {
		s.doExport(w, r)
		return
	}
	s.render(w, r, "export.html", pageData{
		Title: "导出", Active: "export", Admin: true,
		Data: struct{ Bridges []*store.Bridge }{Bridges: bridges},
	})
}

func (s *Server) doExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bridgeID, _ := strconv.ParseInt(q.Get("bridge_id"), 10, 64)
	convID, _ := strconv.ParseInt(q.Get("conversation_id"), 10, 64)
	from := parseTime(q.Get("from"), false)
	to := parseTime(q.Get("to"), true)

	items, err := s.st.ExportMessages(r.Context(), bridgeID, convID, from, to)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	name := "muse-bridge-" + time.Now().Format("20060102-1504")

	if q.Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		type row struct {
			Time      string `json:"time"`
			Direction string `json:"direction"`
			Bridge    string `json:"bridge"`
			ChatID    int64  `json:"chat_id"`
			ChatType  string `json:"chat_type"`
			ChatTitle string `json:"chat_title"`
			Sender    string `json:"sender"`
			Username  string `json:"username"`
			Text      string `json:"text"`
			Kind      string `json:"kind"`
			TgMessage int64  `json:"tg_message_id"`
		}
		out := make([]row, 0, len(items))
		for _, it := range items {
			out = append(out, row{
				Time:      time.Unix(it.CreatedAt, 0).Local().Format("2006-01-02 15:04:05"),
				Direction: it.Direction, Bridge: it.BridgeName, ChatID: it.ChatID,
				ChatType: it.ChatType, ChatTitle: it.ChatTitle, Sender: it.SenderName,
				Username: it.SenderUsername, Text: it.Text, Kind: it.ContentKind,
				TgMessage: it.TgMessageID,
			})
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF}) // BOM，Excel 打开不乱码
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"时间", "方向", "桥", "chat_id", "类型", "会话标题", "发送者", "用户名", "内容", "消息类型", "TG消息ID"})
	for _, it := range items {
		dir := "收到"
		if it.Direction == "out" {
			dir = "发出"
		}
		_ = cw.Write([]string{
			time.Unix(it.CreatedAt, 0).Local().Format("2006-01-02 15:04:05"),
			dir, it.BridgeName, strconv.FormatInt(it.ChatID, 10), it.ChatType,
			it.ChatTitle, it.SenderName, it.SenderUsername, it.Text, it.ContentKind,
			strconv.FormatInt(it.TgMessageID, 10),
		})
	}
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	job, err := s.st.GetJob(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.svc.Queue.Replay(r.Context(), id, s.svc.Now()); err != nil {
		s.flashToBridge(w, r, job.BridgeID, "重放失败："+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/bridges/"+strconv.FormatInt(job.BridgeID, 10), http.StatusSeeOther)
}

// ---------- 小工具 ----------

// flashFrom 取出一次性提示并清掉 cookie。
func flashFrom(r *http.Request) (string, string) {
	c, err := r.Cookie("mb_flash")
	if err != nil || c.Value == "" {
		return "", ""
	}
	kind, msg, ok := strings.Cut(c.Value, "|")
	if !ok {
		return "", ""
	}
	return msg, kind
}

// flash 用一个一次性 cookie 传提示，避免把错误塞进 URL 被日志记下来。
func (s *Server) flash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "mb_flash", Value: kind + "|" + msg, Path: "/", MaxAge: 30,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	referer := r.Referer()
	if referer == "" {
		referer = "/admin"
	}
	http.Redirect(w, r, referer, http.StatusSeeOther)
}

func (s *Server) flashToBridge(w http.ResponseWriter, r *http.Request, _ int64, msg string) {
	s.flash(w, r, "err", msg)
}

func parseTime(v string, endOfDay bool) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, v, time.Local); err == nil {
			if l == "2006-01-02" && endOfDay {
				t = t.Add(24*time.Hour - time.Second)
			}
			return t.Unix()
		}
	}
	return 0
}

// 保证 embed 的 FS 满足接口（也便于以后加静态资源）。
var _ fs.FS = tplFS
