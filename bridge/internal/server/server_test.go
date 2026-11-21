package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		WebhookSecret: "hook-secret", Mode: "webhook", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	id, err := st.CreateBridge(t.Context(), b)
	if err != nil {
		t.Fatalf("建桥: %v", err)
	}
	b.ID = id
	token := "mb_test_token"
	if _, err := st.CreateToken(t.Context(), id, "test", secret.HashToken(token), secret.TokenPrefix(token), now); err != nil {
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
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form))
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
		"/admin":               "总览",
		"/admin/bridges/new":   "新建桥",
		"/admin/bridges/1":     "接入信息",
		"/admin/export":        "导出聊天记录",
		"/admin/conversations": "",
	}
	for path, marker := range pages {
		if marker == "" {
			continue
		}
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
	for _, path := range []string{"/admin", "/admin/bridges/new", "/admin/export", "/admin/bridges/1"} {
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
	req := httptest.NewRequest("GET", fmt.Sprintf("/admin/conversations/%d", conv.ID), nil)
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
