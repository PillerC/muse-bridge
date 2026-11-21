// Command bridge 是 Muse ↔ Telegram 的桥：一个进程、一个 SQLite 文件，
// 同时提供 Telegram webhook、Muse 接入 API 和本机管理后台。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"muse-bridge/internal/config"
	"muse-bridge/internal/domain"
	"muse-bridge/internal/poller"
	"muse-bridge/internal/secret"
	"muse-bridge/internal/server"
	"muse-bridge/internal/store"
	"muse-bridge/internal/tg"
)

// version 由 -ldflags 注入。
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[bridge] ")
	log.Printf("Muse Bridge %s 启动", version)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("数据库错误: %v", err)
	}
	defer st.Close()

	// 账号以库为准：env 只在首次创建（或 BRIDGE_ADMIN_RESET=1 显式重置）时生效。
	if err := bootstrapAdmin(context.Background(), st, cfg); err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	}

	box, err := secret.NewBox(cfg.Secret)
	if err != nil {
		log.Fatalf("主密钥错误: %v", err)
	}
	factory, err := tg.NewFactory(cfg.TGProxy, 35*time.Second)
	if err != nil {
		log.Fatalf("Telegram 客户端错误: %v", err)
	}
	queue := domain.NewQueue(st, cfg.LeaseSeconds, cfg.MaxAttempts)
	svc := domain.NewService(st, queue, box, factory, cfg.PublicURL)
	svc.Typing.Enabled = cfg.Typing
	srv := server.New(cfg, svc, st)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 后台巡检：即使没有任何请求进来，过期租约也会被回收。
	// v1 的租约死锁正是因为回收只挂在 /poll 上。
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := queue.Reap(ctx, time.Now().Unix()); err != nil {
					log.Printf("回收过期租约失败: %v", err)
				} else if n > 0 {
					log.Printf("回收过期租约 %d 条", n)
				}
			}
		}
	}()

	go poller.New(st, svc).Run(ctx)

	adminSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Admin(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	publicSrv := &http.Server{
		Addr:              cfg.PublicAddr,
		Handler:           srv.Public(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() {
		log.Printf("管理后台兜底监听 http://%s（公网入口 %s 即后台首页）", cfg.Addr, cfg.PublicURL)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	switch cfg.TLSMode {
	case "autocert":
		mgr := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Host()),
			Cache:      autocert.DirCache(cfg.CertDir()),
			Email:      cfg.ACMEEmail,
		}
		publicSrv.TLSConfig = &tls.Config{
			GetCertificate: mgr.GetCertificate,
			MinVersion:     tls.VersionTLS12,
			// 带上 acme-tls/1，让 TLS-ALPN-01 挑战也能被受理：
			// 80 端口哪天被挡了，证书续期还有第二条路。
			NextProtos: []string{"h2", "http/1.1", "acme-tls/1"},
		}
		go func() {
			log.Printf("公网监听 https://%s（autocert，域名 %s）", cfg.PublicAddr, cfg.Host())
			if err := publicSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
		// 80 端口只用于 ACME HTTP-01 挑战和跳转。
		go func() {
			h := mgr.HTTPHandler(nil)
			s := &http.Server{Addr: ":80", Handler: h, ReadHeaderTimeout: 10 * time.Second}
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("80 端口监听失败（证书签发可能受影响）: %v", err)
			}
		}()
	case "cert":
		publicSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		go func() {
			log.Printf("公网监听 https://%s（证书 %s）", cfg.PublicAddr, cfg.TLSCert)
			if err := publicSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	default:
		go func() {
			log.Printf("公网监听 http://%s（未启用 TLS，仅供反代前置时使用）", cfg.PublicAddr)
			if err := publicSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Printf("收到退出信号，正在关闭…")
	case err := <-errCh:
		log.Printf("监听失败: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(shutdownCtx)
	_ = publicSrv.Shutdown(shutdownCtx)
	// 停掉所有"输入中"保活，否则进程走了 Telegram 那边还在转圈。
	svc.Typing.StopAll()
	os.Exit(0)
}

// bootstrapAdmin 决定这次启动要不要动管理员账号。
//
// 后台开放了改密码，所以 env 不能继续"每次启动都覆盖"——那样网页上刚改的
// 密码下次重启就没了。现在：库里没有这个账号才用 env 建；BRIDGE_ADMIN_RESET=1
// 才用 env 强制覆盖（忘记密码时的唯一退路）。
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg *config.Config) error {
	now := time.Now().Unix()
	hash := secret.HashPassword(cfg.AdminPassword)
	if cfg.AdminReset {
		log.Printf("BRIDGE_ADMIN_RESET=1：用环境变量覆盖账号 %s 的密码", cfg.AdminUser)
		return st.UpsertAdmin(ctx, cfg.AdminUser, hash, now)
	}
	if err := st.EnsureAdmin(ctx, cfg.AdminUser, hash, now); err != nil {
		return err
	}
	// 账号在、但 env 里的密码跟库里的不一致，基本就是网页上改过密码。
	// 这里不改库，只留一句提示，免得"我改了 env 怎么没生效"变成无头案。
	if a, err := st.GetAdmin(ctx, cfg.AdminUser); err == nil && !secret.VerifyPassword(a.PasswordHash, cfg.AdminPassword) {
		log.Printf("提示：环境变量里的密码与库中 %s 的密码不一致，已按库中的为准；"+
			"要强制用 env 覆盖请设 BRIDGE_ADMIN_RESET=1 再重启", cfg.AdminUser)
	}
	return nil
}
