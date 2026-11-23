package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// webdav 是 WebDAV 网盘客户端。
//
// 只用 PUT / DELETE / PROPFIND 三个方法：备份场景只需要"传上去、能删、能测通"，
// 建目录这类操作交给用户在自己的网盘里完成——替用户在他网盘里建目录
// 是越权行为，而且各家对 MKCOL 的支持也不一致。
type webdav struct {
	cfg    WebDAVConfig
	prefix string
	client *http.Client
}

// NewWebDAV 构造 WebDAV 目标。
func NewWebDAV(cfg WebDAVConfig, prefix string) (Target, error) {
	if cfg.URL == "" {
		return nil, errors.New("WebDAV 地址不能为空")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("WebDAV 地址不是合法 URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("WebDAV 地址必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return nil, errors.New("WebDAV 地址缺少主机名")
	}
	return &webdav{
		cfg:    cfg,
		prefix: prefix,
		client: &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

func (w *webdav) Name() string {
	u, _ := url.Parse(w.cfg.URL)
	return "WebDAV: " + u.Host + normalizePath(u.Path)
}

// objectURL 拼出对象地址。
//
// 目录 URL 结尾有没有斜杠都要正确：用户从网盘复制地址时经常带一个斜杠，
// 直接拼会得到 `.../bridge//2026-11-20.jsonl.gz`，有些服务端会当成不同路径。
//
// 注意这里**不再拼 prefix**：调用方传进来的 key 已经是"前缀 + 文件名"。
// 两层都拼会得到 prefix/prefix/文件，这是个很难发现的 bug（文件传上去了，
// 只是位置不对），所以前缀只在 service 那一层处理一次。
func (w *webdav) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(w.cfg.URL)
	if err != nil {
		return nil, err
	}
	dir := normalizePath(u.Path)
	segs := strings.Split(escapePath(key), "/")
	u.Path = dir + "/" + strings.Join(segs, "/")
	u.RawPath = ""
	return u, nil
}

// normalizePath 保证路径以单个斜杠开头、不以斜杠结尾。
func normalizePath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func (w *webdav) setAuth(req *http.Request) {
	if w.cfg.Username != "" || w.cfg.Password != "" {
		req.SetBasicAuth(w.cfg.Username, w.cfg.Password)
	}
}

// Put 上传文件。
//
// 用文件句柄而不是字节切片：归档文件可能几十 MB，
// 交给 http 流式发送即可，不必读进内存。
func (w *webdav) Put(ctx context.Context, key, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("打开本地文件失败: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	u, err := w.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/gzip")
	w.setAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("上传失败: %w", err)
	}
	defer resp.Body.Close()
	// 200/201/204 都算成功：不同网盘返回的不一样。
	if resp.StatusCode/100 != 2 {
		return webdavError(resp, "上传失败")
	}
	return nil
}

// Delete 删除文件。404 算成功（本来就没有）。
func (w *webdav) Delete(ctx context.Context, key string) error {
	u, err := w.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	w.setAuth(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("删除失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return webdavError(resp, "删除失败")
	}
	return nil
}

// Ping 用 PROPFIND 探测目录是否可访问。
func (w *webdav) Ping(ctx context.Context) error {
	u, err := url.Parse(w.cfg.URL)
	if err != nil {
		return err
	}
	dir := normalizePath(u.Path)
	u.Path = dir + "/"
	u.RawPath = ""

	// PROPFIND Depth:0 只查目录本身，代价最小。
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", u.String(), strings.NewReader(
		`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`))
	if err != nil {
		return err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	w.setAuth(req)

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("连接 WebDAV 失败: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch resp.StatusCode {
	case http.StatusMultiStatus, http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return errors.New("用户名或密码不对（HTTP 401）")
	case http.StatusForbidden:
		return errors.New("凭据没有权限访问该目录（HTTP 403）")
	case http.StatusNotFound:
		return errors.New("目录不存在（HTTP 404），请先在网盘里创建它")
	case http.StatusMethodNotAllowed:
		return errors.New("该地址不是 WebDAV 服务（不支持 PROPFIND）")
	default:
		return fmt.Errorf("连接 WebDAV 失败（HTTP %d）", resp.StatusCode)
	}
}

// webdavError 把响应转成中文错误。
func webdavError(resp *http.Response, prefix string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	msg := strings.TrimSpace(string(body))
	// 有的服务端会在错误页里塞整页 HTML，对用户没意义，截断即可。
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s（用户名或密码不对）", prefix)
	case http.StatusForbidden:
		return fmt.Errorf("%s（没有权限）", prefix)
	case http.StatusNotFound:
		return fmt.Errorf("%s（目录不存在）", prefix)
	case http.StatusInsufficientStorage:
		return fmt.Errorf("%s（网盘空间不足）", prefix)
	}
	if msg == "" {
		return fmt.Errorf("%s（HTTP %d）", prefix, resp.StatusCode)
	}
	return fmt.Errorf("%s（HTTP %d: %s）", prefix, resp.StatusCode, msg)
}
