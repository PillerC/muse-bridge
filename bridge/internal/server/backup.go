package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"muse-bridge/internal/backup"
)

// 备份页数据。
type backupData struct {
	Status *backup.Status
	// Disabled 表示该实例关掉了备份（BRIDGE_BACKUP_DISABLED=1）。
	Disabled bool
	// SecretSet 表示库里已有 S3 Secret Key（表单不回显，只提示"已设置"）。
	SecretSet bool
	// PassSet 表示库里已有 WebDAV 密码。
	PassSet bool
	// DefaultKeep 是保留天数的推荐值。
	DefaultKeep int
}

// handleBackupPage 展示归档、备份与保留设置。
func (s *Server) handleBackupPage(w http.ResponseWriter, r *http.Request) {
	if s.bk == nil {
		s.render(w, r, "backup.html", pageData{
			Title: "备份", Active: "backup", Admin: true,
			Data: backupData{Disabled: true},
		})
		return
	}
	st, err := s.bk.Status(r.Context())
	if err != nil {
		s.flashTo(w, "err", "读取备份状态失败："+err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cfg := st.Config
	s.render(w, r, "backup.html", pageData{
		Title: "备份", Active: "backup", Admin: true,
		Data: backupData{
			Status: st, SecretSet: cfg.S3.SecretKey != "",
			PassSet: cfg.WebDAV.Password != "", DefaultKeep: 30,
		},
	})
}

// backupConfigFromForm 从表单读配置。
//
// 凭据留空表示"不修改"：后台不回显已保存的密钥，
// 用户只改别的字段时不该被迫重新输入一遍密码。
func (s *Server) backupConfigFromForm(r *http.Request) (backup.Config, error) {
	_ = r.ParseForm()
	cur, err := s.bk.LoadConfig(r.Context())
	if err != nil {
		return backup.Config{}, err
	}
	cfg := backup.Config{
		Enabled:  r.PostFormValue("enabled") == "1",
		Type:     r.PostFormValue("type"),
		Prefix:   strings.TrimSpace(r.PostFormValue("prefix")),
		Retain:   r.PostFormValue("retain") == "1",
		KeepDays: cur.KeepDays,
	}
	if v := strings.TrimSpace(r.PostFormValue("keep_days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return cfg, errInvalidKeepDays
		}
		cfg.KeepDays = n
	}

	cfg.S3 = backup.S3Config{
		Endpoint:  strings.TrimSpace(r.PostFormValue("s3_endpoint")),
		Region:    strings.TrimSpace(r.PostFormValue("s3_region")),
		Bucket:    strings.TrimSpace(r.PostFormValue("s3_bucket")),
		PathStyle: r.PostFormValue("s3_path_style") == "1",
		AccessKey: strings.TrimSpace(r.PostFormValue("s3_access_key")),
		SecretKey: cur.S3.SecretKey, // 默认沿用已保存的
	}
	if v := r.PostFormValue("s3_secret_key"); v != "" {
		cfg.S3.SecretKey = v
	}
	if v := r.PostFormValue("s3_session_token"); v != "" {
		cfg.S3.SessionToken = v
	}

	cfg.WebDAV = backup.WebDAVConfig{
		URL:      strings.TrimSpace(r.PostFormValue("webdav_url")),
		Username: strings.TrimSpace(r.PostFormValue("webdav_user")),
		Password: cur.WebDAV.Password,
	}
	if v := r.PostFormValue("webdav_password"); v != "" {
		cfg.WebDAV.Password = v
	}
	return cfg, nil
}

var errInvalidKeepDays = errString("保留天数必须是数字")

type errString string

func (e errString) Error() string { return string(e) }

// handleBackupSave 保存配置。
func (s *Server) handleBackupSave(w http.ResponseWriter, r *http.Request) {
	if s.bk == nil {
		s.backupDisabled(w, r)
		return
	}
	cfg, err := s.backupConfigFromForm(r)
	if err != nil {
		s.flashTo(w, "err", "保存失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	if err := s.bk.SaveConfig(r.Context(), cfg); err != nil {
		s.flashTo(w, "err", "保存失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	msg := "已保存"
	if cfg.Retain && cfg.KeepDays > 0 {
		msg += "：本地只保留最近 " + strconv.Itoa(cfg.KeepDays) + " 天，更早的会在上传成功后自动删除"
	}
	s.flashTo(w, "ok", msg)
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// handleBackupTest 用表单里的配置测一次连接（不保存）。
func (s *Server) handleBackupTest(w http.ResponseWriter, r *http.Request) {
	if s.bk == nil {
		s.backupDisabled(w, r)
		return
	}
	cfg, err := s.backupConfigFromForm(r)
	if err != nil {
		s.flashTo(w, "err", "测试失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.bk.TestConnection(ctx, cfg); err != nil {
		s.flashTo(w, "err", "连接失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	s.flashTo(w, "ok", "连接成功，配置可用")
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// handleBackupRun 立即执行一轮归档 + 上传 + 清理。
func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	if s.bk == nil {
		s.backupDisabled(w, r)
		return
	}
	// 用独立 context：一轮归档上传可能要几十秒，不能跟着浏览器断开就取消，
	// 否则会留下"上传了一半"的中间状态。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.bk.RunOnce(ctx); err != nil {
		s.flashTo(w, "err", "执行失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	st, err := s.bk.Status(r.Context())
	if err != nil {
		s.flashTo(w, "ok", "执行完成")
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	if st.LastResult == "ok" {
		s.flashTo(w, "ok", "执行完成："+st.LastMessage)
	} else {
		s.flashTo(w, "err", "执行完成但有错误："+st.LastMessage)
	}
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// handleBackupUploadDay 手动补传某一天。
func (s *Server) handleBackupUploadDay(w http.ResponseWriter, r *http.Request) {
	if s.bk == nil {
		s.backupDisabled(w, r)
		return
	}
	day := r.PathValue("day")
	if !validDay(day) {
		s.flashTo(w, "err", "日期格式错误")
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.bk.UploadDay(ctx, day); err != nil {
		s.flashTo(w, "err", day+" 上传失败："+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	s.flashTo(w, "ok", day+" 已上传")
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// validDay 校验 YYYY-MM-DD。
func validDay(v string) bool {
	if len(v) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", v)
	return err == nil
}

// backupDisabled 统一处理"该实例关掉了备份"。
func (s *Server) backupDisabled(w http.ResponseWriter, r *http.Request) {
	s.flashTo(w, "err", "该实例已关闭在线备份（BRIDGE_BACKUP_DISABLED=1）")
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}
