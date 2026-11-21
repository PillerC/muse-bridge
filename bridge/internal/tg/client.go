package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// APIError 是 Telegram 返回的业务错误（HTTP 200 但 ok=false，或 4xx/5xx 带描述）。
type APIError struct {
	Method      string
	Code        int // Telegram 的 error_code
	Description string
	RetryAfter  int // 429 时的 retry_after
	HTTPStatus  int
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("telegram %s: %d %s (retry_after=%ds)", e.Method, e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Permanent 表示不应重试的错误（比如 chat 不存在、bot 被踢）。
// 429 / 5xx / 网络错误可重试，其余按永久错误处理。
func (e *APIError) Permanent() bool {
	switch {
	case e.Code == 429:
		return false
	case e.Code >= 500:
		return false
	case e.HTTPStatus >= 500:
		return false
	default:
		return true
	}
}

// Factory 复用连接池，按 bot token 生成轻量客户端。
type Factory struct {
	http    *http.Client
	baseURL string
	mu      sync.Mutex
	cache   map[string]*Client
}

// NewFactory 构造工厂。proxy 为空表示直连（可用 http:// 或 socks5://）。
func NewFactory(proxy string, timeout time.Duration) (*Factory, error) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("BRIDGE_TG_PROXY 不是合法 URL: %w", err)
		}
		switch u.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		default:
			return nil, fmt.Errorf("BRIDGE_TG_PROXY 目前只支持 http/https 代理，当前 %q", u.Scheme)
		}
	}
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	return &Factory{
		http:    &http.Client{Transport: transport, Timeout: timeout},
		baseURL: "https://api.telegram.org",
		cache:   make(map[string]*Client),
	}, nil
}

// SetBaseURL 仅用于测试：把请求指到本地假 Telegram。
func (f *Factory) SetBaseURL(u string) { f.baseURL = strings.TrimRight(u, "/") }

// For 返回某个 bot token 对应的客户端。
func (f *Factory) For(token string) *Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cache[token]; ok {
		return c
	}
	c := &Client{factory: f, token: token}
	f.cache[token] = c
	return c
}

// Client 绑定一个 bot token。
type Client struct {
	factory *Factory
	token   string
}

type apiResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Call 调用任意 Bot API 方法，把 result 解到 out（out 可为 nil）。
func (c *Client) Call(ctx context.Context, method string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	endpoint := c.factory.baseURL + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.factory.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 telegram 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var ar apiResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return &APIError{Method: method, HTTPStatus: resp.StatusCode,
			Description: "响应不是 JSON: " + truncate(string(raw), 200)}
	}
	if !ar.OK {
		code := ar.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Method: method, Code: code, HTTPStatus: resp.StatusCode,
			Description: ar.Description, RetryAfter: ar.Parameters.RetryAfter}
	}
	if out != nil && len(ar.Result) > 0 {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("解析 %s 返回失败: %w", method, err)
		}
	}
	return nil
}

// BotInfo 是 getMe 的返回。
type BotInfo struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// GetMe 校验 token 并取回 bot 身份。建桥时就靠它判断 token 是否有效。
func (c *Client) GetMe(ctx context.Context) (*BotInfo, error) {
	var info BotInfo
	if err := c.Call(ctx, "getMe", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// SetWebhook 把某座桥的 webhook 指向桥自己。
func (c *Client) SetWebhook(ctx context.Context, publicURL, secret string) error {
	payload := map[string]any{
		"url":                  publicURL,
		"secret_token":         secret,
		"allowed_updates":      []string{"message", "edited_message"},
		"drop_pending_updates": false,
		"max_connections":      40,
	}
	return c.Call(ctx, "setWebhook", payload, nil)
}

// DeleteWebhook 断开 webhook，切回长轮询时用。
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.Call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// WebhookInfo 是 getWebhookInfo 的返回。
//
// 排查"消息没进来"时最有用的是 PendingCount 和 LastErrorMessage：
// 前者说明 Telegram 打了但桥回了非 2xx，后者通常是证书或 404。
type WebhookInfo struct {
	URL              string `json:"url"`
	HasCustomCert    bool   `json:"has_custom_certificate"`
	PendingCount     int    `json:"pending_update_count"`
	LastErrorDate    int64  `json:"last_error_date"`
	LastErrorMessage string `json:"last_error_message"`
	MaxConnections   int    `json:"max_connections"`
	IPAddress        string `json:"ip_address"`
}

// GetWebhookInfo 查询 Telegram 侧记录的 webhook 状态。
func (c *Client) GetWebhookInfo(ctx context.Context) (*WebhookInfo, error) {
	var info WebhookInfo
	if err := c.Call(ctx, "getWebhookInfo", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Update 是 getUpdates 的一页。
type UpdatesPage struct {
	Updates []Update
}

// GetUpdates 拉一页更新。offset=0 表示从服务端未确认的第一条开始。
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	payload := map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message", "edited_message"},
	}
	var ups []Update
	if err := c.Call(ctx, "getUpdates", payload, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// SentMessage 是 sendMessage / editMessageText 的返回里我们会用到的字段。
type SentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
	Date      int64 `json:"date"`
}

// SendMessage 发文本。parseMode 为空表示纯文本。
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64, parseMode string) (*SentMessage, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]any{
			"message_id":                  replyTo,
			"allow_sending_without_reply": true,
		}
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
		payload["link_preview_options"] = map[string]any{"is_disabled": true}
	}
	var out SentMessage
	if err := c.Call(ctx, "sendMessage", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EditMessageText 编辑已发送的文本。
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text, parseMode string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
		payload["link_preview_options"] = map[string]any{"is_disabled": true}
	}
	return c.Call(ctx, "editMessageText", payload, nil)
}

// SendChatAction 发 typing 等状态。失败不影响主流程，调用方可忽略错误。
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.Call(ctx, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  action,
	}, nil)
}

// AnswerCallbackQuery 留作后续按钮交互，当前未使用。
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	return c.Call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              text,
	}, nil)
}

// IsPermanent 判断错误是否不该重试。
func IsPermanent(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Permanent()
	}
	return false
}

// RetryAfter 返回 429 建议的等待秒数。
func RetryAfter(err error) int {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
