package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"muse-bridge/internal/archive"
)

// archive_state 记录每一天的归档情况。
//
// 为什么不靠"文件在不在"来判断：文件可能被用户手动删掉、可能上传失败，
// 而数据库里的状态是唯一可信的"这一天到底处理到哪一步了"。
// 有了它才能回答三个问题：哪天还没归档、哪天还没上传、哪天可以删本地数据。

// ArchiveState 是一天的归档状态。
//
// 直接别名到 archive.DayState：store 要读写它、backup 要判断它，
// 类型定义放公共的 archive 包里，两边都不用互相依赖。
type ArchiveState = archive.DayState

// EnsureArchiveState 保证某天有状态行。
func (s *Store) EnsureArchiveState(ctx context.Context, day string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO archive_state (day, records, bytes, archived_at, uploaded_at, upload_error, purged_at)
		 VALUES (?,0,0,0,0,'',0) ON CONFLICT(day) DO NOTHING`, day)
	return err
}

// MarkArchived 记录某天归档完成。
func (s *Store) MarkArchived(ctx context.Context, day string, records int, bytes int64, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO archive_state (day, records, bytes, archived_at, uploaded_at, upload_error, purged_at)
		 VALUES (?,?,?,?,0,'',0)
		 ON CONFLICT(day) DO UPDATE SET
		   records=excluded.records, bytes=excluded.bytes,
		   archived_at=excluded.archived_at, upload_error=''`,
		day, records, bytes, now)
	return err
}

// MarkUploaded 记录某天上传成功。
func (s *Store) MarkUploaded(ctx context.Context, day string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE archive_state SET uploaded_at=?, upload_error='' WHERE day=?`, now, day)
	return err
}

// MarkUploadFailed 记录上传失败原因。
func (s *Store) MarkUploadFailed(ctx context.Context, day, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE archive_state SET upload_error=? WHERE day=?`, reason, day)
	return err
}

// MarkPurged 记录某天本地消息已删除。
func (s *Store) MarkPurged(ctx context.Context, day string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE archive_state SET purged_at=? WHERE day=?`, now, day)
	return err
}

// GetArchiveState 读某天的状态，不存在返回 nil。
func (s *Store) GetArchiveState(ctx context.Context, day string) (*ArchiveState, error) {
	var a ArchiveState
	err := s.db.QueryRowContext(ctx,
		`SELECT day, records, bytes, archived_at, uploaded_at, COALESCE(upload_error,''), purged_at
		   FROM archive_state WHERE day=?`, day).Scan(
		&a.Day, &a.Records, &a.Bytes, &a.ArchivedAt, &a.UploadedAt, &a.UploadError, &a.PurgedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListArchiveStates 按日期倒序返回状态。
func (s *Store) ListArchiveStates(ctx context.Context, limit int) ([]*ArchiveState, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, records, bytes, archived_at, uploaded_at, COALESCE(upload_error,''), purged_at
		   FROM archive_state ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ArchiveState
	for rows.Next() {
		var a ArchiveState
		if err := rows.Scan(&a.Day, &a.Records, &a.Bytes, &a.ArchivedAt,
			&a.UploadedAt, &a.UploadError, &a.PurgedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// PendingUploads 返回已归档但还没成功上传的日期（升序）。
//
// 只要求 archived_at>0：还没归档的日子谈不上上传。
// 有 upload_error 的也包含在内——下次重试。
func (s *Store) PendingUploads(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT day FROM archive_state
		  WHERE archived_at>0 AND uploaded_at=0
		  ORDER BY day LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DaysNeedingArchive 返回还没归档、但库里确实有消息的日期（升序）。
//
// 判据是"该天有消息 且 archive_state 里没有归档记录"。用 min/max 先框范围，
// 再按天分组，避免对空库做无意义的全表扫描。
func (s *Store) DaysNeedingArchive(ctx context.Context, loc *time.Location, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	mod := offsetModifier(loc)
	rows, err := s.db.QueryContext(ctx,
		`SELECT day FROM (
		   SELECT strftime('%Y-%m-%d', created_at, 'unixepoch', ?) AS day,
		          COUNT(*) AS n
		     FROM messages
		    GROUP BY day
		 ) WHERE n>0
		   AND day NOT IN (SELECT day FROM archive_state WHERE archived_at>0)
		 ORDER BY day LIMIT ?`, mod, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PurgeDay 删除某一天的本地消息与对应任务，返回删除的消息数。
//
// 只删消息和任务，不删 conversations：会话本身是轻量元信息，
// 删了会让后台会话列表整段消失，用户看到的是"聊天记录凭空没了"，
// 而实际上他只是设置了保留 30 天。
func (s *Store) PurgeDay(ctx context.Context, day string, loc *time.Location) (int, error) {
	start, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return 0, fmt.Errorf("日期格式错误: %w", err)
	}
	end := start.AddDate(0, 0, 1)
	var n int
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		// 先删 jobs（外键指向 messages），再删 messages。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM jobs WHERE message_id IN (
			   SELECT id FROM messages WHERE created_at>=? AND created_at<?)`,
			start.Unix(), end.Unix()); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE created_at>=? AND created_at<?`,
			start.Unix(), end.Unix())
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
		return nil
	})
	return n, err
}

// ArchiveStats 是给后台看的汇总。
type ArchiveStats struct {
	// Days 是已归档天数。
	Days int
	// Bytes 是归档文件总大小。
	Bytes int64
	// PendingUpload 是待上传天数。
	PendingUpload int
	// PendingArchive 是待归档天数。
	PendingArchive int
	// EarliestDay / LatestDay 是库里现存消息的日期范围。
	EarliestDay string
	LatestDay   string
}

// GetArchiveStats 汇总归档情况。
func (s *Store) GetArchiveStats(ctx context.Context, loc *time.Location) (*ArchiveStats, error) {
	out := &ArchiveStats{}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(bytes),0),
		        COALESCE(SUM(CASE WHEN archived_at>0 AND uploaded_at=0 THEN 1 ELSE 0 END),0)
		   FROM archive_state`).Scan(&out.Days, &out.Bytes, &out.PendingUpload); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MIN(day),''), COALESCE(MAX(day),'') FROM (
		   SELECT strftime('%Y-%m-%d', created_at, 'unixepoch', ?) AS day FROM messages
		 )`, offsetModifier(loc)).Scan(&out.EarliestDay, &out.LatestDay); err != nil {
		return nil, err
	}
	if n, err := s.DaysNeedingArchive(ctx, loc, 1000); err == nil {
		out.PendingArchive = len(n)
	}
	return out, nil
}

// offsetModifier 把时区换算成 SQLite 的修饰符，例如 "+08:00"。
//
// SQLite 的 unixepoch 默认按 UTC 切日期，直接用会得到 UTC 的"天"，
// 而用户说的"今天"是他本地时区的今天。用固定偏移而不是命名时区：
// SQLite 不认识 IANA 时区名。
func offsetModifier(loc *time.Location) string {
	_, off := time.Now().In(loc).Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
}
