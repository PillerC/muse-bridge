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

// 匿名 /pending-count 是所有 active 桥的合计：任何一座桥有活，装了 hook 的
// agent 全都会醒。带 ?bridge=<slug> 只算这一座，各桥的 hook 才不会互相惊醒。
func TestPendingCountScopedBySlug(t *testing.T) {
	f := newFixture(t)

	// 第二座桥，slug 和 secret 都与 fixture 里那座不同。
	now := time.Now().Unix()
	other := &store.Bridge{
		Slug: "other", Name: "第二座", BotTokenEnc: f.bridge.BotTokenEnc,
		BotID: 43, BotUsername: "otherbot",
		WebhookSecret: "hook-secret-2", Mode: "webhook", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := f.st.CreateBridge(t.Context(), other); err != nil {
		t.Fatalf("建第二座桥: %v", err)
	}

	// demo 两座各投一条，other 投一条（同一个 update_id 不算重复：
	// 唯一索引是 (bridge_id, update_id)）。
	second := strings.Replace(sampleUpdate, `"update_id": 1001`, `"update_id": 1002`, 1)
	if w := f.do(t, "POST", "/tg/demo", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"}); w.Code != 200 {
		t.Fatalf("demo 投递失败 %d: %s", w.Code, w.Body)
	}
	if w := f.do(t, "POST", "/tg/demo", second,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret"}); w.Code != 200 {
		t.Fatalf("demo 第二次投递失败 %d: %s", w.Code, w.Body)
	}
	if w := f.do(t, "POST", "/tg/other", sampleUpdate,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "hook-secret-2"}); w.Code != 200 {
		t.Fatalf("other 投递失败 %d: %s", w.Code, w.Body)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/pending-count", 3},
		{"/pending-count?bridge=demo", 2},
		{"/pending-count?bridge=other", 1},
	} {
		w := f.do(t, "GET", tc.path, "", nil)
		if w.Code != 200 {
			t.Fatalf("%s 返回 %d: %s", tc.path, w.Code, w.Body)
		}
		var res struct {
			Pending int `json:"pending"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%s 解析失败: %v", tc.path, err)
		}
		if res.Pending != tc.want {
			t.Fatalf("%s 应为 %d，实际 %d", tc.path, tc.want, res.Pending)
		}
	}

	// slug 写错要 404：hook 守着永远为 0 的数字，比报错更难发现。
	if w := f.do(t, "GET", "/pending-count?bridge=nope", "", nil); w.Code != 404 {
		t.Fatalf("未知 slug 应 404，实际 %d", w.Code)
	}

	// 令牌优先于 slug：带着 demo 的令牌却写了 other 的 slug，仍按令牌算 2。
	w := f.do(t, "GET", "/pending-count?bridge=other", "", f.auth())
	if !strings.Contains(w.Body.String(), `"pending":2`) {
		t.Fatalf("令牌应优先于 slug，实际 %s", w.Body)
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

// 早期提示词里写的是 {"job_id":...}，现在只剩一座桥、直接重建即可，
// 不再为这个拼写留别名：写错就该 400，而不是含糊地查一个 id=0 的任务。
func TestReplyRejectsLegacyJobIDField(t *testing.T) {
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

	body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"text":"旧写法"}`, poll.Messages[0].ID, poll.LeaseID)
	if w := f.do(t, "POST", "/reply", body, f.auth()); w.Code != 400 {
		t.Fatalf("job_id 应被拒绝并返回 400，实际 %d %s", w.Code, w.Body)
	}
	if len(f.tg.sent) != 0 {
		t.Fatalf("字段名错误不应发出消息，实际 %v", f.tg.sent)
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
		"/bridges/1":   "接入 Muse",
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

// 后台搬到根路径后，旧的 /admin/* 已彻底移除，不再做跳转：
// 这些路径现在应该干净地 404，而不是把人送到某个残留页面上。
func TestLegacyAdminPathGone(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/admin",
		"/admin/",
		"/admin/login",
		"/admin/bridges/1",
		"/admin/export?format=csv&bridge_id=0",
	} {
		w := f.do(t, "GET", path, "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s 应 404，实际 %d", path, w.Code)
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
	if !strings.Contains(body, "确认删除") || !strings.Contains(body, "不可恢复") {
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
	// 提示词是用户定稿的那一份：要能独立转发，所以地址在、凭据不在。
	for _, want := range []string{
		"https://bridge.test", "api_hosts", "bearer_header",
		"/pending-count?bridge=demo", "/poll?limit=5", "/reply", "/stats", "muse-bridge.py",
		// 唤醒靠 hook：每 5 秒查一次公开端点，名字要和仓库里的样例一致。
		"muse-bridge-pending", "每 5 秒",
		// reply 的形状要和真实接口一致：id 就叫 id，mode 只有 draft / final。
		// 引号在 textarea 里是 HTML 实体，复制出去浏览器会还原。
		`&#34;id&#34;:1,&#34;lease_id&#34;`, `&#34;mode&#34;:&#34;final&#34;`, "draft",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("提示词应包含 %q，实际:\n%s", want, prompt)
		}
	}
	// Muse 照抄过这个字段名，写进提示词就会一路错到底。
	if strings.Contains(prompt, "job_id") {
		t.Fatal("提示词里任务 id 的字段名是 id，不能写成 job_id")
	}
	if strings.Contains(prompt, "mb_test_token") {
		t.Fatal("提示词里不能出现令牌：它会被整段转发出去")
	}
	if !strings.Contains(w.Body.String(), `data-copy="prompt"`) {
		t.Fatal("缺复制提示词的按钮")
	}
}

// 轮转令牌：发新的、废旧的，顺序不能反——中间不能有一刻是零令牌，
// 否则正在轮询的 Muse 会吃到 401 然后停下。
func TestRotateTokenReplacesOld(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("POST", "/bridges/1/tokens/rotate", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("轮转后应 303，实际 %d", w.Code)
	}
	tokens, err := f.st.ListTokens(t.Context(), f.bridge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("应有 2 枚令牌（新旧各一），实际 %d", len(tokens))
	}
	fresh, old := tokens[0], tokens[1]
	if fresh.TokenEnc == "" {
		t.Fatal("新令牌必须存密文，否则下次打开又只有前缀")
	}
	if fresh.RevokedAt != 0 {
		t.Fatal("新令牌不该是已吊销状态")
	}
	if old.RevokedAt == 0 {
		t.Fatal("轮转必须把旧令牌作废")
	}

	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32))
	plain, err := box.Open(fresh.TokenEnc)
	if err != nil {
		t.Fatal(err)
	}
	if plain == "mb_test_token" {
		t.Fatal("轮转不能只是把旧令牌原样复制一份")
	}
	if w := f.do(t, "GET", "/stats", "", map[string]string{"Authorization": "Bearer " + plain}); w.Code != http.StatusOK {
		t.Fatalf("新令牌应可用，实际 %d: %s", w.Code, w.Body)
	}
	if w := f.do(t, "GET", "/stats", "", map[string]string{"Authorization": "Bearer mb_test_token"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("旧令牌应立刻失效（401），实际 %d", w.Code)
	}
}

// 详情页的版式是用户定的：删掉「接入信息」「令牌」表、「删除桥」卡，
// 暂停和删除并排放进最底部的设置卡，令牌那块只留「复制令牌 + 轮转令牌」。
func TestBridgeDetailLayout(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/bridges/1", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	body := w.Body.String()

	for _, gone := range []string{"接入信息", "重新注册 webhook", "断开 webhook", "永久删除", "暂停这座桥", "<h2>令牌</h2>"} {
		if strings.Contains(body, gone) {
			t.Fatalf("详情页不该再出现 %q", gone)
		}
	}
	for _, want := range []string{"轮转令牌", "复制令牌", ">暂停<", ">删除<", "设置"} {
		if !strings.Contains(body, want) {
			t.Fatalf("详情页应包含 %q", want)
		}
	}
	// 设置卡在最底部：会话、死信之后；暂停与删除在同一行里，删除紧跟暂停。
	if strings.Index(body, "设置") < strings.Index(body, "<h2>会话</h2>") {
		t.Fatal("设置卡应在会话之后（最底部）")
	}
	pause := strings.Index(body, ">暂停<")
	del := strings.Index(body, ">删除<")
	if pause < 0 || del < 0 || del < pause || del-pause > 400 {
		t.Fatalf("删除应紧挨着暂停，实际位置 pause=%d delete=%d", pause, del)
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
	// slug 不再由用户填：表单里没有这个字段，服务端随机生成。
	form := "name=新桥&bot_token=123:AA-new&allowed_user_ids=&mode=webhook"
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
	var slug string
	if err := f.st.DB().QueryRowContext(t.Context(),
		`SELECT id, slug FROM bridges WHERE name='新桥' ORDER BY id DESC LIMIT 1`).Scan(&id, &slug); err != nil {
		t.Fatal(err)
	}
	// slug 可以出现在 URL 里，所以必须是安全字符；长度固定便于辨认。
	if !regexp.MustCompile(`^[a-z0-9]{10}$`).MatchString(slug) {
		t.Fatalf("自动生成的 slug 不合法：%q", slug)
	}
	tokens, err := f.st.ListTokens(t.Context(), id)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("应有一枚令牌，实际 %d: %v", len(tokens), err)
	}
	if tokens[0].TokenEnc == "" {
		t.Fatal("新建桥的令牌应存密文")
	}
}

// 页脚那句宣传语已经从所有页面删掉了；提示词默认折叠，展开按钮用原生 details。
func TestNoFooterAndFoldedPrompt(t *testing.T) {
	f := newFixture(t)
	cookie := f.adminCookie(t)
	for _, path := range []string{"/", "/bridges/new", "/bridges/1", "/export", "/account"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s 应 200，实际 %d", path, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, "消息双向落库") {
			t.Fatalf("%s 仍然带着页脚宣传语", path)
		}
		if strings.Contains(body, "<footer") {
			t.Fatalf("%s 仍然渲染了 footer 元素", path)
		}
	}

	req := httptest.NewRequest("GET", "/bridges/1", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `class="fold"`) || !strings.Contains(w.Body.String(), "<summary>") {
		t.Fatal("提示词应包在默认折叠的 details 里，并带展开按钮")
	}
	// 折叠只影响显示，复制按钮仍然要能取到 textarea 的值。
	if !strings.Contains(w.Body.String(), `data-copy="prompt"`) {
		t.Fatal("折叠后仍要保留复制提示词的按钮")
	}
}

// 新建桥不再让人填 slug，也不再显示那两段解释。
func TestNewBridgeFormHasNoSlugField(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/bridges/new", nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	body := w.Body.String()
	for _, gone := range []string{`name="slug"`, "只能小写字母、数字和连字符", "第一个给机器人发消息的人会自动成为唯一允许的人", "创建时会调用"} {
		if strings.Contains(body, gone) {
			t.Fatalf("新建页不该再出现 %q", gone)
		}
	}
	if !strings.Contains(body, "私聊 @userinfobot 获取ID") {
		t.Fatal("允许列表的说明应写明去哪里拿 ID")
	}
}

// 账号页：改用户名、改密码都走这里，改密码要先验当前密码。
func TestAccountChangeUsernameAndPassword(t *testing.T) {
	f := newFixture(t)
	cookie := f.adminCookie(t)

	post := func(form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/account", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w
	}
	login := func(user, pass string) int {
		req := httptest.NewRequest("POST", "/login",
			strings.NewReader("username="+user+"&password="+pass))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w.Code
	}

	// 当前密码不对：什么都不改。
	if w := post("current_password=wrong&username=hacker"); w.Code != http.StatusSeeOther {
		t.Fatalf("应 303 回账号页，实际 %d", w.Code)
	}
	if login("admin", "pw") != http.StatusSeeOther {
		t.Fatal("密码验错时不该改动账号")
	}

	// 只改用户名。
	if w := post("current_password=pw&username=alice"); w.Code != http.StatusSeeOther {
		t.Fatalf("改名后应 303，实际 %d", w.Code)
	}
	if login("alice", "pw") != http.StatusSeeOther {
		t.Fatal("改名后应能用新用户名登录")
	}
	if login("admin", "pw") != http.StatusOK { // 200 = 停在登录页，没有 cookie
		t.Fatal("旧用户名不该还能登录")
	}

	// 密码太短要被挡回来。
	if w := post("current_password=pw&username=alice&password=short&password2=short"); w.Code != http.StatusSeeOther {
		t.Fatalf("应 303，实际 %d", w.Code)
	}
	if login("alice", "pw") != http.StatusSeeOther {
		t.Fatal("密码没改成时旧密码应继续有效")
	}

	// 两次输入不一致同样挡回来。
	post("current_password=pw&username=alice&password=abcdefgh&password2=abcdefgi")
	if login("alice", "pw") != http.StatusSeeOther {
		t.Fatal("两次不一致时不该改密码")
	}

	// 正常改密码：旧会话全部作废，跳登录页。
	if w := post("current_password=pw&username=alice&password=newpass123&password2=newpass123"); w.Code != http.StatusSeeOther {
		t.Fatalf("改密码后应 303，实际 %d", w.Code)
	}
	var sessions int
	if err := f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("改密码后应清空所有会话，实际还剩 %d 个", sessions)
	}
	if login("alice", "newpass123") != http.StatusSeeOther {
		t.Fatal("应能用新密码登录")
	}
	if login("alice", "pw") != http.StatusOK {
		t.Fatal("旧密码应失效")
	}
}

// 全新实例的首次访问：登录页不存在，直接去初始化页建第一个管理员。
func TestFirstRunSetupFlow(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.DB().ExecContext(t.Context(), `DELETE FROM admins`); err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w
	}
	post := func(form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/setup", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w
	}

	// 没有账号时，未登录访问首页 → /login，而 /login 又把人送回 /setup。
	if w := get("/"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("未登录访问首页应 303 /login，实际 %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("/login"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/setup" {
		t.Fatalf("没有账号时 /login 应 303 /setup，实际 %d %q", w.Code, w.Header().Get("Location"))
	}
	w := get("/setup")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "创建管理员") {
		t.Fatalf("/setup 应 200 并显示创建表单，实际 %d", w.Code)
	}

	// 校验拦住：用户名太短、密码太短、两次不一致，都不该建号。
	for _, form := range []string{
		"username=ab&password=abcdefgh&password2=abcdefgh",
		"username=alice&password=short&password2=short",
		"username=alice&password=abcdefgh&password2=abcdefgi",
	} {
		if w := post(form); w.Code != http.StatusOK {
			t.Fatalf("%s 应留在表单页（200），实际 %d", form, w.Code)
		}
	}
	if n, err := f.st.CountAdmins(t.Context()); err != nil || n != 0 {
		t.Fatalf("校验失败时不该建号，实际 %d 个（err=%v）", n, err)
	}

	// 正常创建：303 进后台，并且当场登进去（带会话 cookie）。
	w = post("username=alice&password=abcdefgh&password2=abcdefgh")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("创建成功应 303 /，实际 %d %q", w.Code, w.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_admin" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("创建成功后应直接下发会话 cookie")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	dash := httptest.NewRecorder()
	f.public.ServeHTTP(dash, req)
	if dash.Code != http.StatusOK {
		t.Fatalf("带会话访问首页应 200，实际 %d", dash.Code)
	}

	// 有账号之后 /setup 关掉，再打开就是回登录页；用新账号能登录。
	if w := get("/setup"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("有账号后 /setup 应 303 /login，实际 %d %q", w.Code, w.Header().Get("Location"))
	}
	login := func(u, p string) int {
		req := httptest.NewRequest("POST", "/login",
			strings.NewReader("username="+u+"&password="+p))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.public.ServeHTTP(w, req)
		return w.Code
	}
	if login("alice", "abcdefgh") != http.StatusSeeOther {
		t.Fatal("应能用刚创建的账号密码登录")
	}
	if login("alice", "wrongpass") != http.StatusOK {
		t.Fatal("错误密码不该登录成功")
	}
}
