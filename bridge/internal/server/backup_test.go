package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"muse-bridge/internal/archive"
	"muse-bridge/internal/backup"
)

// newBackupFixture 造一个挂了备份服务的实例。
//
// 归档目录放在临时目录，数据库用 fixture 自带的那个：
// 这样归档能从真实消息里查数据，整条链路都是真的。
func newBackupFixture(t *testing.T) (*fixture, *backup.Service) {
	t.Helper()
	f := newFixture(t)
	dir := t.TempDir()
	svc := backup.NewService(f.st, f.st, testSealer{}, f.st.DB(), dir, time.Local)
	f.srv.SetBackup(svc)
	return f, svc
}

// testSealer 是最简的"加密"：测试只关心往返一致。
type testSealer struct{}

func (testSealer) Seal(p string) (string, error) { return "enc:" + p, nil }
func (testSealer) Open(e string) (string, error) {
	if !strings.HasPrefix(e, "enc:") {
		return "", os.ErrInvalid
	}
	return strings.TrimPrefix(e, "enc:"), nil
}

func TestBackupPageRequiresLogin(t *testing.T) {
	f, _ := newBackupFixture(t)
	req := httptest.NewRequest("GET", "/backup", nil)
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("未登录应跳登录页，得到 %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestBackupPageRenders(t *testing.T) {
	f, _ := newBackupFixture(t)
	w := f.adminGet(t, "/backup")
	if w.Code != 200 {
		t.Fatalf("备份页返回 %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"备份与归档", "S3 兼容对象存储", "WebDAV", "本地保留", "立即归档并上传"} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q", want)
		}
	}
}

// 关闭备份的实例：页面显示关闭，所有写接口都要拒绝。
func TestBackupDisabledInstance(t *testing.T) {
	f := newFixture(t) // 不挂 backup
	body := f.adminGet(t, "/backup").Body.String()
	if !strings.Contains(body, "已关闭") {
		t.Fatal("禁用时应显示关闭说明")
	}
	for _, path := range []string{"/backup/save", "/backup/test", "/backup/run"} {
		w := f.adminForm(t, path, "enabled=1")
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s 应跳回，得到 %d", path, w.Code)
		}
	}
}

// 保存 S3 配置。
func TestBackupSaveS3Config(t *testing.T) {
	f, svc := newBackupFixture(t)
	form := "type=s3&enabled=1&s3_endpoint=https%3A%2F%2Fs3.example.com" +
		"&s3_region=us-east-1&s3_bucket=mybucket&s3_access_key=AK" +
		"&s3_secret_key=SECRET&prefix=muse&keep_days=30"
	w := f.adminForm(t, "/backup/save", form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("保存应跳回，得到 %d", w.Code)
	}

	cfg, err := svc.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Type != backup.TypeS3 || !cfg.Enabled {
		t.Fatalf("配置未保存: %+v", cfg)
	}
	if cfg.S3.Bucket != "mybucket" || cfg.S3.SecretKey != "SECRET" {
		t.Fatalf("S3 字段不对: %+v", cfg.S3)
	}
	if cfg.Prefix != "muse" {
		t.Fatalf("前缀不对: %q", cfg.Prefix)
	}
}

// 保存 WebDAV 配置。
func TestBackupSaveWebDAVConfig(t *testing.T) {
	f, svc := newBackupFixture(t)
	form := "type=webdav&enabled=1&webdav_url=https%3A%2F%2Fdav.example.com%2Fbridge" +
		"&webdav_user=alice&webdav_password=pw&keep_days=0"
	if w := f.adminForm(t, "/backup/save", form); w.Code != http.StatusSeeOther {
		t.Fatalf("保存应跳回，得到 %d", w.Code)
	}
	cfg, err := svc.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Type != backup.TypeWebDAV || cfg.WebDAV.Username != "alice" {
		t.Fatalf("WebDAV 配置未保存: %+v", cfg)
	}
}

// 安全校验：开自动清理但没开备份，必须被拒绝（否则会丢数据）。
func TestBackupRejectsRetainWithoutBackup(t *testing.T) {
	f, svc := newBackupFixture(t)
	form := "type=s3&enabled=0&retain=1&keep_days=30" +
		"&s3_endpoint=https%3A%2F%2Fs3.example.com&s3_bucket=b&s3_access_key=a&s3_secret_key=s"
	w := f.adminForm(t, "/backup/save", form)
	body := f.followFlash(t, w, "/backup").Body.String()
	if !strings.Contains(body, "在线备份") {
		t.Fatalf("应提示必须先启用备份，页面: %s", truncateForLog(body, 400))
	}
	cfg, _ := svc.LoadConfig(context.Background())
	if cfg.Retain {
		t.Fatal("非法配置不应被保存")
	}
}

// 开清理但保留天数为 0 也要拒绝。
func TestBackupRejectsRetainWithoutDays(t *testing.T) {
	f, _ := newBackupFixture(t)
	form := "type=s3&enabled=1&retain=1&keep_days=0" +
		"&s3_endpoint=https%3A%2F%2Fs3.example.com&s3_bucket=b&s3_access_key=a&s3_secret_key=s"
	w := f.adminForm(t, "/backup/save", form)
	body := f.followFlash(t, w, "/backup").Body.String()
	if !strings.Contains(body, "保留天数") {
		t.Fatalf("应提示保留天数，页面: %s", truncateForLog(body, 400))
	}
}

// 手动执行一轮：应把当天消息归档成文件。
func TestBackupRunArchivesToday(t *testing.T) {
	f, svc := newBackupFixture(t)

	// 先塞一条今天的消息。用 fixture 里真实的桥和会话 id，
	// 写死 1 会因为外键约束失败（建桥时 id 是自增的）。
	insertMessage(t, f, "今天的话")

	w := f.adminForm(t, "/backup/run", "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("执行应跳回，得到 %d", w.Code)
	}

	// 归档目录里应该有今天的文件。
	day := time.Now().Format("2006-01-02")
	path := filepath.Join(svc.Dir(), archive.FileName(day))
	if _, err := os.Stat(path); err != nil {
		entries, _ := os.ReadDir(svc.Dir())
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("应生成 %s，目录内容: %v", archive.FileName(day), names)
	}
}

// 页面要能显示归档状态。
func TestBackupPageShowsArchivedDays(t *testing.T) {
	f, _ := newBackupFixture(t)
	insertMessage(t, f, "消息")
	_ = f.adminForm(t, "/backup/run", "")

	body := f.adminGet(t, "/backup").Body.String()
	if !strings.Contains(body, "已归档") {
		t.Fatalf("页面应显示已归档状态")
	}
	if !strings.Contains(body, time.Now().Format("2006-01-02")) {
		t.Fatal("页面应显示日期")
	}
}

// insertMessage 往 fixture 的桥里塞一条今天的消息。
func insertMessage(t *testing.T, f *fixture, text string) {
	t.Helper()
	now := time.Now().Unix()
	bridgeID := f.bridge.ID
	var convID int64
	if err := f.st.DB().QueryRow(
		`SELECT id FROM conversations WHERE bridge_id=? LIMIT 1`, bridgeID).Scan(&convID); err != nil {
		// 会话还不存在就先建一个。
		res, cerr := f.st.DB().Exec(
			`INSERT INTO conversations (bridge_id, chat_id, chat_type, title, last_message_at, created_at)
			 VALUES (?,?,?,?,?,?)`, bridgeID, 999, "private", "家", now, now)
		if cerr != nil {
			t.Fatal(cerr)
		}
		convID, _ = res.LastInsertId()
	}
	if _, err := f.st.DB().Exec(
		`INSERT INTO messages (bridge_id, conversation_id, direction, text, content_kind, created_at)
		 VALUES (?,?,'in',?,'text',?)`, bridgeID, convID, text, now); err != nil {
		t.Fatal(err)
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
