// Package server 暴露两组 HTTP 接口：
//
//   - 公网：Telegram webhook 和 Muse 接入 API，只做协议转换；
//   - 后台：单管理员登录后的管理界面，默认只绑 127.0.0.1，走 SSH 隧道访问。
//
// 两套接口用不同的监听地址，后台不出现在公网端口上。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"muse-bridge/internal/config"
	"muse-bridge/internal/domain"
	"muse-bridge/internal/secret"
	"muse-bridge/internal/store"
)

// Server 持有依赖并构造路由。
type Server struct {
	cfg *config.Config
	svc *domain.Service
	st  *store.Store
}

// New 构造 Server。
func New(cfg *config.Config, svc *domain.Service, st *store.Store) *Server {
	return &Server{cfg: cfg, svc: svc, st: st}
}

type ctxKey string

const (
	ctxBridge  ctxKey = "bridge"
	ctxTokenID ctxKey = "token_id"
)

func bridgeFrom(ctx context.Context) *store.Bridge {
	b, _ := ctx.Value(ctxBridge).(*store.Bridge)
	return b
}

func tokenIDFrom(ctx context.Context) int64 {
	v, _ := ctx.Value(ctxTokenID).(int64)
	return v
}

// writeJSON 输出 JSON，禁用缓存。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// readJSON 限制体积解析请求体。桥收到的 JSON 都很小，64KB 足够。
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// bearer 取出 Authorization: Bearer 后的令牌。
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// withBridge 校验桥令牌，把桥放进 context。
//
// 令牌只存摘要，这里做的是哈希反查；令牌无效一律 401，
// 不区分"没这个令牌"和"桥被暂停"，避免探测。
func (s *Server) withBridge(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "缺少 Bearer 令牌")
			return
		}
		b, tokenID, err := s.st.ResolveToken(r.Context(), hashToken(token))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "令牌无效或已吊销")
			return
		}
		if b.Status != "active" {
			writeErr(w, http.StatusForbidden, "该桥已暂停")
			return
		}
		s.st.TouchToken(r.Context(), tokenID, s.svc.Now())
		ctx := context.WithValue(r.Context(), ctxBridge, b)
		ctx = context.WithValue(ctx, ctxTokenID, tokenID)
		next(w, r.WithContext(ctx))
	}
}

// withAdmin 校验后台会话。
func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if !s.st.SessionValid(r.Context(), hashToken(c.Value), s.svc.Now()) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// isNotFound 判断是否为"记录不存在"。
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// hashToken 与 secret.HashToken 等价，单独包一层避免 handlers 到处引入 secret。
func hashToken(token string) string { return secret.HashToken(token) }
