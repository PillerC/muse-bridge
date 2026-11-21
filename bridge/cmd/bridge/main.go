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

	// 管理员密码以环境变量为准：改了环境变量重启即生效。
	now := time.Now().Unix()
	if err := st.UpsertAdmin(context.Background(), cfg.AdminUser,
		secret.HashPassword(cfg.AdminPassword), now); err != nil {
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
		log.Printf("管理后台监听 http://%s（建议通过 SSH 隧道访问）", cfg.Addr)
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
	os.Exit(0)
}
