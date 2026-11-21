package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"muse-bridge/internal/domain"
	"muse-bridge/internal/secret"
	"muse-bridge/internal/store"
)

const sessionCookie = "mb_admin"
const sessionTTL = 12 * time.Hour

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Admin 返回只含后台的独立 handler。
//
// 给 127.0.0.1 的兜底监听用（TLS 出问题、公网 443 打不开时还能从本机进）。
// 公网那边不是这样挂的：后台和 API 合在一个 mux 上，见 Public()。
func (s *Server) Admin() http.Handler {
	mux := http.NewServeMux()
	s.registerAdmin(mux)
	return securityHeaders(mux)
}

// registerAdmin 把后台页面注册到一个 mux 上，路径全在根下。
//
// 域名首页就是后台，不再有 /admin 前缀：用户只需要记住一个地址，
// 手机上也不用敲路径。页面路径都带具体名字（/bridges、/export、/login），
// 和 API 的 /poll、/reply、/tg/{slug} 不重叠，所以能安全地共用一个 mux。
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// {$} 只匹配根路径本身，不会把 /poll 之类吞掉。
	mux.HandleFunc("GET /{$}", s.withAdmin(s.handleDashboard))
	mux.HandleFunc("GET /bridges/new", s.withAdmin(s.handleNewBridgePage))
	mux.HandleFunc("POST /bridges", s.withAdmin(s.handleCreateBridge))
	mux.HandleFunc("GET /bridges/{id}", s.withAdmin(s.handleBridgeDetail))
	mux.HandleFunc("POST /bridges/{id}/settings", s.withAdmin(s.handleBridgeSettings))
	mux.HandleFunc("POST /bridges/{id}/status", s.withAdmin(s.handleBridgeStatus))
	mux.HandleFunc("POST /bridges/{id}/tokens/rotate", s.withAdmin(s.handleRotateToken))
	mux.HandleFunc("GET /bridges/{id}/webhook-status", s.withAdmin(s.handleWebhookStatus))
	mux.HandleFunc("POST /bridges/{id}/delete", s.withAdmin(s.handleDeleteBridge))
	mux.HandleFunc("GET /bridges/{id}/delete", s.withAdmin(s.handleDeleteConfirm))

	mux.HandleFunc("GET /conversations/{id}", s.withAdmin(s.handleConversation))
	mux.HandleFunc("GET /export", s.withAdmin(s.handleExportPage))
	mux.HandleFunc("POST /jobs/{id}/replay", s.withAdmin(s.handleReplay))

	// 账号：改用户名和密码。只有登录后能进，改密码要求先验当前密码。
	mux.HandleFunc("GET /account", s.withAdmin(s.handleAccountPage))
	mux.HandleFunc("POST /account", s.withAdmin(s.handleAccountSave))

	// 后台唯一的静态资源：复制按钮用的一小段脚本。
	// 以前后台完全不带 JS（CSP 里 script-src 是禁死的），但"一键复制"
	// 没有别的办法——execCommand 需要脚本，纯 CSS 做不到写剪贴板。
	// 折中是只允许同源的这一个文件，模板里仍然不允许出现内联脚本。
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
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

// loginLimiter 按来源 IP 统计失败次数。
//
// 键必须是纯 IP：如果直接用 r.RemoteAddr，里面带着源端口，
// 攻击者每建一条新连接端口都不同，等于限流完全失效。
type loginLimiter struct {
	mu      sync.Mutex
	attempt map[string][]time.Time
}

// maxTrackedIPs 封住 map 的无界增长：攻击者换 IP 刷时不会吃光内存。
const maxTrackedIPs = 20000

var limiter = &loginLimiter{attempt: map[string][]time.Time{}}

// clientIP 从 RemoteAddr 去掉端口。本服务直接对外监听，
// 不信任 X-Forwarded-For（没有反向代理，伪造头就能绕过限流）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

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
	if len(keep) == 0 {
		delete(l.attempt, ip)
	} else {
		l.attempt[ip] = keep
	}
	return len(keep) < 10
}

func (l *loginLimiter) hit(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.attempt[ip]; !ok && len(l.attempt) >= maxTrackedIPs {
		// 满了就随机丢一个，够用了：这条路径只为限制暴力破解，
		// 不做精确统计，宁可误放一次也不能让内存无上限增长。
		for k := range l.attempt {
			delete(l.attempt, k)
			break
		}
	}
	l.attempt[ip] = append(l.attempt[ip], time.Now())
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", pageData{Title: "登录"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.DeleteSession(r.Context(), secret.HashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- 账号 ----------

type accountData struct {
	Username string
}

// currentAdmin 取当前的后台账号。
//
// 表结构按多人预留了，但目前没有"新建账号"的入口，实际只有一行：
// 优先认配置里的用户名，改名之后它就对不上了，退回最早的那一行。
func (s *Server) currentAdmin(ctx context.Context) (*store.Admin, error) {
	if a, err := s.st.GetAdmin(ctx, s.cfg.AdminUser); err == nil {
		return a, nil
	}
	all, err := s.st.ListAdmins(ctx)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, store.ErrNotFound
	}
	return all[0], nil
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	a, err := s.currentAdmin(r.Context())
	if err != nil {
		http.Error(w, "账号不存在", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "account.html", pageData{
		Title: "账号", Active: "account", Admin: true,
		Data: accountData{Username: a.Username},
	})
}

// handleAccountSave 改用户名和密码。
//
// 改密码要求先验当前密码：这是页面上唯一能凭会话直接换掉钥匙的操作，
// 万一 cookie 被顺走，至少不能直接把主人锁在门外。
func (s *Server) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ctx := r.Context()
	a, err := s.currentAdmin(ctx)
	if err != nil {
		http.Error(w, "账号不存在", http.StatusInternalServerError)
		return
	}
	if !secret.VerifyPassword(a.PasswordHash, r.PostFormValue("current_password")) {
		s.flash(w, r, "err", "当前密码不对，没有做任何修改")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	if err := validateAdminUsername(username); err != nil {
		s.flash(w, r, "err", err.Error())
		return
	}
	newPass := r.PostFormValue("password")
	changedPass := newPass != ""
	if changedPass {
		if len(newPass) < 8 {
			s.flash(w, r, "err", "新密码至少 8 位")
			return
		}
		if newPass != r.PostFormValue("password2") {
			s.flash(w, r, "err", "两次输入的新密码不一致")
			return
		}
	}
	hash := a.PasswordHash
	if changedPass {
		hash = secret.HashPassword(newPass)
	}
	if err := s.st.UpdateAdmin(ctx, a.ID, username, hash); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			s.flash(w, r, "err", "这个用户名已经被占用了")
			return
		}
		s.flash(w, r, "err", "保存失败："+err.Error())
		return
	}
	if changedPass {
		// 密码变了，所有旧 cookie 一律作废，包括当前这个。
		s.st.DeleteAllSessions(ctx)
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		setFlash(w, "ok", "密码已更新，请用新密码登录")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	setFlash(w, "ok", "已保存")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// validateAdminUsername 只挡明显不合理的输入。
// 中文是允许的：这台机器的使用者写中文比拼音顺手。
func validateAdminUsername(name string) error {
	if name == "" {
		return errors.New("用户名不能为空")
	}
	if n := utf8.RuneCountInString(name); n < 2 || n > 32 {
		return errors.New("用户名长度要在 2–32 个字符之间")
	}
	if strings.ContainsAny(name, " \t\r\n/\\") {
		return errors.New("用户名里不能有空格或斜杠")
	}
	return nil
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
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

func (s *Server) handleNewBridgePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "bridge_new.html", pageData{
		Title: "新建桥", Active: "new", Admin: true,
		Data: newBridgeData{DefaultMode: s.cfg.DefaultMode},
	})
}

func (s *Server) handleCreateBridge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "err", "请求格式错误")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	botToken := strings.TrimSpace(r.PostFormValue("bot_token"))
	allowed, err := parseAllowedUserIDs(r.PostFormValue("allowed_user_ids"))
	if err != nil {
		s.flash(w, r, "err", err.Error())
		return
	}
	mode := r.PostFormValue("mode")
	if mode != "poll" {
		mode = "webhook"
	}
	if name == "" || botToken == "" {
		s.flash(w, r, "err", "桥名字和机器人 token 都必须填写")
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
	// slug 不再让人填：它只是 webhook 的路径段，用户没有理由关心它，
	// 让人先想一个不重名的 slug 纯属添堵。随机生成，撞名了再摇一次。
	b := &store.Bridge{
		Slug: secret.RandomSlug(10), Name: name, BotTokenEnc: enc, BotID: info.ID, BotUsername: info.Username,
		WebhookSecret: secret.RandomHex(24), AllowedUserIDs: allowed, Mode: mode,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	var id int64
	for attempt := 0; ; attempt++ {
		if !slugRe.MatchString(b.Slug) {
			s.flash(w, r, "err", "生成的 slug 不合法："+b.Slug)
			return
		}
		id, err = s.st.CreateBridge(ctx, b)
		if err == nil {
			break
		}
		// 只有撞名才值得重摇；其它错误重试多少次都一样。
		if attempt >= 5 || !strings.Contains(err.Error(), "UNIQUE") {
			s.flash(w, r, "err", "创建失败："+err.Error())
			return
		}
		b.Slug = secret.RandomSlug(10)
	}
	b.ID = id

	token := secret.NewToken()
	encToken, err := s.svc.Box.Seal(token)
	if err != nil {
		s.flash(w, r, "err", "加密令牌失败："+err.Error())
		return
	}
	if _, err := s.st.CreateToken(ctx, id, "默认", secret.HashToken(token), secret.TokenPrefix(token), encToken, now); err != nil {
		s.flash(w, r, "err", "创建令牌失败："+err.Error())
		return
	}

	if mode == "webhook" && s.cfg.PublicURL != "" {
		whCtx, whCancel := context.WithTimeout(ctx, 20*time.Second)
		defer whCancel()
		if err := client.SetWebhook(whCtx, s.cfg.PublicURL+"/tg/"+b.Slug, b.WebhookSecret); err != nil {
			setFlash(w, "err", "webhook 注册失败（"+err.Error()+"），可在详情页重试；也可以改用长轮询。")
		}
	}
	// 用 303 跳过去而不是直接把详情页渲染在这个 POST 上：
	// 否则刷新一次就重新提交一遍建桥表单，第二次会多建一座桥（还要多调一次 getMe）。
	// 令牌已经存了密文，跳转后详情页照样能显示，不再依赖"只此一次"。
	http.Redirect(w, r, "/bridges/"+strconv.FormatInt(id, 10)+"?created=1", http.StatusSeeOther)
}

// ---------- 桥详情 ----------

type bridgeDetailData struct {
	Bridge        *store.Bridge
	Tokens        []*store.Token
	Conversations []*store.Conversation
	DeadJobs      []*store.DeadJob
	Blocked       *domain.BlockedNotice

	// 接入 Muse 的两段可复制文本。
	//
	// 令牌刻意不拼进提示词：提示词是"拿去发给别人/别的 agent"的东西，
	// 一旦混进令牌，它就会跟着聊天记录、剪贴板历史一起扩散。
	// 分成两个按钮，用户想什么时候发、发给谁都能自己控制。
	Prompt      string
	Token       string // 当前有效令牌的明文；没有可用令牌时为空
	TokenLegacy bool   // 有有效令牌，但签发于"存密文"之前，复制不出来
	JustCreated bool
}

func (s *Server) handleBridgeDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.renderBridge(w, r, b, r.URL.Query().Get("created") == "1")
}

func (s *Server) renderBridge(w http.ResponseWriter, r *http.Request, b *store.Bridge, justCreated bool) {
	ctx := r.Context()
	tokens, _ := s.st.ListTokens(ctx, b.ID)
	convs, _ := s.st.ListConversations(ctx, b.ID, 100)
	dead, _ := s.st.ListDeadJobs(ctx, b.ID, 20)

	base := s.cfg.PublicURL
	if base == "" {
		base = "http://" + s.cfg.Addr
	}
	// 最近一条被白名单拦下的消息只活在内存里，重启后没有就没有了。
	// 它对"填错 ID 所以毫无反应"这种情形是唯一的可见线索。
	var blocked *domain.BlockedNotice
	if n, ok := s.svc.BlockedNotice(b.ID); ok {
		blocked = &n
	}

	// 令牌按签发顺序倒序，取最新一枚还有效的。
	legacy := false
	tokPlain := ""
	for _, t := range tokens {
		if t.RevokedAt != 0 {
			continue
		}
		if t.TokenEnc == "" {
			legacy = true
			break
		}
		p, err := s.svc.Box.Open(t.TokenEnc)
		if err != nil {
			legacy = true
			break
		}
		tokPlain = p
		break
	}

	s.render(w, r, "bridge_detail.html", pageData{
		Title: b.Name, Active: "dashboard", Admin: true,
		Data: bridgeDetailData{
			Bridge: b, Tokens: tokens, Conversations: convs, DeadJobs: dead,
			Blocked: blocked,
			Prompt:  onboardPrompt(base), Token: tokPlain,
			TokenLegacy: legacy, JustCreated: justCreated,
		},
	})
}

// onboardPrompt 生成"发给 Muse"的接入说明。
//
// 里面只放地址和调用节奏，不放令牌：这份文本是要被整段复制到别处的，
// 任何凭据都不该跟着走。文案是用户定稿的，要改先对齐，别自行增删。
func onboardPrompt(base string) string {
	return `桥地址：` + base + `

这是一座 Muse Bridge：Telegram Bot 的消息队列。用户在 Telegram 里给机器人发消息，消息会进这座桥的队列；你认领、回复，桥负责把回复发回 Telegram。

接入步骤：
1. 注册 connector：api_key，bearer_header，api_hosts 用上面的桥地址；
2. 令牌我另外单独发给你 —— 通过 Muse 的安全录入页面提交，不要让我粘贴在聊天里。收到后按 bearer_header 方式带上，不要写进文档、脚本、命令行参数或日志；
3. 不要向我要机器人 token —— 它在桥里，Muse 不需要也不该拿到；也不要自己调 Telegram Bot API 发消息，一切发送都走桥的 reply；
4. 写一个 helper（命名为 muse-bridge.py），实现下面的接口；写好后先跑一次 stats 确认连通，再开始收消息。如果超时，重试一次再判断。

接口（除 pending-count 外都要带 Bearer 令牌；返回都是 JSON，成功时 ok 为 true）：
- GET /pending-count（公开，不用令牌）→ {"ok":true,"pending":N}，N 是待处理消息数；
- GET /poll?limit=5&lease_seconds=180 → 认领待处理消息。每条任务有 job_id、lease_id、lease_seconds，以及消息文本、发送者、chat_id 等字段；lease_seconds 要够覆盖你的思考时间，默认 180 秒；
- POST /reply {"job_id":1,"lease_id":"...","text":"..."} → 发出回复。桥会自动分段、退避重试并 ack，发完不用再调 ack；
- POST /ack {"ids":[1],"lease_id":"..."} → 不回复，直接完成（确实不需要回复的消息用它）；
- POST /fail {"id":1,"lease_id":"...","error":"..."} → 没能完成，让桥稍后重投；超过上限进死信，后台可重放；
- POST /extend {"id":1,"lease_id":"...","lease_seconds":180} → 续租；
- GET /stats → 队列状态（各状态计数和最近的死信）。

每条消息的节奏：
- pending-count 为 0 就结束这一轮，不要回复任何东西。这个端点是公开的，适合高频轮询；
- 有消息就 poll 认领；poll 返回空说明已被别的 agent 认领或队列已空，直接结束；
- 想好后用 reply 发出；
- 长任务在租约过半时 extend 续租。

「正在输入…」由桥负责：消息被认领的那一刻起就持续给 Telegram 发 typing，
直到 reply / ack / fail 或租约到期。你不需要发 sendChatAction。`
}

func (s *Server) handleBridgeSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	name := strings.TrimSpace(r.PostFormValue("name"))
	allowed, err := parseAllowedUserIDs(r.PostFormValue("allowed_user_ids"))
	if err != nil {
		s.flashToBridge(w, r, id, err.Error())
		return
	}
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
	// 清空名单等于重新认领：说清楚会发生什么，否则下一次陌生人发消息
	// 就把桥绑走了，人只会觉得莫名其妙。
	if allowed == "" {
		setFlash(w, "ok", "已清空允许列表：下一个给机器人发消息的人会被自动绑定")
	}
	http.Redirect(w, r, "/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	_ = r.ParseForm()
	status := r.PostFormValue("status")
	if status != "active" && status != "paused" {
		status = "paused"
	}
	_ = s.st.SetBridgeStatus(r.Context(), id, status, s.svc.Now())
	http.Redirect(w, r, "/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleRotateToken 换一枚新令牌，并把旧的全部作废。
//
// 顺序是"先发新的、再废旧的"：中间不留零令牌的瞬间。
// Muse 可能正在轮询，先废后发会让它吃到 401 然后停下来等人。
func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := s.st.GetBridgeByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	token := secret.NewToken()
	enc, err := s.svc.Box.Seal(token)
	if err != nil {
		s.flashToBridge(w, r, id, "加密令牌失败："+err.Error())
		return
	}
	now := s.svc.Now()
	newID, err := s.st.CreateToken(r.Context(), id, "轮转",
		secret.HashToken(token), secret.TokenPrefix(token), enc, now)
	if err != nil {
		s.flashToBridge(w, r, id, "轮转失败："+err.Error())
		return
	}
	tokens, _ := s.st.ListTokens(r.Context(), id)
	revoked := 0
	for _, t := range tokens {
		if t.ID == newID || t.RevokedAt != 0 {
			continue
		}
		if err := s.st.RevokeToken(r.Context(), id, t.ID, now); err == nil {
			revoked++
		}
	}
	if revoked > 0 {
		setFlash(w, "ok", fmt.Sprintf("已轮转：新令牌生效，%d 枚旧令牌作废。把新令牌发给 Muse。", revoked))
	} else {
		setFlash(w, "ok", "已签发令牌。点「复制令牌」把它发给 Muse。")
	}
	http.Redirect(w, r, "/bridges/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleWebhookStatus 返回 Telegram 侧的 webhook 实况。
//
// 排查"消息没进来"时分三种情况：
//   - LastErrorMessage 非空：Telegram 打了但桥返回了错误，多半是证书或路径不对；
//   - PendingCount > 0：桥在收但处理失败，Telegram 一直重投；
//   - 都为空且 URL 为空：setWebhook 没生效，Telegram 根本不知道往哪打。
func (s *Server) handleWebhookStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "桥不存在")
		return
	}
	client, err := s.svc.Client(r.Context(), b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "解密机器人 token 失败："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := client.GetWebhookInfo(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "调用 getWebhookInfo 失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"expected_url":     s.cfg.PublicURL + "/tg/" + b.Slug,
		"url":              info.URL,
		"url_matches":      info.URL == s.cfg.PublicURL+"/tg/"+b.Slug,
		"pending_count":    info.PendingCount,
		"last_error":       info.LastErrorMessage,
		"last_error_epoch": info.LastErrorDate,
		"max_connections":  info.MaxConnections,
	})
}

// handleDeleteConfirm 渲染删除确认页。
//
// 用服务端确认页而不是 onsubmit="return confirm(...)"：
// 后台的 CSP 里 script-src 是禁用的，内联事件处理器会被浏览器丢弃，
// 那样"确认删除"会退化成点一下就直接删。删除是不可逆操作，
// 不能依赖一个可能被 CSP 静默挡掉的对话框。
func (s *Server) handleDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b, err := s.st.GetBridgeByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "confirm_delete.html", pageData{Title: "删除桥", Data: bridgeDetailData{Bridge: b}})
}

func (s *Server) handleDeleteBridge(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteBridge(r.Context(), id); err != nil {
		s.flashToBridge(w, r, id, "删除失败："+err.Error())
		return
	}
	s.svc.ForgetBlocked(id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	http.Redirect(w, r, "/bridges/"+strconv.FormatInt(job.BridgeID, 10), http.StatusSeeOther)
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
// setFlash 把一条一次性提示放进 cookie，下次页面渲染时读出来显示。
func setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "mb_flash", Value: kind + "|" + msg, Path: "/", MaxAge: 30,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) flash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	setFlash(w, kind, msg)
	referer := r.Referer()
	if referer == "" {
		referer = "/"
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
