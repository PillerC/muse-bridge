package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memSettings 是 Settings 的内存实现，测试里代替 SQLite。
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

// releaseServer 起一个假的 GitHub，返回新版本二进制和校验文件。
func releaseServer(t *testing.T, tag string, binary []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	checksums := hex.EncodeToString(sum[:]) + "  bridge-linux-amd64\n"
	assetName := AssetName("linux", "amd64")

	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/repos/test/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := []map[string]any{{
			"tag_name":     tag,
			"name":         tag,
			"body":         "修了几个 bug",
			"html_url":     "https://example.com/release",
			"published_at": "2026-11-22T00:00:00Z",
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": srvURL + "/dl/" + assetName, "size": len(binary)},
				{"name": "checksums.txt", "browser_download_url": srvURL + "/dl/checksums.txt", "size": len(checksums)},
			},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/dl/") {
		case assetName:
			_, _ = w.Write(binary)
		case "checksums.txt":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// newUpgradeFixture 造一个"当前版本 v1.0.0、自身文件在临时目录"的服务。
func newUpgradeFixture(t *testing.T, srv *httptest.Server, current string) (*Service, *memSettings, string) {
	t.Helper()
	dir := t.TempDir()
	self := filepath.Join(dir, "bridge")
	if err := os.WriteFile(self, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := newMemSettings()
	svc := NewService(st, Options{
		Repo:    "test/repo",
		APIBase: srv.URL,
		Current: current,
		GOOS:    "linux",
		GOARCH:  "amd64",
	})
	svc.SetSelfPathForTest(self)
	return svc, st, self
}

func TestServiceCheckAndApply(t *testing.T) {
	newBinary := []byte("#!/bin/sh\necho new-version\n")
	srv := releaseServer(t, "v1.1.0", newBinary)
	svc, st, self := newUpgradeFixture(t, srv, "v1.0.0")

	var restarted bool
	svc.SetRestartForTest(func() error {
		restarted = true
		return nil
	})

	ctx := context.Background()
	status, err := svc.Check(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !status.HasUpdate {
		t.Fatalf("应识别出新版本，得到 %+v", status)
	}
	if status.Latest.Tag != "v1.1.0" {
		t.Fatalf("最新版本应为 v1.1.0，得到 %s", status.Latest.Tag)
	}

	tag, err := svc.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.1.0" {
		t.Fatalf("应返回安装到的版本 v1.1.0，得到 %q", tag)
	}
	// Apply 不再负责重启：调用方要在写完自己的响应之后自己调 Restart。
	if restarted {
		t.Fatal("Apply 不该自己重启（会让 HTTP 响应发不出去）")
	}
	if err := svc.Restart(); err != nil {
		t.Fatal(err)
	}
	if !restarted {
		t.Fatal("Restart 应触发重启")
	}

	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("自身文件应被替换成新版本，得到 %q", got)
	}

	// 升级标记应落库，供重启后确认。
	if v, _ := st.GetSetting(ctx, KeyPendingTo); v != "v1.1.0" {
		t.Fatalf("升级标记应为 v1.1.0，得到 %q", v)
	}
}

func TestServiceApplyRejectsWhenUpToDate(t *testing.T) {
	srv := releaseServer(t, "v1.0.0", []byte("bin"))
	svc, _, _ := newUpgradeFixture(t, srv, "v1.0.0")
	svc.SetRestartForTest(func() error { t.Fatal("不该重启"); return nil })

	_, err := svc.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "已不低于最新版本") {
		t.Fatalf("同版本时不应升级，得到 %v", err)
	}
}

func TestServiceApplyRejectsBadChecksum(t *testing.T) {
	newBinary := []byte("new-binary")
	// 手动造一个摘要不匹配的服务器。
	assetName := AssetName("linux", "amd64")
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/repos/test/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := []map[string]any{{
			"tag_name": "v1.1.0", "name": "v1.1.0", "published_at": "2026-11-22T00:00:00Z",
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": srvURL + "/dl/" + assetName, "size": len(newBinary)},
				{"name": "checksums.txt", "browser_download_url": srvURL + "/dl/checksums.txt"},
			},
		}}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + assetName + "\n"))
			return
		}
		_, _ = w.Write(newBinary)
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	defer srv.Close()

	svc, st, self := newUpgradeFixture(t, srv, "v1.0.0")
	svc.SetRestartForTest(func() error { t.Fatal("校验失败不该重启"); return nil })

	_, err := svc.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("摘要不符应失败，得到 %v", err)
	}

	got, _ := os.ReadFile(self)
	if string(got) != "old-binary" {
		t.Fatalf("校验失败后旧文件必须完好，得到 %q", got)
	}
	if v, _ := st.GetSetting(context.Background(), KeyLastResult); v != ResultError {
		t.Fatalf("失败应记录到 last_result，得到 %q", v)
	}
}

func TestConfirmStartupSuccess(t *testing.T) {
	svc, st, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.1.0")
	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyPendingFrom: "v1.0.0",
		KeyPendingTo:   "v1.1.0",
	}, 0)

	svc.ConfirmStartup(ctx)

	if v, _ := st.GetSetting(ctx, KeyPendingFrom); v != "" {
		t.Fatal("确认后应清掉 pending_from 标记")
	}
	if v, _ := st.GetSetting(ctx, KeyLastMessage); !strings.Contains(v, "升级到 v1.1.0") {
		t.Fatalf("应记录升级完成，得到 %q", v)
	}
}

func TestConfirmStartupNotApplied(t *testing.T) {
	svc, st, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	ctx := context.Background()
	_ = st.SetSettings(ctx, map[string]string{
		KeyPendingFrom: "v0.9.0",
		KeyPendingTo:   "v1.0.1",
	}, 0)

	svc.ConfirmStartup(ctx)

	if v, _ := st.GetSetting(ctx, KeyLastResult); v != ResultError {
		t.Fatalf("版本没变应记为失败，得到 %q", v)
	}
	if v, _ := st.GetSetting(ctx, KeyLastMessage); !strings.Contains(v, "未生效") {
		t.Fatalf("应说明升级未生效，得到 %q", v)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	svc, _, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	ctx := context.Background()

	// 默认：稳定通道、定时升级关闭。
	cfg, err := svc.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channel != ChannelRelease || cfg.Auto || cfg.At != "04:00" || !cfg.StartupCheck {
		t.Fatalf("默认配置不对: %+v", cfg)
	}

	want := Config{Channel: ChannelPrerelease, Auto: true, At: "03:30", StartupCheck: false}
	if err := svc.SaveConfig(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := svc.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("配置往返不一致: 得到 %+v，期望 %+v", got, want)
	}
}

func TestSaveConfigValidation(t *testing.T) {
	svc, _, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	ctx := context.Background()

	if err := svc.SaveConfig(ctx, Config{Channel: "nightly", At: "04:00"}); err == nil {
		t.Fatal("非法通道应被拒绝")
	}
	for _, bad := range []string{"4:00", "24:00", "04:60", "", "0400", "aa:bb"} {
		if err := svc.SaveConfig(ctx, Config{Channel: ChannelRelease, At: bad}); err == nil {
			t.Fatalf("非法时间 %q 应被拒绝", bad)
		}
	}
}

func TestRollback(t *testing.T) {
	svc, _, self := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	// 造一个备份文件（相当于升级前留下的旧版本）。
	if err := os.WriteFile(self+".bak", []byte("previous-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(self)
	if string(got) != "previous-binary" {
		t.Fatalf("回滚后应恢复旧版本，得到 %q", got)
	}
}

func TestRollbackWithoutBackup(t *testing.T) {
	svc, _, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	if err := svc.Rollback(); err == nil {
		t.Fatal("没有备份时应报错")
	}
}

func TestStatusReportsPlatform(t *testing.T) {
	svc, _, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.AssetName != "bridge-linux-amd64" {
		t.Fatalf("资产名不对: %s", status.AssetName)
	}
	if !status.CanUpgrade {
		t.Fatal("linux/amd64 应支持在线升级")
	}
	if status.LastResult != ResultNone {
		t.Fatalf("初始状态应为 none，得到 %q", status.LastResult)
	}
}

func TestCheckCachesResult(t *testing.T) {
	newBinary := []byte("bin")
	srv := releaseServer(t, "v1.1.0", newBinary)
	svc, _, _ := newUpgradeFixture(t, srv, "v1.0.0")
	ctx := context.Background()

	// 第一次检查会打服务；之后十分钟内复用缓存。
	if _, err := svc.Check(ctx, true); err != nil {
		t.Fatal(err)
	}
	// 把时钟拨后 1 分钟，仍在缓存窗口内。
	svc.SetNowForTest(func() time.Time { return time.Now().Add(time.Minute) })
	if _, err := svc.Check(ctx, false); err != nil {
		t.Fatal(err)
	}
	// 拨后 11 分钟，缓存过期，会重新请求（服务仍在，应当成功）。
	svc.SetNowForTest(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if _, err := svc.Check(ctx, false); err != nil {
		t.Fatal(err)
	}
}

// 定时窗口：只认"到点后 30 分钟内"，避免进程恰好在那一刻重启就整天跳过，
// 也避免下午启动时立刻补跑一次凌晨的定时。
func TestAutoDue(t *testing.T) {
	base := time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local)
	cases := []struct {
		offset time.Duration
		want   bool
	}{
		{-time.Minute, false},    // 03:59 还没到
		{0, true},                // 04:00 正好
		{10 * time.Minute, true}, // 04:10 仍在窗口内
		{29 * time.Minute, true},
		{31 * time.Minute, false}, // 04:31 已过窗口
		{12 * time.Hour, false},   // 16:00 不该补跑凌晨的
	}
	for _, c := range cases {
		at := base.Add(4*time.Hour + c.offset)
		if got := autoDue(at, "04:00"); got != c.want {
			t.Fatalf("autoDue(%s)=%v，期望 %v", at.Format("15:04"), got, c.want)
		}
	}
	// 非法时刻一律不触发。
	for _, bad := range []string{"", "4:00", "24:00", "aa:bb"} {
		if autoDue(base.Add(4*time.Hour), bad) {
			t.Fatalf("非法时刻 %q 不应触发", bad)
		}
	}
}

// 定时升级的"今天跑过没"必须落库：升级成功会 exec，内存状态全丢。
func TestAutoRunDayPersists(t *testing.T) {
	svc, st, _ := newUpgradeFixture(t, httptest.NewServer(http.NotFoundHandler()), "v1.0.0")
	ctx := context.Background()

	day := "2026-11-22"
	if v, _ := st.GetSetting(ctx, KeyLastAutoDay); v != "" {
		t.Fatalf("初始应为空，得到 %q", v)
	}
	if err := st.SetSetting(ctx, KeyLastAutoDay, day, 0); err != nil {
		t.Fatal(err)
	}
	// 新进程（新 Service 实例，同一个库）也要读到这个日期。
	fresh := NewService(st, svc.opt)
	if v, _ := fresh.st.GetSetting(ctx, KeyLastAutoDay); v != day {
		t.Fatalf("重启后应读到 %q，得到 %q", day, v)
	}
}
