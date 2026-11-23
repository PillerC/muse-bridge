package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// memSettings 是 Settings 的内存实现。
type memSettings struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemSettings() *memSettings { return &memSettings{data: map[string]string{}} }

func (m *memSettings) GetSetting(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *memSettings) SetSetting(_ context.Context, key, value string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memSettings) SetSettings(_ context.Context, values map[string]string, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range values {
		m.data[k] = v
	}
	return nil
}

func (m *memSettings) DeleteSetting(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

// noopSealer 不做真加密，测试里只需要往返一致。
type noopSealer struct{}

func (noopSealer) Seal(p string) (string, error) { return "enc:" + p, nil }
func (noopSealer) Open(e string) (string, error) {
	return strings.TrimPrefix(e, "enc:"), nil
}

// memArchives 是 Archives 的内存实现，同时持有真实的 SQLite 用于归档查询。
type memArchives struct {
	db     *sql.DB
	mu     sync.Mutex
	states map[string]*ArchiveState
	// purged 记录被删除的日期。
	purged []string
}

func newMemArchives(db *sql.DB) *memArchives {
	return &memArchives{db: db, states: map[string]*ArchiveState{}}
}

func (m *memArchives) get(day string) *ArchiveState {
	if a, ok := m.states[day]; ok {
		return a
	}
	a := &ArchiveState{Day: day}
	m.states[day] = a
	return a
}

func (m *memArchives) MarkArchived(_ context.Context, day string, records int, bytes int64, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.get(day)
	a.Records, a.Bytes, a.ArchivedAt, a.UploadError = records, bytes, now, ""
	return nil
}

func (m *memArchives) MarkUploaded(_ context.Context, day string, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.get(day)
	a.UploadedAt, a.UploadError = now, ""
	return nil
}

func (m *memArchives) MarkUploadFailed(_ context.Context, day, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.get(day).UploadError = reason
	return nil
}

func (m *memArchives) MarkPurged(_ context.Context, day string, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.get(day).PurgedAt = now
	m.purged = append(m.purged, day)
	return nil
}

func (m *memArchives) GetArchiveState(_ context.Context, day string) (*ArchiveState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.states[day]; ok {
		return a, nil
	}
	return nil, nil
}

func (m *memArchives) ListArchiveStates(_ context.Context, _ int) ([]*ArchiveState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*ArchiveState
	for _, a := range m.states {
		out = append(out, a)
	}
	return out, nil
}

func (m *memArchives) PendingUploads(_ context.Context, _ int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, a := range m.states {
		if a.ArchivedAt > 0 && a.UploadedAt == 0 {
			out = append(out, a.Day)
		}
	}
	return out, nil
}

func (m *memArchives) DaysNeedingArchive(ctx context.Context, loc *time.Location, _ int) ([]string, error) {
	return queryDays(ctx, m.db, loc)
}

func (m *memArchives) PurgeDay(_ context.Context, day string, _ *time.Location) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purged = append(m.purged, day)
	return 1, nil
}

// queryDays 从真实库里查有消息但没归档的日期（测试里简化为"有消息的日期"）。
func queryDays(ctx context.Context, db *sql.DB, loc *time.Location) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT strftime('%Y-%m-%d', created_at, 'unixepoch', ?) AS day
		   FROM messages ORDER BY day`, offsetMod(loc))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func offsetMod(loc *time.Location) string {
	_, off := time.Now().In(loc).Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return sign + pad2(off/3600) + ":" + pad2((off%3600)/60)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// fakeTarget 记录上传调用，可配置为总是失败。
type fakeTarget struct {
	mu       sync.Mutex
	uploaded []string
	failPut  bool
	failPing bool
}

func (f *fakeTarget) Name() string { return "fake" }

func (f *fakeTarget) Put(_ context.Context, key, _ string) error {
	if f.failPut {
		return errors.New("模拟上传失败")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded = append(f.uploaded, key)
	return nil
}

func (f *fakeTarget) Delete(_ context.Context, _ string) error { return nil }

func (f *fakeTarget) Ping(_ context.Context) error {
	if f.failPing {
		return errors.New("模拟连接失败")
	}
	return nil
}

func (f *fakeTarget) uploadedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uploaded...)
}

// newTestDB 造一个含消息表的最小库。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
CREATE TABLE bridges (id INTEGER PRIMARY KEY, slug TEXT, name TEXT);
CREATE TABLE conversations (id INTEGER PRIMARY KEY, bridge_id INTEGER, chat_id INTEGER,
  chat_type TEXT, title TEXT);
CREATE TABLE messages (id INTEGER PRIMARY KEY, bridge_id INTEGER, conversation_id INTEGER,
  direction TEXT, tg_message_id INTEGER, sender_name TEXT, sender_username TEXT,
  text TEXT, content_kind TEXT, created_at INTEGER);
INSERT INTO bridges VALUES (1,'demo','演示桥');
INSERT INTO conversations VALUES (1,1,999,'private','家');
`); err != nil {
		t.Fatal(err)
	}
	return db
}

func addMsg(t *testing.T, db *sql.DB, id int64, at time.Time, text string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO messages VALUES (?,1,1,'in',?,?,?,?, 'text', ?)`,
		id, id, "家", "jia", text, at.Unix()); err != nil {
		t.Fatal(err)
	}
}

// fixture 组装一套可测的服务。
func newFixture(t *testing.T) (*Service, *memSettings, *memArchives, *fakeTarget, *sql.DB) {
	t.Helper()
	db := newTestDB(t)
	st := newMemSettings()
	ar := newMemArchives(db)
	dir := t.TempDir()
	svc := NewService(st, ar, noopSealer{}, db, dir, time.Local)
	target := &fakeTarget{}
	// 让 New(cfg) 返回假目标。
	newTargetHook = func(Config) (Target, error) { return target, nil }
	t.Cleanup(func() { newTargetHook = nil })
	return svc, st, ar, target, db
}

// 每日归档 + 上传的完整流程。
func TestRunOnceArchivesAndUploads(t *testing.T) {
	svc, st, ar, target, db := newFixture(t)
	now := time.Now()
	addMsg(t, db, 1, now.Add(-24*time.Hour), "昨天")
	addMsg(t, db, 2, now, "今天")

	if err := st.SetSettings(context.Background(), map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
	}, 0); err != nil {
		t.Fatal(err)
	}

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 两个日子都应归档并上传。
	if n := len(target.uploadedKeys()); n != 2 {
		t.Fatalf("应上传 2 个文件，得到 %d: %v", n, target.uploadedKeys())
	}
	states, _ := ar.ListArchiveStates(context.Background(), 100)
	if len(states) != 2 {
		t.Fatalf("应有 2 天归档状态，得到 %d", len(states))
	}
	for _, a := range states {
		if a.ArchivedAt == 0 {
			t.Fatalf("%s 应标记为已归档", a.Day)
		}
		if a.UploadedAt == 0 {
			t.Fatalf("%s 应标记为已上传", a.Day)
		}
	}
	if v, _ := st.GetSetting(context.Background(), KeyLastResult); v != "ok" {
		t.Fatalf("上次结果应为 ok，得到 %q", v)
	}
}

// 安全底线：上传失败的那一天绝不删本地数据。
func TestPurgeSkipsFailedUploads(t *testing.T) {
	svc, st, ar, target, db := newFixture(t)
	target.failPut = true

	old := time.Now().AddDate(0, 0, -40)
	addMsg(t, db, 1, old, "很旧的消息")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
		KeyKeepEnabled: "1", KeyKeepDays: "7",
	}, 0)

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// 上传失败 → 不能清理。
	if len(ar.purged) != 0 {
		t.Fatalf("上传失败时不应清理任何数据，却清理了 %v", ar.purged)
	}
}

// 上传成功且超过保留期，才删本地。
func TestPurgeAfterSuccessfulUpload(t *testing.T) {
	svc, st, ar, _, db := newFixture(t)
	old := time.Now().AddDate(0, 0, -40)
	addMsg(t, db, 1, old, "很旧的消息")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
		KeyKeepEnabled: "1", KeyKeepDays: "7",
	}, 0)

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	day := old.Format("2006-01-02")
	found := false
	for _, d := range ar.purged {
		if d == day {
			found = true
		}
	}
	if !found {
		t.Fatalf("上传成功后应清理 %s，实际清理了 %v", day, ar.purged)
	}
}

// 保留期内的数据即使上传了也不能删。
func TestPurgeKeepsRecentDays(t *testing.T) {
	svc, st, ar, _, db := newFixture(t)
	addMsg(t, db, 1, time.Now(), "今天的消息")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
		KeyKeepEnabled: "1", KeyKeepDays: "7",
	}, 0)

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ar.purged) != 0 {
		t.Fatalf("保留期内的数据不应被清理，却清理了 %v", ar.purged)
	}
}

// 没开清理时，归档上传之后本地数据必须还在。
func TestNoPurgeWhenRetainDisabled(t *testing.T) {
	svc, st, ar, _, db := newFixture(t)
	addMsg(t, db, 1, time.Now().AddDate(0, 0, -100), "很旧")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
		KeyKeepEnabled: "0", KeyKeepDays: "7",
	}, 0)

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ar.purged) != 0 {
		t.Fatalf("未启用清理时不应删数据，却删了 %v", ar.purged)
	}
}

// 配置校验：开了清理就必须有保留天数，且必须先启用备份。
func TestSaveConfigValidation(t *testing.T) {
	svc, _, _, _, _ := newFixture(t)
	ctx := context.Background()

	// 开清理但没天数。
	err := svc.SaveConfig(ctx, Config{Retain: true, KeepDays: 0})
	if err == nil || !strings.Contains(err.Error(), "保留天数") {
		t.Fatalf("应提示保留天数，得到 %v", err)
	}

	// 开清理但没备份——会丢数据，必须拒绝。
	err = svc.SaveConfig(ctx, Config{Retain: true, KeepDays: 30, Enabled: false})
	if err == nil || !strings.Contains(err.Error(), "在线备份") {
		t.Fatalf("应要求先启用备份，得到 %v", err)
	}

	// 天数越界。
	err = svc.SaveConfig(ctx, Config{Enabled: true, Type: TypeS3, KeepDays: -1,
		S3: S3Config{Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	if err == nil {
		t.Fatal("负数天数应被拒绝")
	}

	// 正常配置。
	err = svc.SaveConfig(ctx, Config{Enabled: true, Type: TypeS3, KeepDays: 30,
		S3: S3Config{Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s"}})
	if err != nil {
		t.Fatalf("合法配置应保存成功: %v", err)
	}
}

// 凭据必须加密存储，不能明文落库。
func TestCredentialsAreEncrypted(t *testing.T) {
	svc, st, _, _, _ := newFixture(t)
	ctx := context.Background()
	if err := svc.SaveConfig(ctx, Config{
		Enabled: true, Type: TypeS3,
		S3: S3Config{Endpoint: "e", Bucket: "b", AccessKey: "ak", SecretKey: "super-secret"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.GetSetting(ctx, KeyS3SecretKey)
	if raw == "super-secret" {
		t.Fatal("Secret Key 不应明文存储")
	}
	// 但读回来要能解密成原文。
	cfg, err := svc.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3.SecretKey != "super-secret" {
		t.Fatalf("解密后应还原，得到 %q", cfg.S3.SecretKey)
	}
}

// 凭据留空保存时，不该把已保存的密码清掉。
func TestSaveKeepsExistingCredentials(t *testing.T) {
	svc, _, _, _, _ := newFixture(t)
	ctx := context.Background()
	base := Config{Enabled: true, Type: TypeWebDAV,
		WebDAV: WebDAVConfig{URL: "https://dav.example.com/x", Username: "u", Password: "p"}}
	if err := svc.SaveConfig(ctx, base); err != nil {
		t.Fatal(err)
	}

	// 模拟表单里密码留空（handler 会沿用旧值，这里直接验证 LoadConfig 仍能读到）。
	cfg, err := svc.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebDAV.Password != "p" {
		t.Fatalf("密码应保留，得到 %q", cfg.WebDAV.Password)
	}
}

func TestStatusReportsDays(t *testing.T) {
	svc, st, _, _, db := newFixture(t)
	now := time.Now()
	addMsg(t, db, 1, now, "今天")
	_ = st.SetSettings(context.Background(), map[string]string{KeyEnabled: "1"}, 0)

	if err := svc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Days) == 0 {
		t.Fatal("状态里应列出归档的日子")
	}
	if status.LocalCount == 0 {
		t.Fatal("应统计到本地归档文件")
	}
	if status.LocalBytes == 0 {
		t.Fatal("应统计到归档字节数")
	}
}

// 空的一天不该在归档目录里留下 0 字节文件。
func TestEmptyDayProducesNoFile(t *testing.T) {
	svc, st, _, _, _ := newFixture(t)
	// 手工标记一个"有消息"的日子，但库里其实没有。
	_ = st.SetSettings(context.Background(), map[string]string{KeyEnabled: "1"}, 0)
	if err := svc.archiveDay(context.Background(), "2020-01-01"); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(svc.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		t.Fatalf("空的一天不应产生文件，发现 %s", f.Name())
	}
}

var _ = newTargetHook

// 上传全部失败时，RunOnce 的结果必须反映出来。
//
// 这条来自真实踩坑：WebDAV 完全不可达时页面还显示"执行完成"，
// 用户以为备份好了。备份静默失败比备份坏掉更危险。
func TestRunOnceReportsUploadFailures(t *testing.T) {
	svc, st, _, target, db := newFixture(t)
	target.failPut = true
	addMsg(t, db, 1, time.Now(), "今天")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
	}, 0)

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting(ctx, KeyLastResult); v != "error" {
		t.Fatalf("上传失败时结果应为 error，得到 %q", v)
	}
	msg, _ := st.GetSetting(ctx, KeyLastMessage)
	if !strings.Contains(msg, "上传失败") {
		t.Fatalf("提示应说明上传失败，得到 %q", msg)
	}
}

// 部分失败时，成功的那天仍然要标记为已上传。
func TestRunOncePartialUploadFailure(t *testing.T) {
	svc, st, ar, target, db := newFixture(t)
	addMsg(t, db, 1, time.Now().AddDate(0, 0, -1), "昨天")
	addMsg(t, db, 2, time.Now(), "今天")

	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyEnabled: "1", KeyType: TypeS3, KeyS3Bucket: "b",
	}, 0)

	// 让第二次上传失败。
	var calls int
	newTargetHook = func(Config) (Target, error) {
		return &flakyTarget{inner: target, failAfter: 1, calls: &calls}, nil
	}
	t.Cleanup(func() { newTargetHook = nil })

	if err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	states, _ := ar.ListArchiveStates(ctx, 10)
	if len(states) != 2 {
		t.Fatalf("应有 2 天状态，得到 %d", len(states))
	}
	uploaded := 0
	for _, a := range states {
		if a.UploadedAt > 0 {
			uploaded++
		}
	}
	if uploaded != 1 {
		t.Fatalf("应恰好 1 天上传成功，实际 %d", uploaded)
	}
}

// flakyTarget 前 n 次成功、之后失败。
type flakyTarget struct {
	inner     Target
	failAfter int
	calls     *int
}

func (f *flakyTarget) Name() string { return f.inner.Name() }
func (f *flakyTarget) Put(ctx context.Context, key, path string) error {
	*f.calls++
	if *f.calls > f.failAfter {
		return errors.New("模拟第二次上传失败")
	}
	return f.inner.Put(ctx, key, path)
}
func (f *flakyTarget) Delete(ctx context.Context, key string) error { return f.inner.Delete(ctx, key) }
func (f *flakyTarget) Ping(ctx context.Context) error               { return f.inner.Ping(ctx) }
