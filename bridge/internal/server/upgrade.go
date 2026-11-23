package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"muse-bridge/internal/upgrade"
	"muse-bridge/internal/version"
)

// 升级页的数据。
type upgradeData struct {
	Status *upgrade.Status
	// Disabled 为 true 表示这个实例关掉了在线升级（BRIDGE_UPGRADE_DISABLED=1）。
	Disabled bool
	// AutoDisabled 表示当前版本号无法与发布号比较，自动升级不会生效。
	AutoDisabled bool
	// Version 是当前版本号，用于页头展示。
	Version string
}

// upgradeDisabled 统一处理"这个实例关掉了在线升级"。
//
// 入口在页面上是隐藏的，但所有写接口仍然要自己拒绝：
// 不能靠"用户看不到按钮"来保证安全。
func (s *Server) upgradeDisabled(w http.ResponseWriter, r *http.Request) {
	s.flashTo(w, "err", "该实例已关闭在线升级（BRIDGE_UPGRADE_DISABLED=1）")
	http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
}

// handleUpgradePage 展示当前版本、检查结果和定时升级设置。
func (s *Server) handleUpgradePage(w http.ResponseWriter, r *http.Request) {
	data := upgradeData{Version: version.Version, Disabled: s.up == nil}
	if s.up == nil {
		s.render(w, r, "upgrade.html", pageData{
			Title: "升级", Active: "upgrade", Admin: true, Data: data,
		})
		return
	}
	st, err := s.up.Status(r.Context())
	if err != nil {
		s.flashTo(w, "err", "读取升级状态失败："+err.Error())
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	data.Status = st
	data.AutoDisabled = !upgrade.IsComparable(version.Version)
	s.render(w, r, "upgrade.html", pageData{
		Title: "升级", Active: "upgrade", Admin: true, Data: data,
	})
}

// handleUpgradeCheck 立即检查一次更新。
func (s *Server) handleUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	if s.up == nil {
		s.upgradeDisabled(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	st, err := s.up.Check(ctx, true)
	if err != nil {
		s.flashTo(w, "err", "检查更新失败："+err.Error())
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	switch {
	case st.Latest == nil:
		s.flashTo(w, "ok", "没有找到适合当前平台的发布")
	case st.HasUpdate:
		s.flashTo(w, "ok", "发现新版本 "+st.Latest.Tag+"（当前 "+st.Current+"），可以点「立即升级」")
	default:
		s.flashTo(w, "ok", "已是最新版本（"+st.Current+"）")
	}
	http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
}

// handleUpgradeApply 下载并升级，成功后进程 exec 重启。
func (s *Server) handleUpgradeApply(w http.ResponseWriter, r *http.Request) {
	if s.up == nil {
		s.upgradeDisabled(w, r)
		return
	}
	_ = r.ParseForm()
	// 需要二次确认：升级会重启服务，正在处理的回复可能被打断。
	if r.PostFormValue("confirm") != "1" {
		s.flashTo(w, "err", "请先勾选确认再升级")
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	// 用独立的 context：升级要下载十几 MB，不能跟着浏览器断开就取消，
	// 否则会留下一个"下载了一半"的临时文件。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.up.Apply(ctx); err != nil {
		s.flashTo(w, "err", "升级失败："+err.Error())
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	// 正常情况下 Apply 成功就已经 exec 了，走不到这里。
	s.flashTo(w, "ok", "升级完成，正在重启")
	http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
}

// handleUpgradeConfig 保存定时升级设置。
func (s *Server) handleUpgradeConfig(w http.ResponseWriter, r *http.Request) {
	if s.up == nil {
		s.upgradeDisabled(w, r)
		return
	}
	_ = r.ParseForm()
	cfg := upgrade.Config{
		Channel:      r.PostFormValue("channel"),
		Auto:         r.PostFormValue("auto") == "1",
		At:           strings.TrimSpace(r.PostFormValue("at")),
		StartupCheck: r.PostFormValue("startup_check") == "1",
	}
	if cfg.At == "" {
		cfg.At = "04:00"
	}
	if err := s.up.SaveConfig(r.Context(), cfg); err != nil {
		s.flashTo(w, "err", "保存失败："+err.Error())
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	msg := "已保存升级设置"
	if cfg.Auto {
		msg += "：每天 " + cfg.At + " 自动检查并升级"
	}
	s.flashTo(w, "ok", msg)
	http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
}

// handleUpgradeRollback 用 .bak 恢复上一个版本。
//
// 只做文件替换，不重启：重启交给用户（或下次自然重启），
// 这样"回滚"这个动作本身不会打断正在处理的消息。
func (s *Server) handleUpgradeRollback(w http.ResponseWriter, r *http.Request) {
	if s.up == nil {
		s.upgradeDisabled(w, r)
		return
	}
	_ = r.ParseForm()
	if r.PostFormValue("confirm") != "1" {
		s.flashTo(w, "err", "请先勾选确认再回滚")
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	if err := s.up.Rollback(); err != nil {
		s.flashTo(w, "err", "回滚失败："+err.Error())
		http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
		return
	}
	s.flashTo(w, "ok", "已恢复上一个版本，重启后生效")
	http.Redirect(w, r, "/upgrade", http.StatusSeeOther)
}
