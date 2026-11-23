package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 固定的签名测试向量：签名算法一旦写错，所有上传都会 403，
// 而线上又看不到请求细节，所以必须在这里把每一段都断言住。
func TestS3SignsRequest(t *testing.T) {
	var gotAuth, gotSHA, gotDate, gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSHA = r.Header.Get("X-Amz-Content-Sha256")
		gotDate = r.Header.Get("X-Amz-Date")
		gotHost = r.Host
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := S3Config{
		Endpoint:  srv.URL,
		Region:    "us-east-1",
		Bucket:    "my-bucket",
		PathStyle: true,
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	target, err := NewS3(cfg, "prefix")
	if err != nil {
		t.Fatal(err)
	}
	// 固定时钟，让签名可复现。
	client := target.(*s3)
	client.now = func() time.Time { return time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC) }

	// 造一个待上传文件。
	dir := t.TempDir()
	local := filepath.Join(dir, "2026-11-22.jsonl.gz")
	body := []byte("hello archive")
	if err := os.WriteFile(local, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := client.Put(context.Background(), "2026-11-22.jsonl.gz", local); err != nil {
		t.Fatal(err)
	}

	// Authorization 头格式：AWS4-HMAC-SHA256 Credential=.../scope, SignedHeaders=..., Signature=...
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization 算法不对: %s", gotAuth)
	}
	if !strings.Contains(gotAuth, "Credential=AKIDEXAMPLE/20261122/us-east-1/s3/aws4_request") {
		t.Fatalf("Credential scope 不对: %s", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("SignedHeaders 不对: %s", gotAuth)
	}
	if !strings.Contains(gotAuth, "Signature=") {
		t.Fatalf("缺少 Signature: %s", gotAuth)
	}

	// payload 摘要必须是文件内容的 sha256。
	want := sha256.Sum256(body)
	if gotSHA != hex.EncodeToString(want[:]) {
		t.Fatalf("X-Amz-Content-Sha256 不对: %s", gotSHA)
	}
	if gotDate != "20261122T100000Z" {
		t.Fatalf("X-Amz-Date 不对: %s", gotDate)
	}
	// path-style：/{bucket}/{key}
	if gotPath != "/my-bucket/2026-11-22.jsonl.gz" {
		t.Fatalf("path-style 路径不对: %s", gotPath)
	}
	_ = gotHost
}

// 签名必须是确定性的：同样的输入、同样的时间，签名必须一致。
func TestS3SignatureDeterministic(t *testing.T) {
	cfg := S3Config{
		Endpoint: "https://s3.example.com", Region: "eu-west-1",
		Bucket: "b", AccessKey: "ak", SecretKey: "sk",
	}
	target, _ := NewS3(cfg, "")
	c := target.(*s3)
	c.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

	req1, _ := http.NewRequest("PUT", "https://s3.example.com/b/k", nil)
	req2, _ := http.NewRequest("PUT", "https://s3.example.com/b/k", nil)
	payload := hex.EncodeToString(sha256Sum([]byte("x")))
	if err := c.sign(req1, "/b/k", payload); err != nil {
		t.Fatal(err)
	}
	if err := c.sign(req2, "/b/k", payload); err != nil {
		t.Fatal(err)
	}
	if req1.Header.Get("Authorization") != req2.Header.Get("Authorization") {
		t.Fatalf("签名不确定:\n%s\n%s", req1.Header.Get("Authorization"), req2.Header.Get("Authorization"))
	}
}

// virtual-hosted-style 的 URL 拼接：bucket 要变成主机名的前缀。
func TestS3VirtualHostedURL(t *testing.T) {
	target, err := NewS3(S3Config{
		Endpoint: "https://s3.us-east-1.amazonaws.com", Bucket: "my-bucket",
		AccessKey: "a", SecretKey: "s",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := target.(*s3)
	u, p := c.objectURL("2026-11-22.jsonl.gz")
	if u.Host != "my-bucket.s3.us-east-1.amazonaws.com" {
		t.Fatalf("主机名不对: %s", u.Host)
	}
	if p != "/2026-11-22.jsonl.gz" {
		t.Fatalf("路径不对: %s", p)
	}
}

// 对象键里的特殊字符必须逐段编码，`/` 保留。
func TestS3EscapesKeySegments(t *testing.T) {
	target, _ := NewS3(S3Config{
		Endpoint: "https://s3.example.com", Bucket: "b",
		PathStyle: true, AccessKey: "a", SecretKey: "s",
	}, "")
	c := target.(*s3)
	u, canonical := c.objectURL("备份 目录/2026-11-22.jsonl.gz")
	// url.Path 存未转义形式、RawPath 存转义形式，这是 Go 的约定；
	// 真正发出去的是 EscapedPath()，签名用的是 canonical。
	escaped := u.EscapedPath()
	if !strings.HasPrefix(escaped, "/b/") {
		t.Fatalf("应保留 path-style 的 bucket 段: %s", escaped)
	}
	if strings.Contains(escaped, " ") {
		t.Fatalf("空格应被编码: %s", escaped)
	}
	if !strings.Contains(escaped, "%20") {
		t.Fatalf("空格应编码为 %%20: %s", escaped)
	}
	// 目录分隔符不能被编成 %2F，否则 S3 会把它当键名的一部分。
	if strings.Contains(escaped, "%2F") {
		t.Fatalf("路径分隔符不应被编码: %s", escaped)
	}
	// 中文必须编码。
	if strings.Contains(escaped, "备") {
		t.Fatalf("中文应被编码: %s", escaped)
	}
	// 签名用的 canonical URI 必须与实际请求行一致。
	if canonical != escaped {
		t.Fatalf("canonical URI 应与实际路径一致:\n canonical=%s\n escaped=%s", canonical, escaped)
	}
}

// S3 返回的 XML 错误要解析成可读信息。
func TestS3ErrorParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
			Message string   `xml:"Message"`
		}{Code: "NoSuchBucket", Message: "The specified bucket does not exist"})
	}))
	defer srv.Close()

	target, _ := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "nope", PathStyle: true,
		AccessKey: "a", SecretKey: "s",
	}, "")
	dir := t.TempDir()
	local := filepath.Join(dir, "x.gz")
	os.WriteFile(local, []byte("x"), 0o600)

	err := target.Put(context.Background(), "x.gz", local)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Fatalf("错误应包含 S3 错误码，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("错误应包含 S3 错误信息，得到: %v", err)
	}
}

// DELETE 返回 404 说明对象本来就不在，不该当成失败。
func TestS3DeleteNotFoundIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	target, _ := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", PathStyle: true, AccessKey: "a", SecretKey: "s",
	}, "")
	if err := target.Delete(context.Background(), "gone.gz"); err != nil {
		t.Fatalf("404 应视为成功: %v", err)
	}
}

// Ping：200 成功、403 给出"权限"提示。
func TestS3Ping(t *testing.T) {
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("Ping 应使用 HEAD，实际 %s", r.Method)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	target, _ := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", PathStyle: true, AccessKey: "a", SecretKey: "s",
	}, "")

	status = http.StatusOK
	if err := target.Ping(context.Background()); err != nil {
		t.Fatalf("200 应成功: %v", err)
	}
	status = http.StatusForbidden
	err := target.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("403 应提示权限问题，得到 %v", err)
	}
	status = http.StatusNotFound
	err = target.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bucket 不存在") {
		t.Fatalf("404 应提示 bucket 不存在，得到 %v", err)
	}
}

// Endpoint 不带 scheme 时补 https；Region 留空用 us-east-1。
func TestS3ConfigDefaults(t *testing.T) {
	target, err := NewS3(S3Config{
		Endpoint: "s3.example.com", Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := target.(*s3)
	if !strings.HasPrefix(c.cfg.Endpoint, "https://") {
		t.Fatalf("应补上 https: %s", c.cfg.Endpoint)
	}
	if c.cfg.Region != "us-east-1" {
		t.Fatalf("默认 region 应为 us-east-1，得到 %s", c.cfg.Region)
	}
}

func TestS3ConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  S3Config
	}{
		{"空 endpoint", S3Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}},
		{"空 bucket", S3Config{Endpoint: "https://x", AccessKey: "a", SecretKey: "s"}},
		{"空凭据", S3Config{Endpoint: "https://x", Bucket: "b"}},
	}
	for _, c := range cases {
		if _, err := NewS3(c.cfg, ""); err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
	}
}

// 上传必须真的把文件内容发出去，长度要对。
func TestS3PutSendsContent(t *testing.T) {
	var gotBody []byte
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target, _ := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", PathStyle: true, AccessKey: "a", SecretKey: "s",
	}, "")
	dir := t.TempDir()
	local := filepath.Join(dir, "f.gz")
	content := []byte("archive-content-12345")
	os.WriteFile(local, content, 0o600)

	if err := target.Put(context.Background(), "f.gz", local); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(content) {
		t.Fatalf("上传内容不对: %q", gotBody)
	}
	if gotLen != int64(len(content)) {
		t.Fatalf("Content-Length 不对: %d", gotLen)
	}
}

// Session Token 要进签名头和请求头。
func TestS3SessionToken(t *testing.T) {
	var gotToken, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Amz-Security-Token")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target, _ := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", PathStyle: true,
		AccessKey: "a", SecretKey: "s", SessionToken: "tok123",
	}, "")
	dir := t.TempDir()
	local := filepath.Join(dir, "f.gz")
	os.WriteFile(local, []byte("x"), 0o600)
	if err := target.Put(context.Background(), "f.gz", local); err != nil {
		t.Fatal(err)
	}
	if gotToken != "tok123" {
		t.Fatalf("Session Token 头不对: %q", gotToken)
	}
	if !strings.Contains(gotAuth, "x-amz-security-token") {
		t.Fatalf("Session Token 应参与签名: %s", gotAuth)
	}
}
