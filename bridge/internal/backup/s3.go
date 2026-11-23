package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// s3 是 S3 兼容对象存储的客户端。
//
// 为什么自己实现 SigV4 而不引 aws-sdk-go：这个项目是单文件二进制，
// SDK 会给它加十几 MB，而我们只需要 PUT / DELETE / HEAD 三个操作。
// SigV4 本身只有几十行，签名逻辑也容易用固定向量验证。
type s3 struct {
	cfg    S3Config
	prefix string
	client *http.Client
	// now 供测试注入固定时间，签名里的日期必须可预测才能断言。
	now func() time.Time
}

// NewS3 构造 S3 目标。
func NewS3(cfg S3Config, prefix string) (Target, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("S3 Endpoint 不能为空")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("S3 Bucket 不能为空")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 Access Key 和 Secret Key 都不能为空")
	}
	ep := cfg.Endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("S3 Endpoint 不是合法 URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3 Endpoint 缺少主机名: %s", cfg.Endpoint)
	}
	cfg.Endpoint = strings.TrimRight(ep, "/")
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &s3{
		cfg:    cfg,
		prefix: prefix,
		client: &http.Client{Timeout: 5 * time.Minute},
		now:    time.Now,
	}, nil
}

func (s *s3) Name() string {
	return "S3: " + joinKey(s.cfg.Bucket, s.prefix)
}

// objectURL 拼出对象的完整地址与 SigV4 用的 canonical URI。
//
// PathStyle：{endpoint}/{bucket}/{key}
// VirtualHosted：{scheme}://{bucket}.{host}/{key}
//
// 返回的 canonicalURI 是**已转义**的路径，签名必须用它；
// 而 url.URL 的 Path 字段放未转义版本、RawPath 放转义版本，
// 这样 Go 拼请求行时不会再编码一次（双重编码会让 S3 找不到对象）。
func (s *s3) objectURL(key string) (*url.URL, string) {
	base, _ := url.Parse(s.cfg.Endpoint)
	host := base.Host
	prefix := strings.TrimSuffix(base.Path, "/")

	if !s.cfg.PathStyle {
		if h, p, err := splitHostPort(base.Host); err == nil {
			host = s.cfg.Bucket + "." + h
			if p != "" {
				host += ":" + p
			}
		}
	} else {
		prefix += "/" + s.cfg.Bucket
	}

	var decoded string
	if key == "" {
		decoded = prefix + "/"
	} else {
		decoded = prefix + "/" + key
	}

	escaped := escapePath(decoded)
	u := &url.URL{Scheme: base.Scheme, Host: host, Path: decoded, RawPath: escaped}
	return u, escaped
}

// splitHostPort 拆主机与端口；没有端口时返回空端口。
func splitHostPort(host string) (string, string, error) {
	i := strings.LastIndex(host, ":")
	if i < 0 {
		return host, "", nil
	}
	// 处理 IPv6 [::1]:9000
	if strings.HasPrefix(host, "[") {
		j := strings.LastIndex(host, "]")
		if j < 0 {
			return "", "", errors.New("IPv6 地址格式错误")
		}
		if j+1 < len(host) && host[j+1] == ':' {
			return host[:j+1], host[j+2:], nil
		}
		return host, "", nil
	}
	return host[:i], host[i+1:], nil
}

// escapePath 对路径逐段做 URI 编码，保留分隔符 `/`。
//
// 不能直接 url.QueryEscape 整条：那会把 `/` 也编成 %2F，
// S3 会把它当成键名的一部分，最终存到一个奇怪的地方。
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// uriEncode 按 SigV4 规范做 URI 编码：除 unreserved 字符外全部 %XX 大写。
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Put 上传本地文件。
//
// 需要读两遍：先算 payload 的 sha256 用于签名，再 Seek(0) 上传内容。
// 这样不必把整个文件读进内存（归档文件可能几十 MB）。
func (s *s3) Put(ctx context.Context, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("读取文件失败: %w", err)
	}
	payloadHash := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	u, canonicalURI := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/gzip")

	if err := s.sign(req, canonicalURI, payloadHash); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error(resp, "上传失败")
	}
	return nil
}

// Delete 删除对象。对象本来就不存在不算错误。
func (s *s3) Delete(ctx context.Context, key string) error {
	u, canonicalURI := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	emptyHash := hex.EncodeToString(sha256Sum(nil))
	if err := s.sign(req, canonicalURI, emptyHash); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return s3Error(resp, "删除失败")
	}
	return nil
}

// Ping 探测 bucket 是否可访问。
//
// 200 说明完全可用；403 说明服务可达但凭据没权限（或 bucket 不存在时
// AWS 也返回 403），这种情况要说清楚，否则用户只会看到"连接失败"。
func (s *s3) Ping(ctx context.Context) error {
	u, canonicalURI := s.objectURL("")
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return err
	}
	emptyHash := hex.EncodeToString(sha256Sum(nil))
	if err := s.sign(req, canonicalURI, emptyHash); err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("连接 S3 失败: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return errors.New("能连上 S3 服务，但凭据没有访问该 bucket 的权限（检查 Access Key、Secret Key 和 bucket 名）")
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("bucket 不存在（检查 bucket 名和 region）")
	default:
		return s3Error(resp, "连接 S3 失败")
	}
}

// sign 计算并写入 SigV4 签名头。
func (s *s3) sign(req *http.Request, canonicalURI, payloadHash string) error {
	now := s.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.cfg.SessionToken)
	}

	// 参与签名的头必须包含 host。
	headers := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if s.cfg.SessionToken != "" {
		headers["x-amz-security-token"] = s.cfg.SessionToken
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(headers[k]))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery(req.URL),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.cfg.Region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), dateStamp)
	key = hmacSHA256(key, s.cfg.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, scope, signedHeaders, signature))
	return nil
}

// canonicalQuery 按 SigV4 要求对查询参数排序并编码。
func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	vals := u.Query()
	type kv struct{ k, v string }
	var list []kv
	for k, vs := range vals {
		for _, v := range vs {
			list = append(list, kv{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].k != list[j].k {
			return list[i].k < list[j].k
		}
		return list[i].v < list[j].v
	})
	parts := make([]string, len(list))
	for i, p := range list {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// s3Error 把 S3 的 XML 错误体解析成中文错误。
func s3Error(resp *http.Response, prefix string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	detail := ""
	if err := xml.Unmarshal(bytes.TrimSpace(body), &e); err == nil && e.Code != "" {
		detail = e.Code
		if e.Message != "" {
			detail += ": " + e.Message
		}
	}
	if detail == "" {
		return fmt.Errorf("%s（HTTP %d）", prefix, resp.StatusCode)
	}
	return fmt.Errorf("%s（%s）", prefix, detail)
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
