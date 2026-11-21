package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// migrate 给历史数据库补上后来新增的列。
//
// schema 用的是 CREATE TABLE IF NOT EXISTS，表已经存在时定义变了也不会多出列，
// 所以每次启动都读一次 PRAGMA 比对。纯读，代价可以忽略。
func (s *Store) migrate(ctx context.Context) error {
	cols, err := s.tableColumns(ctx, "bridges")
	if err != nil {
		return err
	}
	if !cols["allowed_user_ids"] {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE bridges ADD COLUMN allowed_user_ids TEXT`); err != nil {
			return fmt.Errorf("添加 allowed_user_ids 列失败: %w", err)
		}
	}
	// v2.2 之前这列叫 allowed_chat_ids，存的是会话 ID，含义是"整个会话能不能触发"。
	// 私聊的会话 ID 恰好等于对方用户 ID，所以旧值全是正数时可以直接搬过来；
	// 一旦出现负数（群/频道 ID），说明当初确实是按会话配的，无法等价成"人"，
	// 宁可不迁——后台会显示成未配置，由人自己决定填谁。
	if cols["allowed_chat_ids"] {
		if err := s.migrateChatIDsToUserIDs(ctx); err != nil {
			return err
		}
	}

	// 令牌早期只存摘要，明文看一眼就没了。摘要不可逆，老令牌的明文补不回来，
	// 只能加一列给新令牌存密文；老令牌在后台标成"无法显示"，由人重签。
	tokenCols, err := s.tableColumns(ctx, "bridge_tokens")
	if err != nil {
		return err
	}
	if !tokenCols["token_enc"] {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE bridge_tokens ADD COLUMN token_enc TEXT`); err != nil {
			return fmt.Errorf("添加 token_enc 列失败: %w", err)
		}
	}
	return nil
}

func (s *Store) migrateChatIDsToUserIDs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(allowed_user_ids,''), COALESCE(allowed_chat_ids,'') FROM bridges`)
	if err != nil {
		return err
	}
	type rec struct {
		id  int64
		cur string
		old string
	}
	var list []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.cur, &r.old); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range list {
		if strings.TrimSpace(r.cur) != "" {
			continue
		}
		ids, ok := positiveIDs(r.old)
		if !ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE bridges SET allowed_user_ids=? WHERE id=?`, ids, r.id); err != nil {
			return err
		}
	}
	return nil
}

// positiveIDs 把逗号分隔的数字规整成规范形式；只要有一个不是正整数就放弃。
func positiveIDs(csv string) (string, bool) {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil || v <= 0 {
			return "", false
		}
		out = append(out, strconv.FormatInt(v, 10))
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, ","), true
}

func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
