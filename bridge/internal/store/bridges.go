package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotFound 表示按条件没查到记录。
var ErrNotFound = errors.New("record not found")

// Bridge 是一座桥：一个 Telegram 机器人 + 一个 Muse 接入点。
// BotToken 只在内存中出现，落库的是密文。
type Bridge struct {
	ID             int64
	Slug           string
	Name           string
	BotTokenEnc    string
	BotID          int64
	BotUsername    string
	WebhookSecret  string
	AllowedUserIDs string // 逗号分隔的 Telegram 用户 ID；空表示不接收任何消息
	Mode           string // poll | webhook
	LastUpdateID   int64
	Status         string // active | paused
	CreatedAt      int64
	UpdatedAt      int64
}

// Token 是桥令牌的元数据，不含明文。
type Token struct {
	ID          int64
	BridgeID    int64
	Label       string
	TokenHash   string
	TokenPrefix string
	LastUsedAt  int64
	RevokedAt   int64
	CreatedAt   int64
}

// CreateBridge 插入一座桥。
func (s *Store) CreateBridge(ctx context.Context, b *Bridge) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO bridges (slug, name, bot_token_enc, bot_id, bot_username,
		                      webhook_secret, allowed_user_ids, mode, status, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		b.Slug, b.Name, b.BotTokenEnc, b.BotID, b.BotUsername,
		b.WebhookSecret, b.AllowedUserIDs, b.Mode, b.Status, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("slug %q 已被占用", b.Slug)
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetBridgeByID 按主键取桥。
func (s *Store) GetBridgeByID(ctx context.Context, id int64) (*Bridge, error) {
	return s.scanBridge(s.db.QueryRowContext(ctx, bridgeColumns+` WHERE id=?`, id))
}

// GetBridgeBySlug 按 slug 取桥，webhook 路由用。
func (s *Store) GetBridgeBySlug(ctx context.Context, slug string) (*Bridge, error) {
	return s.scanBridge(s.db.QueryRowContext(ctx, bridgeColumns+` WHERE slug=?`, slug))
}

// ListBridges 返回全部桥，新建的排前面。
func (s *Store) ListBridges(ctx context.Context) ([]*Bridge, error) {
	rows, err := s.db.QueryContext(ctx, bridgeColumns+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Bridge
	for rows.Next() {
		b, err := s.scanBridge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const bridgeColumns = `SELECT id, slug, name, bot_token_enc, COALESCE(bot_id,0), COALESCE(bot_username,''),
       webhook_secret, COALESCE(allowed_user_ids,''), mode, last_update_id, status, created_at, updated_at
  FROM bridges`

func (s *Store) scanBridge(row interface{ Scan(...any) error }) (*Bridge, error) {
	var b Bridge
	err := row.Scan(&b.ID, &b.Slug, &b.Name, &b.BotTokenEnc, &b.BotID, &b.BotUsername,
		&b.WebhookSecret, &b.AllowedUserIDs, &b.Mode, &b.LastUpdateID, &b.Status,
		&b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpdateBridgeBot 更新机器人身份与加密后的 token（换 token 时用）。
func (s *Store) UpdateBridgeBot(ctx context.Context, id int64, encToken string, botID int64, username string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE bridges SET bot_token_enc=?, bot_id=?, bot_username=?, updated_at=? WHERE id=?`,
		encToken, botID, username, now, id)
	return err
}

// ClaimFirstSender 在允许列表为空时把 firstUserID 绑定成唯一允许的人。
//
// 用一条带条件的 UPDATE 完成，靠数据库的串行写保证只有一个人能抢到：
// 两个陌生人同时发消息时，后来的那条会得到 false，然后按"已有名单"处理。
// 返回 true 表示本次调用完成了绑定。
func (s *Store) ClaimFirstSender(ctx context.Context, bridgeID, firstUserID, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE bridges SET allowed_user_ids=?, updated_at=?
		  WHERE id=? AND COALESCE(allowed_user_ids,'')=''`,
		strconv.FormatInt(firstUserID, 10), now, bridgeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateBridgeSettings 更新名字、允许的用户、模式。
func (s *Store) UpdateBridgeSettings(ctx context.Context, id int64, name, allowedUsers, mode string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE bridges SET name=?, allowed_user_ids=?, mode=?, updated_at=? WHERE id=?`,
		name, allowedUsers, mode, now, id)
	return err
}

// SetBridgeStatus 启用或暂停。
func (s *Store) SetBridgeStatus(ctx context.Context, id int64, status string, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE bridges SET status=?, updated_at=? WHERE id=?`, status, now, id)
	return err
}

// SetLastUpdateID 记录长轮询进度。只增不减，避免 Telegram 重投已处理的更新。
func (s *Store) SetLastUpdateID(ctx context.Context, id, updateID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE bridges SET last_update_id=? WHERE id=? AND last_update_id<?`, updateID, id, updateID)
	return err
}

// DeleteBridge 删除桥及其全部会话、消息、任务、令牌。
func (s *Store) DeleteBridge(ctx context.Context, id int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM jobs WHERE bridge_id=?`,
			`DELETE FROM messages WHERE bridge_id=?`,
			`DELETE FROM conversations WHERE bridge_id=?`,
			`DELETE FROM bridge_tokens WHERE bridge_id=?`,
			`DELETE FROM bridges WHERE id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// CreateToken 保存令牌摘要。
func (s *Store) CreateToken(ctx context.Context, bridgeID int64, label, hash, prefix string, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO bridge_tokens (bridge_id, label, token_hash, token_prefix, created_at)
		 VALUES (?,?,?,?,?)`, bridgeID, label, hash, prefix, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResolveToken 用令牌摘要反查桥。返回 ErrNotFound 表示令牌无效或已吊销。
func (s *Store) ResolveToken(ctx context.Context, hash string) (*Bridge, int64, error) {
	var bridgeID, tokenID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT bridge_id, id FROM bridge_tokens
		  WHERE token_hash=? AND revoked_at IS NULL`, hash).Scan(&bridgeID, &tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	b, err := s.GetBridgeByID(ctx, bridgeID)
	if err != nil {
		return nil, 0, err
	}
	return b, tokenID, nil
}

// TouchToken 记录令牌最近使用时间，便于后台辨认哪枚在用。
func (s *Store) TouchToken(ctx context.Context, tokenID, now int64) {
	// 失败无所谓，不阻塞请求。
	_, _ = s.db.ExecContext(ctx, `UPDATE bridge_tokens SET last_used_at=? WHERE id=?`, now, tokenID)
}

// ListTokens 返回某座桥的全部令牌（含已吊销）。
func (s *Store) ListTokens(ctx context.Context, bridgeID int64) ([]*Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, bridge_id, COALESCE(label,''), token_hash, token_prefix,
		        COALESCE(last_used_at,0), COALESCE(revoked_at,0), created_at
		   FROM bridge_tokens WHERE bridge_id=? ORDER BY id DESC`, bridgeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.BridgeID, &t.Label, &t.TokenHash, &t.TokenPrefix,
			&t.LastUsedAt, &t.RevokedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RevokeToken 吊销一枚令牌，立即失效。
func (s *Store) RevokeToken(ctx context.Context, bridgeID, tokenID, now int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE bridge_tokens SET revoked_at=? WHERE id=? AND bridge_id=? AND revoked_at IS NULL`,
		now, tokenID, bridgeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
