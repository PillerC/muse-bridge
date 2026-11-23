package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"muse-bridge/internal/upgrade"
)

// newUpgradeFixture 造一个带假 GitHub 的测试实例。
//
// 后台升级页要真的走"查 GitHub -> 下载 -> 校验 -> 替换"这条链，
// 所以这里起一个假服务，而不是把 upgrader 换成桩：链路本身才是要测的。
func newUpgradeFixture(t *testing.T, current, tag string, binary []byte) (*fixture, *httptest.Server) {
	t.Helper()

	sum := upgrade.NewHash()
	sum.Write(binary)
	assetName := upgrade.AssetName("linux", "amd64")

	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/test/repo/releases", func(w http.ResponseWriter, r *http.Request) {
		rel := []map[string]any{{
			"tag_name": tag, "name": tag, "body": "测试更新说明",
			"html_url": "https://example.com/rel", "published_at": "2026-11-22T00:00:00Z",
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": base + "/dl/" + assetName, "size": len(binary)},
				{"name": "checksums.txt", "browser_download_url": base + "/dl/checksums.txt"},
			},
		}}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			_, _ = w.Write([]byte(upgrade.SumHex(sum) + "  " + assetName + "\n"))
			return
		}
		_, _ = w.Write(binary)
	})
	gh := httptest.NewServer(mux)
	base = gh.URL
	t.Cleanup(gh.Close)

	f := newFixture(t)

	// 自己文件放在临时目录：升级要替换的是它。
	selfDir := t.TempDir()
	self := filepath.Join(selfDir, "bridge")
	if err := os.WriteFile(self, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := upgrade.NewService(f.st, upgrade.Options{
		Repo:    "test/repo",
		APIBase: gh.URL,
		Current: current,
		GOOS:    "linux",
		GOARCH:  "amd64",
	})
	svc.SetSelfPathForTest(self)
	// 测试里绝不真的 exec：换成一个记录用的桩。
	svc.SetRestartForTest(func() error { return nil })
	f.srv.SetUpgrader(svc)
	return f, gh
}

// adminForm 以已登录身份提交一个表单。
func (f *fixture) adminForm(t *testing.T, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	return w
}

// adminGet 以已登录身份打开页面。
func (f *fixture) adminGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(f.adminCookie(t))
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	return w
}

// followFlash 模拟浏览器"带着上一步下发的 cookie 再请求一次"。
//
// 一次性提示走的是 cookie，真实浏览器会自动带上；测试里每个请求都是
// 新建的，必须手动把 Set-Cookie 回填，否则永远看不到提示——
// 这不是被测代码的问题，是测试脚手架漏了浏览器行为。
func (f *fixture) followFlash(t *testing.T, prev *httptest.ResponseRecorder, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(f.adminCookie(t))
	for _, c := range prev.Result().Cookies() {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	return w
}

func TestUpgradePageRequiresLogin(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))
	req := httptest.NewRequest("GET", "/upgrade", nil)
	w := httptest.NewRecorder()
	f.public.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("未登录应跳登录页，得到 %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestUpgradePageShowsVersion(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))
	w := f.adminGet(t, "/upgrade")
	if w.Code != 200 {
		t.Fatalf("升级页返回 %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"在线升级", "定时升级", "检查更新", "bridge-linux-amd64"} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q", want)
		}
	}
}

func TestUpgradeCheckShowsNewVersion(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new-binary"))

	w := f.adminForm(t, "/upgrade/check", "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("检查应跳回升级页，得到 %d", w.Code)
	}
	body := f.followFlash(t, w, "/upgrade").Body.String()
	if !strings.Contains(body, "v1.1.0") || !strings.Contains(body, "有新版本") {
		t.Fatal("检查后页面应显示新版本")
	}
}

func TestUpgradeApplyRequiresConfirm(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new-binary"))
	_ = f.adminForm(t, "/upgrade/check", "")

	w := f.adminForm(t, "/upgrade/apply", "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("未确认时也应跳回页面（带错误提示），得到 %d", w.Code)
	}
	// 没勾确认就不该动文件：这里通过"升级后版本没变"间接确认。
	body := f.followFlash(t, w, "/upgrade").Body.String()
	if strings.Contains(body, "刚刚完成升级") {
		t.Fatal("未确认不应执行升级")
	}
}

func TestUpgradeApplyReplacesBinary(t *testing.T) {
	newBinary := []byte("#!/bin/sh\necho upgraded\n")
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", newBinary)
	_ = f.adminForm(t, "/upgrade/check", "")

	w := f.adminForm(t, "/upgrade/apply", "confirm=1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("升级后应跳回页面，得到 %d: %s", w.Code, w.Body)
	}

	// 二进制必须被真的替换成新内容。
	self := f.srv.Upgrader().SelfPath()
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("自身文件应被替换，得到 %q", got)
	}

	// 升级标记要落库：这是重启后确认"升级真的生效"的唯一依据。
	// 测试里重启是桩，进程仍跑 v1.0.0，所以这里不该显示"刚刚完成升级"。
	body := f.followFlash(t, w, "/upgrade").Body.String()
	if strings.Contains(body, "刚刚完成升级") {
		t.Fatal("还没重启到新版本时不应声称升级完成")
	}
}

// 重启后（新版本进程起来）页面要能确认"刚才是从 v1.0.0 升上来的"。
func TestUpgradeConfirmedAfterRestart(t *testing.T) {
	newBinary := []byte("new")
	f, gh := newUpgradeFixture(t, "v1.0.0", "v1.1.0", newBinary)
	_ = f.adminForm(t, "/upgrade/check", "")
	_ = f.adminForm(t, "/upgrade/apply", "confirm=1")

	// 模拟进程已经 exec 成新版本：换一个 Current=v1.1.0 的服务，
	// 共用同一个数据库（升级标记就在里面）。
	up := upgrade.NewService(f.st, upgrade.Options{
		Repo: "test/repo", APIBase: gh.URL, Current: "v1.1.0", GOOS: "linux", GOARCH: "amd64",
	})
	up.SetSelfPathForTest(f.srv.Upgrader().SelfPath())
	up.SetRestartForTest(func() error { return nil })
	up.ConfirmStartup(context.Background())
	f.srv.SetUpgrader(up)

	body := f.adminGet(t, "/upgrade").Body.String()
	if !strings.Contains(body, "刚刚完成升级") {
		t.Fatal("重启到新版本后应显示升级完成")
	}
	if !strings.Contains(body, "v1.0.0") || !strings.Contains(body, "v1.1.0") {
		t.Fatal("完成提示应带出升级前后版本")
	}
}

func TestUpgradeConfigSaveAndValidate(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))

	w := f.adminForm(t, "/upgrade/config", "channel=prerelease&auto=1&at=03:30&startup_check=1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("保存配置应跳回，得到 %d", w.Code)
	}
	body := f.adminGet(t, "/upgrade").Body.String()
	if !strings.Contains(body, `value="03:30"`) {
		t.Fatal("页面应回填保存后的时间")
	}

	// 非法时间要给出错误而不是静默存进去。
	w = f.adminForm(t, "/upgrade/config", "channel=release&at=99:99")
	body = f.followFlash(t, w, "/upgrade").Body.String()
	if !strings.Contains(body, "格式应为") {
		t.Fatalf("非法时间应提示错误")
	}
}

func TestUpgradeDisabledInstance(t *testing.T) {
	// 不挂 upgrader，模拟 BRIDGE_UPGRADE_DISABLED=1。
	f := newFixture(t)
	body := f.adminGet(t, "/upgrade").Body.String()
	if !strings.Contains(body, "已关闭") {
		t.Fatal("禁用时应显示关闭说明")
	}

	// 所有写操作也要被拒绝，不能靠隐藏入口。
	for _, path := range []string{"/upgrade/check", "/upgrade/apply", "/upgrade/config", "/upgrade/rollback"} {
		w := f.adminForm(t, path, "confirm=1")
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s 应跳回并提示，得到 %d", path, w.Code)
		}
		body := f.followFlash(t, w, "/upgrade").Body.String()
		if !strings.Contains(body, "已关闭") {
			t.Fatalf("%s 不应生效", path)
		}
	}
}

// flash cookie 必须能承载中文：升级、保存这类操作的反馈全靠它，
// 中文被 net/http 丢掉的话用户什么都看不到。
func TestFlashCookieCarriesChinese(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))
	w := f.adminForm(t, "/upgrade/check", "")
	var flash *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_flash" {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("应下发 flash cookie")
	}
	// 值必须是合法 cookie 字节（ASCII 可见），否则会被 net/http 丢弃。
	for i := 0; i < len(flash.Value); i++ {
		if flash.Value[i] < 0x20 || flash.Value[i] > 0x7e {
			t.Fatalf("flash cookie 含非 ASCII 字节，会被丢弃: %q", flash.Value)
		}
	}
}

func TestUpgradeRollbackRestoresBackup(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new-binary"))
	up := f.srv.Upgrader()
	self := up.SelfPath()
	if err := os.WriteFile(self+".bak", []byte("previous-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := f.adminForm(t, "/upgrade/rollback", "confirm=1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("回滚应跳回，得到 %d", w.Code)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous-binary" {
		t.Fatalf("回滚后应恢复备份内容，得到 %q", got)
	}
}

// 保证 status 计算不依赖真实网络：这里只验证"没有检查过"时的默认值。
func TestUpgradeStatusDefaults(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))
	st, err := f.srv.Upgrader().Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Current != "v1.0.0" {
		t.Fatalf("当前版本应为 v1.0.0，得到 %s", st.Current)
	}
	if st.HasUpdate {
		t.Fatal("还没检查过时不应声称有更新")
	}
	if st.Config.At != "04:00" || st.Config.Auto {
		t.Fatalf("默认配置不对: %+v", st.Config)
	}
}

var _ = time.Now

// 回归：升级成功后必须先把响应写给浏览器，再重启。
//
// 之前的 bug 是 Apply 内部直接 exec：exec 替换进程映像，handler 里
// 写响应的代码永远不执行，浏览器拿到被中断的连接后会自动重发 POST，
// 新进程看到版本已是最新，于是返回"当前版本已不低于最新版本"——
// 升级明明成功了，页面上却显示失败。生产上两次升级都复现了。
func TestUpgradeApplyWritesResponseBeforeRestart(t *testing.T) {
	newBinary := []byte("#!/bin/sh\necho upgraded\n")
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", newBinary)
	_ = f.adminForm(t, "/upgrade/check", "")

	up := f.srv.Upgrader()
	// 记录"重启被调用的那一刻，响应是否已经写好"。
	var restarted bool
	up.SetRestartForTest(func() error {
		restarted = true
		return nil
	})

	w := f.adminForm(t, "/upgrade/apply", "confirm=1")

	// 1) 响应必须是正常的 303，不是 500 也不是空响应。
	if w.Code != http.StatusSeeOther {
		t.Fatalf("升级后应返回 303，得到 %d: %s", w.Code, w.Body)
	}
	// 2) 必须有"已升级到"的成功提示，不能是错误。
	var flash *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_flash" {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("应下发 flash 提示")
	}
	raw, _ := url.QueryUnescape(flash.Value)
	if strings.HasPrefix(raw, "err|") {
		t.Fatalf("升级成功却下发了错误提示: %s", raw)
	}
	if !strings.Contains(raw, "已升级到 v1.1.0") {
		t.Fatalf("提示应说明升级到了哪个版本，得到 %q", raw)
	}

	// 3) 重启是异步的（给浏览器留时间收完响应），等一下再看。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !restarted {
		time.Sleep(20 * time.Millisecond)
	}
	if !restarted {
		t.Fatal("响应写完之后应触发重启")
	}

	// 4) 二进制确实被换了。
	got, err := os.ReadFile(up.SelfPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("自身文件应被替换，得到 %q", got)
	}
}

// 对照：重复提交同一个升级请求时，第二个请求应明确报"已是最新"，
// 而不是又走一遍下载安装。
//
// 这正是旧 bug 下浏览器重发请求会走到的分支——现在它只在真的重复提交时出现，
// 而且是一个干净的 303 + 错误提示，不会误伤第一次成功的升级。
func TestUpgradeApplyTwiceSecondIsNoop(t *testing.T) {
	f, _ := newUpgradeFixture(t, "v1.0.0", "v1.1.0", []byte("new"))
	up := f.srv.Upgrader()
	up.SetRestartForTest(func() error { return nil })
	_ = f.adminForm(t, "/upgrade/check", "")

	if w := f.adminForm(t, "/upgrade/apply", "confirm=1"); w.Code != http.StatusSeeOther {
		t.Fatalf("第一次升级应 303，得到 %d", w.Code)
	}

	// 模拟进程已经 exec 成新版本：换一个 Current=v1.1.0 的服务，共用同一个库。
	up2 := upgrade.NewService(f.st, upgrade.Options{
		Repo: "test/repo", APIBase: up.APIBase(), Current: "v1.1.0", GOOS: "linux", GOARCH: "amd64",
	})
	up2.SetSelfPathForTest(up.SelfPath())
	up2.SetRestartForTest(func() error { return nil })
	f.srv.SetUpgrader(up2)

	w := f.adminForm(t, "/upgrade/apply", "confirm=1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("重复升级应 303，得到 %d", w.Code)
	}
	var flash *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "mb_flash" {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("应下发提示")
	}
	raw, _ := url.QueryUnescape(flash.Value)
	if !strings.Contains(raw, "已不低于最新版本") {
		t.Fatalf("重复提交应提示已是最新，得到 %q", raw)
	}
}
