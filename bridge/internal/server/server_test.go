package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"muse-bridge/internal/config"
	"muse-bridge/internal/domain"
	"muse-bridge/internal/secret"
	"muse-bridge/internal/server"
	"muse-bridge/internal/store"
	"muse-bridge/internal/tg"
)

// fakeTG 是一个最小的 Telegram Bot API 替身，只记录被调用了什么。
type fakeTG struct {
	mu      sync.Mutex
	calls   []string
	nextID  int64
	sent    []string
	editLog []string
}

// count 返回某个方法被调用的次数，用来断言 typing 之类的副作用。
func (f *fakeTG) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeTG) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		method := ""
		if len(parts) >= 2 {
			method = parts[len(parts)-1]
		}
		body, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.calls = append(f.calls, method)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Muse","username":"musebot"}}`)
		case "sendMessage":
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &payload)
			f.mu.Lock()
			f.nextID++
			id := f.nextID
			f.sent = append(f.sent, payload.Text)
			f.mu.Unlock()
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":1,"type":"private"},"date":0}}`, id)
		case "editMessageText":
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(body, &payload)
			f.mu.Lock()
			f.editLog = append(f.editLog, payload.Text)
			f.mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"},"date":0}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	})
}

type fixture struct {
	srv    *server.Server
	st     *store.Store
	tg     *fakeTG
	public http.Handler
	admin  http.Handler
	token  string
	bridge *store.Bridge
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Addr: "127.0.0.1:0", PublicURL: "https://bridge.test",
		DataDir: dir, AdminUser: "admin", AdminPassword: "pw",
		DefaultMode: "webhook", LeaseSeconds: 180, MaxAttempts: 3,
		Secret: bytes.Repeat([]byte{1}, 32),
	}
	box, _ := secret.NewBox(cfg.Secret)
	fake := &fakeTG{}
	ts := httptest.NewServer(fake.handler())
	t.Cleanup(ts.Close)

	factory, err := tg.NewFactory("", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	factory.SetBaseURL(ts.URL)

	queue := domain.NewQueue(st, 180, 3)
	svc := domain.NewService(st, queue, box, factory, cfg.PublicURL)
	srv := server.New(cfg, svc, st)

	now := time.Now().Unix()
	enc, _ := box.Seal("123:AA-test")
	b := &store.Bridge{
		Slug: "demo", Name: "演示桥", BotTokenEnc: enc, BotID: 42, BotUsername: "musebot",
		WebhookSecret: "hook-secret", AllowedUserIDs: "999", Mode: "webhook", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	id, err := st.CreateBridge(t.Context(), b)
	if err != nil {
		t.Fatalf("建桥: %v", err)
	}
	b.ID = id
	token := "mb_test_token"
	tokenEnc, err := box.Seal(token)
	if err != nil {
		t.Fatalf("加密令牌: %v", err)
	}
	if _, err := st.CreateToken(t.Context(), id, "test", secret.HashToken(token), secret.TokenPrefix(token), tokenEnc, now); err != nil {
		t.Fatalf("建令牌: %v", err)
	}

	if err := st.UpsertAdmin(t.Context(), "admin", secret.HashPassword("pw"), now); err != nil {
		t.Fatalf("建管理员: %v", err)
	}

	return &fixture{srv: srv, st: st, tg: fake, public: srv.Public(), admin: srv.Admin(),
		token: token, bridge: b}
}

func (f *fixture) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	return w
}

// adminCookie 登录后台并返回会话 cookie，供需要登录的测试复用。
func (f *fixture) adminCookie(t *testing.T) *http.Cookie {
	t.Helper()
	form := "username=admin&password=pw"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_admin" {
			return c
		}
	}
	t.Fatal("登录未下发会话 cookie")
	return nil
}

func (f *fixture) auth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.token}
}

const sampleUpdate = `{
  "update_id": 1001,
  "message": {
    "message_id": 7,
    "from": {"id": 999, "is_bot": false, "first_name": "家"},
    "chat": {"id": 999, "type": "private"},
    "date": 1763700000,
    "text": "你好，在吗"
  }
}`

func TestWebhookToReplyFlow(t *testing.T) {
	f := newFixture(t)

	// 1. Telegram 投递
	w := f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	if w.Code != 200 {
		t.Fatalf("webhook 返回 %d: %s", w.Code, w.Body)
	}
	var hookRes struct {
		OK     bool  `json:"ok"`
		Queued bool  `json:"queued"`
		MsgID  int64 `json:"message_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &hookRes)
	if !hookRes.OK || !hookRes.Queued || hookRes.MsgID == 0 {
		t.Fatalf("入库结果不对: %s", w.Body)
	}

	// 2. hook 轮询的匿名计数
	w = f.do(t, "GET", "/pending-count", "", nil)
	if !strings.Contains(w.Body.String(), `"pending":1`) {
		t.Fatalf("pending-count 应为 1，得到 %s", w.Body)
	}

	// 3. Muse 认领
	w = f.do(t, "GET", "/poll?limit=5&lease_seconds=180", "", f.auth())
	if w.Code != 200 {
		t.Fatalf("poll 返回 %d: %s", w.Code, w.Body)
	}
	var poll struct {
		OK       bool   `json:"ok"`
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID        int64  `json:"id"`
			MessageID int64  `json:"message_id"`
			ChatID    int64  `json:"chat_id"`
			Text      string `json:"text"`
			LeaseID   string `json:"lease_id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)
	if len(poll.Messages) != 1 {
		t.Fatalf("应认领 1 条，得到 %s", w.Body)
	}
	m := poll.Messages[0]
	if m.Text != "你好，在吗" || m.MessageID != 7 || m.ChatID != 999 {
		t.Fatalf("认领内容不对: %+v", m)
	}
	if m.LeaseID == "" || m.LeaseID != poll.LeaseID {
		t.Fatalf("每条消息应带同一个 lease_id: %+v", m)
	}

	// 4. 重复投递同一 update，不应重复入队
	w = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	var dup struct {
		Queued bool `json:"queued"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &dup)
	if dup.Queued {
		t.Fatalf("重复 update 不应再次入队: %s", w.Body)
	}

	// 5. 回复
	body := fmt.Sprintf(`{"id":%d,"lease_id":%q,"text":"在的，你说"}`, m.ID, poll.LeaseID)
	w = f.do(t, "POST", "/reply", body, f.auth())
	if w.Code != 200 {
		t.Fatalf("reply 返回 %d: %s", w.Code, w.Body)
	}
	if len(f.tg.sent) != 1 || f.tg.sent[0] != "在的，你说" {
		t.Fatalf("Telegram 应收到一条回复，实际 %v", f.tg.sent)
	}

	// 6. 任务完成、出站消息落库
	job, err := f.st.GetJob(t.Context(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "done" {
		t.Fatalf("任务应完成，实际 %s", job.Status)
	}
	items, err := f.st.ExportMessages(t.Context(), f.bridge.ID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out int
	for _, it := range items {
		if it.Direction == "out" {
			out++
			if it.Text != "在的，你说" {
				t.Fatalf("出站记录内容不对: %q", it.Text)
			}
		}
	}
	if out != 1 {
		t.Fatalf("应有 1 条出站记录，实际 %d", out)
	}

	// 7. 再次认领应为空
	w = f.do(t, "GET", "/poll", "", f.auth())
	if !strings.Contains(w.Body.String(), `"messages":[]`) {
		t.Fatalf("队列应已清空: %s", w.Body)
	}
}

func TestWebhookRejectsWrongSecret(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "wrong"})
	if w.Code != 403 {
		t.Fatalf("错误 secret 应返回 403，实际 %d", w.Code)
	}
	if n, _ := f.st.CountJobs(t.Context(), f.bridge.ID, "pending"); n != 0 {
		t.Fatalf("拒绝的请求不应入队，实际 %d", n)
	}
}

func TestMissingSecretHeaderRejected(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "POST", "/tg/demo", sampleUpdate, nil)
	if w.Code != 403 {
		t.Fatalf("缺少 secret 头应返回 403，实际 %d", w.Code)
	}
}

func TestPollRejectsBadToken(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "GET", "/poll", "", map[string]string{"Authorization": "Bearer nope"})
	if w.Code != 401 {
		t.Fatalf("无效令牌应返回 401，实际 %d", w.Code)
	}
}

// 这条测试盯的是 v1 的死锁：租约过期后，即便没有任何人调用 /poll，
// pending-count 也必须把消息重新变成可认领。
func TestPendingCountReapsExpiredLease(t *testing.T) {
	f := newFixture(t)
	if w := f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"}); w.Code != 200 {
		t.Fatalf("webhook 失败: %d", w.Code)
	}
	w := f.do(t, "GET", "/poll", "", f.auth())
	var poll struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)
	if len(poll.Messages) != 1 {
		t.Fatalf("应认领 1 条: %s", w.Body)
	}

	// 把租约改成 200 秒前过期，模拟 agent 崩溃后再也没回来。
	old := time.Now().Unix() - 200
	if _, err := f.st.DB().Exec(`UPDATE jobs SET claimed_at=?, lease_seconds=180 WHERE id=?`,
		old, poll.Messages[0].ID); err != nil {
		t.Fatal(err)
	}

	w = f.do(t, "GET", "/pending-count", "", nil)
	if !strings.Contains(w.Body.String(), `"pending":1`) {
		t.Fatalf("过期租约应被回收成可认领，得到 %s", w.Body)
	}

	// 再认领一次，attempts 必须加一，否则会无限重跑。
	w = f.do(t, "GET", "/poll", "", f.auth())
	if !strings.Contains(w.Body.String(), `"attempts":1`) {
		t.Fatalf("回收后 attempts 应为 1，得到 %s", w.Body)
	}
}

func TestReplyAfterLeaseExpiredConflicts(t *testing.T) {
	f := newFixture(t)
	_ = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	w := f.do(t, "GET", "/poll", "", f.auth())
	var poll struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)

	body := fmt.Sprintf(`{"id":%d,"lease_id":"stale-lease","text":"迟到的回复"}`, poll.Messages[0].ID)
	w = f.do(t, "POST", "/reply", body, f.auth())
	if w.Code != 409 {
		t.Fatalf("过期租约回复应返回 409，实际 %d: %s", w.Code, w.Body)
	}
	if len(f.tg.sent) != 0 {
		t.Fatalf("租约不符不应发出消息，实际 %v", f.tg.sent)
	}
}

func TestDraftThenFinalEditsSameMessage(t *testing.T) {
	f := newFixture(t)
	_ = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	w := f.do(t, "GET", "/poll", "", f.auth())
	var poll struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)
	id := poll.Messages[0].ID

	draft := fmt.Sprintf(`{"id":%d,"lease_id":%q,"text":"思考中…","mode":"draft"}`, id, poll.LeaseID)
	if w := f.do(t, "POST", "/reply", draft, f.auth()); w.Code != 200 {
		t.Fatalf("draft 失败: %d %s", w.Code, w.Body)
	}
	final := fmt.Sprintf(`{"id":%d,"lease_id":%q,"text":"完整回答","mode":"final"}`, id, poll.LeaseID)
	if w := f.do(t, "POST", "/reply", final, f.auth()); w.Code != 200 {
		t.Fatalf("final 失败: %d %s", w.Code, w.Body)
	}
	// 第一次 sendMessage 建占位，第二次应走编辑而不是再发一条。
	if len(f.tg.sent) != 1 {
		t.Fatalf("应只创建一条消息，实际发送 %v", f.tg.sent)
	}
	if len(f.tg.editLog) != 1 || f.tg.editLog[0] != "完整回答" {
		t.Fatalf("final 应编辑占位消息，实际 %v", f.tg.editLog)
	}
}

func TestLongMessageIsSplit(t *testing.T) {
	f := newFixture(t)
	_ = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	w := f.do(t, "GET", "/poll", "", f.auth())
	var poll struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)

	long := strings.Repeat("这是一段很长的回答。\n", 800) // 约 8000 字节
	body := fmt.Sprintf(`{"id":%d,"lease_id":%q,"text":%q}`, poll.Messages[0].ID, poll.LeaseID, long)
	if w := f.do(t, "POST", "/reply", body, f.auth()); w.Code != 200 {
		t.Fatalf("长消息发送失败: %d %s", w.Code, w.Body)
	}
	if len(f.tg.sent) < 3 {
		t.Fatalf("8000 字节应拆成至少 3 条，实际 %d 条", len(f.tg.sent))
	}
}

// 后台页面必须真的渲染出内容。Render 曾经因为 {{template "head"}} 未定义
// 而返回 200 + 空 body，这种错误只有真的读响应体才会发现。
func TestAdminPagesRender(t *testing.T) {
	f := newFixture(t)

	form := "username=admin&password=pw"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.admin.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("登录应 303，实际 %d: %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_admin" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("登录后应下发会话 cookie")
	}

	pages := map[string]string{
		"/":            "总览",
		"/bridges/new": "新建桥",
		"/bridges/1":   "接入信息",
		"/export":      "导出聊天记录",
	}
	for path, marker := range pages {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.admin.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s 应 200，实际 %d: %s", path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), marker) {
			t.Fatalf("%s 内容里缺少 %q，实际前 200 字: %.200s", path, marker, w.Body.String())
		}
	}
}

// 未登录访问后台页面必须跳转，不能泄漏任何数据。
func TestAdminRequiresLogin(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/", "/bridges/new", "/export", "/bridges/1"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		f.admin.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s 未登录应 303，实际 %d", path, w.Code)
		}
	}
}

// 聊天详情页要能渲染出真实消息，包括出站气泡。
func TestConversationPageShowsMessages(t *testing.T) {
	f := newFixture(t)
	_ = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	w := f.do(t, "GET", "/poll", "", f.auth())
	var poll struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &poll)
	body := fmt.Sprintf(`{"id":%d,"lease_id":%q,"text":"这是机器人的回答"}`, poll.Messages[0].ID, poll.LeaseID)
	if w := f.do(t, "POST", "/reply", body, f.auth()); w.Code != 200 {
		t.Fatalf("回复失败: %d %s", w.Code, w.Body)
	}

	conv, err := f.st.ConversationByChat(t.Context(), f.bridge.ID, 999)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", fmt.Sprintf("/conversations/%d", conv.ID), nil)
	// 直接拿会话页需要登录，这里手动构造会话。
	now := time.Now().Unix()
	_ = f.st.CreateSession(t.Context(), secret.HashToken("sess"), now+3600, now)
	req.AddCookie(&http.Cookie{Name: "mb_admin", Value: "sess"})
	rec := httptest.NewRecorder()
	f.admin.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("会话页应 200，实际 %d", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "你好，在吗") || !strings.Contains(out, "这是机器人的回答") {
		t.Fatalf("会话页应同时包含收发消息，实际: %.300s", out)
	}
}

// 后台就在域名根路径上：同一个端口既要给后台页面，也要照常服务 API。
func TestPublicServesAdmin(t *testing.T) {
	f := newFixture(t)

	w := f.do(t, "GET", "/login", "", nil)
	if w.Code != 200 {
		t.Fatalf("公网 /login 应 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "登录") {
		t.Fatalf("登录页内容异常: %.200s", w.Body.String())
	}

	for _, path := range []string{"/", "/export", "/bridges/new"} {
		w := f.do(t, "GET", path, "", nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("公网 %s 未登录应 303，实际 %d", path, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/login" {
			t.Fatalf("应跳登录页，实际 %q", loc)
		}
	}

	// 登录后经公网入口可以正常访问。
	form := "username=admin&password=pw"
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_admin" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("公网登录应下发会话 cookie")
	}
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "总览") {
		t.Fatalf("公网总览页异常: %d %.200s", w.Code, w.Body.String())
	}
}

// 老的 /admin 链接只做跳转：手机书签、早就打开的标签页里还是旧地址，
// 直接 404 会让人以为服务挂了。用 308 而不是 301，
// 表单 POST 跟过去仍然是 POST，不会把"吊销令牌"降级成 GET。
func TestLegacyAdminPathRedirectsToRoot(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{
		"/admin":           "/",
		"/admin/":          "/",
		"/admin/login":     "/login",
		"/admin/bridges/1": "/bridges/1",
		"/admin/export?format=csv&bridge_id=0": "/export?format=csv&bridge_id=0",
	}
	for from, want := range cases {
		w := f.do(t, "GET", from, "", nil)
		if w.Code != http.StatusPermanentRedirect {
			t.Fatalf("%s 应 308，实际 %d", from, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != want {
			t.Fatalf("%s 应跳到 %q，实际 %q", from, want, loc)
		}
	}
}

// 后台搬到根路径之后，API 不能被页面路由抢走。
// {$} 只匹配 "/" 本身，是这条测试守住的约束。
func TestAPIRoutesSurviveAdminAtRoot(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/health", "/pending-count"} {
		if w := f.do(t, "GET", path, "", nil); w.Code != 200 {
			t.Fatalf("%s 应 200，实际 %d: %s", path, w.Code, w.Body)
		}
	}
	if w := f.do(t, "GET", "/poll", "", f.auth()); w.Code != 200 {
		t.Fatalf("/poll 应 200，实际 %d: %s", w.Code, w.Body)
	}
}

// 后台页面必须带上防点击劫持和严格 CSP：它现在对公网开放了。
func TestAdminSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "GET", "/login", "", nil)
	for h, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if got := w.Header().Get(h); got != want {
			t.Fatalf("%s 应为 %q，实际 %q", h, want, got)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src") && !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP 缺失或过松: %q", csp)
	}
}

// 限流必须按 IP 而不是 RemoteAddr 计数。
//
// RemoteAddr 带源端口，公网上每条新连接端口都不同——如果键用了带端口的地址，
// 换一条连接就重置计数，暴力破解完全不受限。这里用同一 IP 不同端口连打 11 次，
// 之后即使密码正确也必须被拒。
func TestLoginRateLimitByIPNotPort(t *testing.T) {
	f := newFixture(t)
	const ip = "198.51.100.77"

	try := func(port, password string) *httptest.ResponseRecorder {
		form := "username=admin&password=" + password
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = ip + ":" + port
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w
	}

	for i := 0; i < 10; i++ {
		w := try(fmt.Sprintf("%d", 40000+i), "wrong")
		if strings.Contains(w.Body.String(), "尝试次数过多") {
			t.Fatalf("第 %d 次就触发限流，阈值不对", i+1)
		}
	}

	// 第 11 次：换端口、并且密码是对的，也必须被挡。
	w := try("50000", "pw")
	if !strings.Contains(w.Body.String(), "尝试次数过多") {
		t.Fatalf("同 IP 换端口应仍受限流约束，实际: %.200s", w.Body.String())
	}
}

// 删除必须走服务端确认页：后台 CSP 禁用了 script-src，
// onsubmit="return confirm(...)" 会被浏览器直接丢弃，删除会变成一击即中。
func TestDeleteConfirmPageRenders(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/bridges/1/delete", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("确认页应 200，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "确认永久删除") || !strings.Contains(body, "不可恢复") {
		t.Fatalf("确认页内容异常: %.300s", body)
	}
	if strings.Contains(body, "onsubmit") {
		t.Fatal("确认页不应依赖内联事件处理器")
	}
}

// 模板里不能出现内联脚本或内联事件处理器，脚本只能引用自带的 /static。
//
// 后台的 CSP 是 script-src 'self'，内联 <script> 和 on*="..." 都会被
// 浏览器静默丢弃——功能不报错，只是莫名其妙不生效。这条测试守住这个约束。
// 唯一允许的写法是 <script src="/static/..."></script>（复制按钮用），
// 而且标签体必须为空，防止有人顺手把代码塞进去。
func TestTemplatesHaveNoInlineScript(t *testing.T) {
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	emptyScript := regexp.MustCompile(`(?is)<script[^>]*>\s*</script>`)
	scriptTag := regexp.MustCompile(`(?is)<script[^>]*>`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*["']`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("templates", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		if loc := handler.FindIndex(data); loc != nil {
			t.Fatalf("%s 含有内联事件处理器（偏移 %d），会被 CSP 挡掉: %q",
				e.Name(), loc[0], s[max(0, loc[0]-40):loc[0]+60])
		}
		// 先把"空标签 + src"这种合法写法抹掉，剩下的 <script 一律非法。
		rest := emptyScript.ReplaceAllString(s, "")
		if loc := strings.Index(strings.ToLower(rest), "<script"); loc >= 0 {
			t.Fatalf("%s 含有内联脚本（偏移 %d），会被 CSP 挡掉: %q",
				e.Name(), loc, rest[max(0, loc-40):loc+60])
		}
		for _, tag := range scriptTag.FindAllString(s, -1) {
			if !strings.Contains(tag, `src="/static/`) {
				t.Fatalf("%s 的脚本只能来自 /static，实际: %s", e.Name(), tag)
			}
		}
	}
}

// 每张表格都必须包在 .tw 滚动容器里。
//
// 手机上列一多就会把表格撑出卡片，整页跟着横向错位——这正是
// "手机端格式错乱"最典型的样子。这条测试防止以后新增表格忘了包。
func TestTablesWrappedForMobile(t *testing.T) {
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("templates", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		tables := strings.Count(s, "<table")
		wraps := strings.Count(s, `<div class="tw">`)
		if tables > wraps {
			t.Fatalf("%s 有 %d 张表但只有 %d 个 .tw 容器", e.Name(), tables, wraps)
		}
	}
}

// 消息被认领的那一刻就要开始在 Telegram 显示"输入中"，
// 而不是等 agent 想完、调 /reply 的时候才发——那时候回复都已经发出去了。
func TestPollStartsTypingImmediately(t *testing.T) {
	f := newFixture(t)
	_ = f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})

	if got := f.tg.count("sendChatAction"); got != 0 {
		t.Fatalf("还没认领就不该发 typing，实际 %d 次", got)
	}

	w := f.do(t, "GET", "/poll", "", f.auth())
	if w.Code != 200 {
		t.Fatalf("认领失败: %d %s", w.Code, w.Body)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.tg.count("sendChatAction") >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("认领后应立即发送 sendChatAction(typing)")
}

// 名单之外的人发消息：要回 200（否则 Telegram 会一直重投），
// 但不入库、不排队，后台只留一条内存提示。
func TestWebhookDropsUnknownSender(t *testing.T) {
	f := newFixture(t)
	const stranger = `{
  "update_id": 2001,
  "message": {
    "message_id": 8,
    "from": {"id": 4242, "is_bot": false, "first_name": "路人"},
    "chat": {"id": 4242, "type": "private"},
    "date": 1763700000,
    "text": "帮我写个脚本"
  }
}`
	w := f.do(t, "POST", "/tg/demo", stranger,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	if w.Code != 200 {
		t.Fatalf("被拒消息也要回 200，实际 %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"reason":"user_not_allowed"`) {
		t.Fatalf("应说明拒绝原因，实际 %s", w.Body)
	}
	w = f.do(t, "GET", "/pending-count", "", nil)
	if !strings.Contains(w.Body.String(), `"pending":0`) {
		t.Fatalf("陌生人消息不该排队，实际 %s", w.Body)
	}
	if convs, _ := f.st.ListConversations(t.Context(), f.bridge.ID, 10); len(convs) != 0 {
		t.Fatalf("陌生人消息不该建会话，实际 %d 个", len(convs))
	}

	// 后台要能看见"有人被挡了"和对方的 ID，否则填错 ID 时毫无线索。
	req := httptest.NewRequest("GET", "/bridges/1", nil)
	req.AddCookie(f.adminCookie(t))
	rec := httptest.NewRecorder()
	f.public.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "刚拦下一条消息") || !strings.Contains(body, "4242") {
		t.Fatalf("详情页应提示被拒的发送者，实际: %.400s", body)
	}
}

// 空名单 = 等待认领：第一个发消息的人被自动绑定，之后就只认这个人。
func TestEmptyAllowListBindsFirstSender(t *testing.T) {
	f := newFixture(t)
	if err := f.st.UpdateBridgeSettings(t.Context(), f.bridge.ID, f.bridge.Name, "", f.bridge.Mode, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	w := f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"queued":true`) {
		t.Fatalf("首个发送者应被接受，实际 %d %s", w.Code, w.Body)
	}
	b, err := f.st.GetBridgeByID(t.Context(), f.bridge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedUserIDs != "999" {
		t.Fatalf("应绑定首个发送者 999，实际 %q", b.AllowedUserIDs)
	}

	// 绑定完成后，换个人来就不行了，而且不入库。
	const other = `{
  "update_id": 2002,
  "message": {
    "message_id": 9,
    "from": {"id": 1000, "is_bot": false, "first_name": "别人"},
    "chat": {"id": 1000, "type": "private"},
    "date": 1763700000,
    "text": "我也要用"
  }
}`
	w = f.do(t, "POST", "/tg/demo", other,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	if !strings.Contains(w.Body.String(), `"reason":"user_not_allowed"`) {
		t.Fatalf("绑定后的其他人应被拒，实际 %s", w.Body)
	}
	if convs, _ := f.st.ListConversations(t.Context(), f.bridge.ID, 10); len(convs) != 1 {
		t.Fatalf("只应有首个发送者的会话，实际 %d 个", len(convs))
	}
}

// 群里的非 @ 闲聊不能用来认领桥：否则谁先在群里说句话，桥就归谁了。
func TestEmptyAllowListIgnoresGroupChatter(t *testing.T) {
	f := newFixture(t)
	if err := f.st.UpdateBridgeSettings(t.Context(), f.bridge.ID, f.bridge.Name, "", f.bridge.Mode, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	const chatter = `{
  "update_id": 2003,
  "message": {
    "message_id": 10,
    "from": {"id": 1000, "is_bot": false, "first_name": "路人"},
    "chat": {"id": -100123, "type": "supergroup", "title": "群"},
    "date": 1763700000,
    "text": "今天天气不错"
  }
}`
	w := f.do(t, "POST", "/tg/demo", chatter,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"})
	if w.Code != 200 {
		t.Fatalf("应回 200，实际 %d", w.Code)
	}
	b, err := f.st.GetBridgeByID(t.Context(), f.bridge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedUserIDs != "" {
		t.Fatalf("群闲聊不该完成绑定，实际 %q", b.AllowedUserIDs)
	}
}

// 令牌以前只在签发那一刻显示，关掉就再也拿不到明文。
// 现在详情页每次都要能复制，否则换个设备就没法把 Muse 接回来。
func TestTokenCopyableAnyTime(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/bridges/1", nil)
		req.AddCookie(f.adminCookie(t))
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `data-copy="token"`) {
			t.Fatalf("第 %d 次打开缺少复制令牌按钮: %.300s", i+1, body)
		}
		if !strings.Contains(body, "mb_test_token") {
			t.Fatalf("第 %d 次打开应看到令牌明文: %.300s", i+1, body)
		}
	}
}

// 提示词要能独立转发：必须有桥地址，但绝不能带上令牌。
func TestOnboardPromptHasURLAndNoToken(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/bridges/1", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)

	m := regexp.MustCompile(`(?s)<textarea[^>]*id="prompt"[^>]*>(.*?)</textarea>`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("详情页应有一段可复制的提示词: %.300s", w.Body.String())
	}
	prompt := m[1]
	for _, want := range []string{"https://bridge.test", "demo", "@musebot", "pending-count", "poll"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("提示词应包含 %q，实际:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "mb_test_token") {
		t.Fatal("提示词里不能出现令牌：它会被整段转发出去")
	}
	if !strings.Contains(w.Body.String(), `data-copy="prompt"`) {
		t.Fatal("缺复制提示词的按钮")
	}
}

// 签发新令牌走 303 回到详情页，并且密文要落库，否则下次又复制不出来。
func TestIssueNewTokenStoresCipherAndRedirects(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("POST", "/bridges/1/tokens", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("签发后应 303，实际 %d", w.Code)
	}
	tokens, err := f.st.ListTokens(t.Context(), f.bridge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("应有 2 枚令牌，实际 %d", len(tokens))
	}
	if tokens[0].TokenEnc == "" {
		t.Fatal("新令牌必须存密文，否则下次打开又只有前缀")
	}
}

// 首页的桥列表只留名字，仍然是链接。
func TestDashboardBridgeListOnlyNames(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/bridges/1"`) || !strings.Contains(body, "演示桥") {
		t.Fatalf("桥名应仍然可点: %.300s", body)
	}
	for _, unwanted := range []string{"运行中", "@musebot", "webhook", "待 Muse 处理"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("桥列表只留名字，不该再出现 %q", unwanted)
		}
	}
	if !strings.Contains(body, ">待处理<") {
		t.Fatal("统计卡应写「待处理」")
	}
}

// 复制按钮依赖的脚本必须真的能取到，CSP 也只放开同源脚本。
func TestCopyScriptServed(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, "GET", "/static/copy.js", "", nil)
	if w.Code != 200 {
		t.Fatalf("copy.js 应 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "clipboard") {
		t.Fatal("脚本内容不对")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP 应只允许同源脚本，实际 %q", csp)
	}
}

// 建桥后必须 303 跳到详情页。
//
// 以前是把详情页直接渲染在 POST 的响应上，刷新一次就重新提交一次，
// 第二次只会得到「slug 已被占用」。现在令牌有密文了，跳过去照样能显示。
func TestCreateBridgeRedirectsToDetail(t *testing.T) {
	f := newFixture(t)
	form := "name=新桥&slug=fresh&bot_token=123:AA-new&allowed_user_ids=&mode=webhook"
	req := httptest.NewRequest("POST", "/bridges", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("建桥后应 303，实际 %d: %s", w.Code, w.Body)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/bridges/") || !strings.HasSuffix(loc, "?created=1") {
		t.Fatalf("应跳到新桥详情页，实际 %q", loc)
	}

	req2 := httptest.NewRequest("GET", loc, nil)
	req2.AddCookie(f.adminCookie(t))
	w2 := httptest.NewRecorder()
	f.public.ServeHTTP(w2, req2)
	body := w2.Body.String()
	if !strings.Contains(body, "桥已创建") {
		t.Fatalf("刚建好的详情页应点明已创建: %.300s", body)
	}
	if !strings.Contains(body, `data-copy="token"`) || !strings.Contains(body, `data-copy="prompt"`) {
		t.Fatal("新桥详情页应同时能复制提示词和令牌")
	}

	// 令牌密文必须落库，否则这一页刷新之后又没得复制了。
	var id int64
	if err := f.st.DB().QueryRowContext(t.Context(),
		`SELECT id FROM bridges WHERE slug='fresh'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tokens, err := f.st.ListTokens(t.Context(), id)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("应有一枚令牌，实际 %d: %v", len(tokens), err)
	}
	if tokens[0].TokenEnc == "" {
		t.Fatal("新建桥的令牌应存密文")
	}
}
