package backup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WebDAV 的目录 URL 结尾有没有斜杠都要拼对。
func TestWebDAVURLJoin(t *testing.T) {
	cases := []struct {
		base, key, want string
	}{
		{"https://dav.example.com/bridge", "2026-11-22.jsonl.gz",
			"https://dav.example.com/bridge/2026-11-22.jsonl.gz"},
		{"https://dav.example.com/bridge/", "2026-11-22.jsonl.gz",
			"https://dav.example.com/bridge/2026-11-22.jsonl.gz"},
		{"https://dav.example.com/remote.php/dav/files/user/bridge/", "x.gz",
			"https://dav.example.com/remote.php/dav/files/user/bridge/x.gz"},
		// 多级前缀也要拼对。
		{"https://dav.example.com/a/b", "sub/x.gz", "https://dav.example.com/a/b/sub/x.gz"},
	}
	for _, c := range cases {
		target, err := NewWebDAV(WebDAVConfig{URL: c.base}, "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := target.(*webdav).objectURL(c.key)
		if err != nil {
			t.Fatal(err)
		}
		if u.String() != c.want {
			t.Fatalf("base=%q key=%q\n得到 %s\n期望 %s", c.base, c.key, u.String(), c.want)
		}
		// 任何情况下都不该出现双斜杠（scheme 后的除外）。
		if strings.Contains(strings.TrimPrefix(u.String(), "https://"), "//") {
			t.Fatalf("URL 出现双斜杠: %s", u.String())
		}
	}
}

func TestWebDAVPutAndAuth(t *testing.T) {
	var gotMethod, gotAuth, gotPath string
	var gotBody []byte
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	target, err := NewWebDAV(WebDAVConfig{
		URL: srv.URL + "/bridge/", Username: "alice", Password: "secret",
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	local := filepath.Join(dir, "2026-11-22.jsonl.gz")
	content := []byte("gzip-content")
	os.WriteFile(local, content, 0o600)

	if err := target.Put(context.Background(), "2026-11-22.jsonl.gz", local); err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPut {
		t.Fatalf("应使用 PUT，实际 %s", gotMethod)
	}
	// 目录 URL 的路径 + key；prefix 由 service 层拼进 key，这里不该重复。
	if gotPath != "/bridge/2026-11-22.jsonl.gz" {
		t.Fatalf("路径不对: %s", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("应带 Basic Auth: %q", gotAuth)
	}
	if string(gotBody) != string(content) {
		t.Fatalf("内容不对: %q", gotBody)
	}
	if gotLen != int64(len(content)) {
		t.Fatalf("Content-Length 不对: %d", gotLen)
	}
}

// key 里已经带了前缀时，客户端不能再拼一次。
//
// 这是个真实踩过的 bug：service 拼 prefix、webdav 又拼一遍，
// 结果文件被传到 prefix/prefix/ 下面，看起来"上传成功"但位置不对。
func TestWebDAVDoesNotDoublePrefix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	target, err := NewWebDAV(WebDAVConfig{URL: srv.URL + "/bridge"}, "muse")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	local := filepath.Join(dir, "f.gz")
	os.WriteFile(local, []byte("x"), 0o600)

	// service 传进来的 key 已经是 "muse/f.gz"。
	if err := target.Put(context.Background(), "muse/f.gz", local); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bridge/muse/f.gz" {
		t.Fatalf("前缀不应重复，得到 %s", gotPath)
	}
	if strings.Contains(gotPath, "muse/muse") {
		t.Fatalf("出现重复前缀: %s", gotPath)
	}
}

// 201 / 200 / 204 都算上传成功，各家网盘返回不一样。
func TestWebDAVPutAcceptsStatuses(t *testing.T) {
	for _, status := range []int{200, 201, 204} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		target, _ := NewWebDAV(WebDAVConfig{URL: srv.URL}, "")
		dir := t.TempDir()
		local := filepath.Join(dir, "f.gz")
		os.WriteFile(local, []byte("x"), 0o600)
		if err := target.Put(context.Background(), "f.gz", local); err != nil {
			t.Fatalf("HTTP %d 应视为成功: %v", status, err)
		}
		srv.Close()
	}
}

func TestWebDAVDeleteNotFoundIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("应使用 DELETE，实际 %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	target, _ := NewWebDAV(WebDAVConfig{URL: srv.URL}, "")
	if err := target.Delete(context.Background(), "gone.gz"); err != nil {
		t.Fatalf("404 应视为成功: %v", err)
	}
}

// Ping 用 PROPFIND，并给出有针对性的错误提示。
func TestWebDAVPing(t *testing.T) {
	var status int
	var gotMethod, gotDepth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotDepth = r.Header.Get("Depth")
		w.WriteHeader(status)
	}))
	defer srv.Close()

	target, _ := NewWebDAV(WebDAVConfig{URL: srv.URL + "/bridge"}, "")

	status = http.StatusMultiStatus // 207
	if err := target.Ping(context.Background()); err != nil {
		t.Fatalf("207 应成功: %v", err)
	}
	if gotMethod != "PROPFIND" {
		t.Fatalf("应使用 PROPFIND，实际 %s", gotMethod)
	}
	if gotDepth != "0" {
		t.Fatalf("Depth 应为 0，实际 %q", gotDepth)
	}

	status = http.StatusUnauthorized
	err := target.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "用户名或密码") {
		t.Fatalf("401 应提示凭据错误，得到 %v", err)
	}

	status = http.StatusNotFound
	err = target.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "目录不存在") {
		t.Fatalf("404 应提示目录不存在，得到 %v", err)
	}

	status = http.StatusMethodNotAllowed
	err = target.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "WebDAV") {
		t.Fatalf("405 应提示不是 WebDAV 服务，得到 %v", err)
	}
}

func TestWebDAVConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  WebDAVConfig
	}{
		{"空地址", WebDAVConfig{}},
		{"缺 scheme", WebDAVConfig{URL: "dav.example.com/x"}},
		{"ftp scheme", WebDAVConfig{URL: "ftp://dav.example.com/x"}},
		{"缺主机", WebDAVConfig{URL: "https:///x"}},
	}
	for _, c := range cases {
		if _, err := NewWebDAV(c.cfg, ""); err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
	}
}

func TestWebDAVErrorMessages(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "用户名或密码"},
		{403, "没有权限"},
		{404, "目录不存在"},
		{507, "空间不足"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		}))
		target, _ := NewWebDAV(WebDAVConfig{URL: srv.URL}, "")
		dir := t.TempDir()
		local := filepath.Join(dir, "f.gz")
		os.WriteFile(local, []byte("x"), 0o600)
		err := target.Put(context.Background(), "f.gz", local)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("HTTP %d 应提示 %q，得到 %v", c.status, c.want, err)
		}
		srv.Close()
	}
}

func TestWebDAVName(t *testing.T) {
	target, _ := NewWebDAV(WebDAVConfig{URL: "https://dav.example.com/remote.php/dav/files/u/bridge/"}, "")
	name := target.Name()
	if !strings.Contains(name, "dav.example.com") {
		t.Fatalf("Name 应含主机名: %s", name)
	}
	if strings.Contains(name, "//") {
		t.Fatalf("Name 不应有双斜杠: %s", name)
	}
}
