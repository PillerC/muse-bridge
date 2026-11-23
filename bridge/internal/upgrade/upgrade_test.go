package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub 模拟 GitHub Releases API 与资产下载。
type fakeGitHub struct {
	releases []GitHubRelease
	assets   map[string][]byte
	status   int
	srv      *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{assets: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustJSON(t, f.releases))
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		body, ok := f.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) addRelease(tag string, prerelease bool, assets ...string) {
	rel := GitHubRelease{
		TagName:     tag,
		Name:        tag,
		HTMLURL:     "https://example.com/" + tag,
		Prerelease:  prerelease,
		PublishedAt: "2026-11-22T00:00:00Z",
	}
	for _, name := range assets {
		rel.Assets = append(rel.Assets, struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Size               int64  `json:"size"`
		}{Name: name, BrowserDownloadURL: f.srv.URL + "/dl/" + name, Size: int64(len(f.assets[name]))})
	}
	f.releases = append(f.releases, rel)
}

func (f *fakeGitHub) setAsset(name string, body []byte) {
	f.assets[name] = body
}

func (f *fakeGitHub) client(t *testing.T, current, channel string) *Client {
	t.Helper()
	return NewClient(Options{
		Repo:    "test/repo",
		APIBase: f.srv.URL,
		Channel: channel,
		Current: current,
		GOOS:    "linux",
		GOARCH:  "amd64",
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckPicksLatestReleaseWithAsset(t *testing.T) {
	f := newFakeGitHub(t)
	f.setAsset("bridge-linux-amd64", []byte("binary-v1"))
	f.setAsset("checksums.txt", []byte("abc  bridge-linux-amd64\n"))
	// 最新的那一版没为当前平台出包，应该被跳过而不是报错。
	f.addRelease("v1.3.0", false, "bridge-darwin-arm64")
	f.addRelease("v1.2.0", false, "bridge-linux-amd64", "checksums.txt")

	rel, err := f.client(t, "v1.0.0", ChannelRelease).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil {
		t.Fatal("应找到可用版本")
	}
	if rel.Tag != "v1.2.0" {
		t.Fatalf("期望选中 v1.2.0，得到 %s", rel.Tag)
	}
	if rel.AssetName != "bridge-linux-amd64" {
		t.Fatalf("资产名不对: %s", rel.AssetName)
	}
	if rel.ChecksumURL == "" {
		t.Fatal("应带上 checksums.txt 地址")
	}
}

func TestCheckSkipsPrereleaseByDefault(t *testing.T) {
	f := newFakeGitHub(t)
	f.setAsset("bridge-linux-amd64", []byte("bin"))
	f.addRelease("v2.0.0-rc1", true, "bridge-linux-amd64")
	f.addRelease("v1.9.0", false, "bridge-linux-amd64")

	rel, err := f.client(t, "v1.0.0", ChannelRelease).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil || rel.Tag != "v1.9.0" {
		t.Fatalf("release 通道应跳过预发布，得到 %+v", rel)
	}

	rel, err = f.client(t, "v1.0.0", ChannelPrerelease).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil || rel.Tag != "v2.0.0-rc1" {
		t.Fatalf("prerelease 通道应选中预发布，得到 %+v", rel)
	}
}

func TestCheckNoRelease(t *testing.T) {
	f := newFakeGitHub(t)
	rel, err := f.client(t, "v1.0.0", ChannelRelease).Check(context.Background())
	if err != nil {
		t.Fatalf("没有发布不应报错: %v", err)
	}
	if rel != nil {
		t.Fatalf("没有发布时应返回 nil，得到 %+v", rel)
	}
}

func TestCheckRateLimited(t *testing.T) {
	f := newFakeGitHub(t)
	f.status = http.StatusForbidden
	_, err := f.client(t, "v1.0.0", ChannelRelease).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("限流应给出可读错误，得到 %v", err)
	}
}

func TestCheckWindowsUnsupported(t *testing.T) {
	c := NewClient(Options{Repo: "test/repo", GOOS: "windows", GOARCH: "amd64"})
	if _, err := c.Check(context.Background()); err != ErrUnsupportedPlatform {
		t.Fatalf("Windows 应返回 ErrUnsupportedPlatform，得到 %v", err)
	}
}

func TestFetchChecksums(t *testing.T) {
	f := newFakeGitHub(t)
	f.setAsset("checksums.txt", []byte(
		"ABC123  bridge-linux-amd64\n"+
			"def456 *bridge-darwin-arm64\n"+
			"垃圾行\n"))
	c := f.client(t, "v1.0.0", ChannelRelease)
	got, err := c.FetchChecksums(context.Background(), f.srv.URL+"/dl/checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got["bridge-linux-amd64"] != "abc123" {
		t.Fatalf("摘要应小写归一，得到 %q", got["bridge-linux-amd64"])
	}
	if got["bridge-darwin-arm64"] != "def456" {
		t.Fatalf("带 * 前缀的文件名应被剥掉，得到 %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("应只解析出 2 行，得到 %d", len(got))
	}
}

func TestFetchChecksumsMissing(t *testing.T) {
	c := NewClient(Options{Repo: "test/repo", GOOS: "linux", GOARCH: "amd64"})
	if _, err := c.FetchChecksums(context.Background(), ""); err == nil {
		t.Fatal("没有校验文件地址时应报错")
	}
}

// 升级包必须校验摘要：不匹配时既不能写进目标文件，也不能留下临时文件。
func TestInstallRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Install(strings.NewReader("new-binary"), target, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("摘要不符时应失败")
	}
	if !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("错误信息应说明校验失败，得到 %v", err)
	}

	body, _ := os.ReadFile(target)
	if string(body) != "old-binary" {
		t.Fatalf("校验失败后旧文件必须保持原样，得到 %q", body)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bridge-upgrade-") {
			t.Fatalf("校验失败后不应残留临时文件: %s", e.Name())
		}
	}
}

func TestInstallReplacesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newBody := []byte("new-binary-content")
	sum := sha256.Sum256(newBody)
	backup, err := Install(strings.NewReader(string(newBody)), target, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != string(newBody) {
		t.Fatalf("目标文件应被替换，得到 %q", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("替换后的文件必须可执行，当前 %v", info.Mode())
	}
	if backup == "" {
		t.Fatal("应返回备份文件路径")
	}
	old, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("备份文件应存在: %v", err)
	}
	if string(old) != "old-binary" {
		t.Fatalf("备份内容应为旧版本，得到 %q", old)
	}
}

func TestInstallEmptyBody(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(strings.NewReader(""), target, ""); err == nil {
		t.Fatal("空升级包应失败")
	}
}

func TestInstallUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视目录权限，这个用例只在普通用户下有意义")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := Install(strings.NewReader("new"), target, ""); err == nil {
		t.Fatal("目录不可写时应失败")
	}
}

func TestDownloadRespectsSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", 1024)), 1024)
	}))
	defer srv.Close()

	c := NewClient(Options{Repo: "t/r", GOOS: "linux", GOARCH: "amd64"})
	var buf strings.Builder
	n, err := c.Download(context.Background(), srv.URL, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1024 || buf.Len() != 1024 {
		t.Fatalf("下载字节数不对: n=%d len=%d", n, buf.Len())
	}
}

// InstallFile 是升级实际走的路径：下载完的文件已经在目标目录，
// 校验通过后直接 rename，不再复制一遍内容。
func TestInstallFileReplacesWithoutCopy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	newBody := []byte("downloaded-binary")
	src := filepath.Join(dir, ".bridge-download-123")
	if err := os.WriteFile(src, newBody, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(newBody)

	backup, err := InstallFile(src, target, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(target)
	if string(got) != string(newBody) {
		t.Fatalf("目标应被替换，得到 %q", got)
	}
	// 源文件被 rename 走了，不该残留。
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("rename 之后源文件不应存在")
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("必须可执行，当前 %v", info.Mode())
	}
	old, _ := os.ReadFile(backup)
	if string(old) != "old-binary" {
		t.Fatalf("备份应为旧版本，得到 %q", old)
	}
}

func TestInstallFileRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bridge")
	if err := os.WriteFile(target, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".bridge-download-456")
	if err := os.WriteFile(src, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallFile(src, target, strings.Repeat("0", 64)); err == nil {
		t.Fatal("摘要不符应失败")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old-binary" {
		t.Fatalf("校验失败后旧文件必须完好，得到 %q", got)
	}
}
