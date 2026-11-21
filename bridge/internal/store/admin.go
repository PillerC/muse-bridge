package store

import (
	"context"
	"database/sql"
	"errors"
)

// Admin 是后台账号。第一版只有单个管理员，表结构已为多人预留。
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
}

// UpsertAdmin 让环境变量成为密码的唯一来源：
// 账号不存在就创建，已存在就同步密码摘要（改密码只要改环境变量再重启）。
func (s *Store) UpsertAdmin(ctx context.Context, username, passwordHash string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admins (username, password_hash, created_at) VALUES (?,?,?)
		 ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash`,
		username, passwordHash, now)
	return err
}

// GetAdmin 按用户名取账号。
func (s *Store) GetAdmin(ctx context.Context, username string) (*Admin, error) {
	var a Admin
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM admins WHERE username=?`, username).Scan(
		&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateSession 存会话令牌的摘要。
func (s *Store) CreateSession(ctx context.Context, tokenHash string, expiresAt, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, expires_at, created_at) VALUES (?,?,?)`,
		tokenHash, expiresAt, now)
	return err
}

// SessionValid 判断会话是否仍然有效，顺手清理过期行。
func (s *Store) SessionValid(ctx context.Context, tokenHash string, now int64) bool {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE token_hash=? AND expires_at>?`,
		tokenHash, now).Scan(&n); err != nil {
		return false
	}
	if n == 0 {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, now)
	return true
}

// DeleteSession 退出登录。
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
}
