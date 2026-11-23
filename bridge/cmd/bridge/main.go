// Command bridge 是 Muse ↔ Telegram 的桥：一个进程、一个 SQLite 文件，
// 同时提供 Telegram webhook、Muse 接入 API 和本机管理后台。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"muse-bridge/internal/backup"
	"muse-bridge/internal/config"
	"muse-bridge/internal/domain"
	"muse-bridge/internal/poller"
	"muse-bridge/internal/secret"
	"muse-bridge/internal/server"
	"muse-bridge/internal/store"
	"muse-bridge/internal/tg"
	"muse-bridge/internal/upgrade"
	"muse-bridge/internal/version"
)

// buildVersion 是启动日志里显示的版本号，来源是 internal/version。
var buildVersion = version.Version

func main() {
	// --version / version 子命令：安装脚本和运维要用它判断当前版本，
	// 不必去翻日志或者解析二进制。必须在加载配置之前处理，
	// 否则只想看版本的用户会先被"BRIDGE_SECRET 未设置"挡住。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			fmt.Printf("muse-bridge %s\n", buildVersion)
			if version.Commit != "" {
				fmt.Printf("commit %s\n", version.Commit)
			}
			return
		}
	}

	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[bridge] ")
	log.Printf("Muse Bridge %s 启动", buildVersion)

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

	// 在线升级：查 GitHub Releases、下载、校验、原地替换后 exec 重启。
	// BRIDGE_UPGRADE_DISABLED=1 时整块不启用（页面也不给入口）。
	var up *upgrade.Service
	if !cfg.UpgradeDisabled {
		up = upgrade.NewService(st, upgrade.Options{
			Repo:    cfg.UpgradeRepo,
			APIBase: cfg.UpgradeAPI,
			Current: version.Version,
			Commit:  version.Commit,
		})
		// 重启前停掉"输入中"保活，否则 exec 之后 Telegram 那边还在转圈。
		up.SetBeforeRestart(svc.Typing.StopAll)
		// 启动时先确认上一次升级有没有真的生效。
		up.ConfirmStartup(context.Background())
	}

	// 归档与在线备份：每天把聊天记录切成独立文件，可上传到 S3/WebDAV，
	// 并按保留天数清理本地。BRIDGE_BACKUP_DISABLED=1 时整块不启用。
	var bk *backup.Service
	if !cfg.BackupDisabled {
		bk = backup.NewService(st, st, box, st.DB(), cfg.ArchiveDir, time.Local)
	}

	srv := server.New(cfg, svc, st)
	srv.SetUpgrader(up)
	srv.SetBackup(bk)

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

	if up != nil {
		go up.Run(ctx)
	}
	if bk != nil {
		go backup.RunDaily(ctx, bk)
	}

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
// 正常情况什么都不做：账号由首次访问时的初始化页创建（/setup）。
// 环境变量不再是"建号"的来源，也不是密码的真相源——否则网页上刚改的
// 密码下次重启就被顶回去。它只剩一个用途：忘了密码时，
// 设 BRIDGE_ADMIN_RESET=1 用 env 里的账号密码强制覆盖回来。
func bootstrapAdmin(ctx context.Context, st *store.Store, cfg *config.Config) error {
	if cfg.AdminReset {
		now := time.Now().Unix()
		hash := secret.HashPassword(cfg.AdminPassword)
		log.Printf("BRIDGE_ADMIN_RESET=1：用环境变量覆盖账号 %s 的密码", cfg.AdminUser)
		return st.UpsertAdmin(ctx, cfg.AdminUser, hash, now)
	}
	n, err := st.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		log.Printf("还没有管理员账号，访问首页会引导创建（/setup）")
	}
	return nil
}
