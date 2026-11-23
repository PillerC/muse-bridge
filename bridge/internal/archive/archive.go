// Package archive 把聊天记录按天切成独立文件。
//
// 为什么要按天切：SQLite 单文件会一直长，备份时整库上传既慢又浪费流量，
// 恢复时也没法只取某几天。按天切成 <日期>.jsonl.gz 之后：
//   - 每天一个文件，大小稳定，可以单独上传、单独保留、单独删；
//   - 格式是 JSON Lines（一行一条消息）再 gzip，流式读写，不用整月进内存；
//   - 人也能看懂：gzip -dc 2026-11-22.jsonl.gz | head 就能看。
//
// 归档不删除数据库里的数据——删数据是 retain 包的职责，两件事分开：
// 归档只负责"生成可备份的文件"，删不删本地记录由保留策略决定。
package archive

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DayLayout 是归档文件名的日期格式。
const DayLayout = "2006-01-02"

// Record 是归档文件里的一行。
//
// 字段名直接用 snake_case 的短名：归档文件是给人看和给脚本处理的，
// 太长反而不好读。跟 /export 的 JSON 输出保持一致的语义。
type Record struct {
	Time      string `json:"time"`
	Direction string `json:"direction"`
	Bridge    string `json:"bridge"`
	BridgeID  int64  `json:"bridge_id"`
	ChatID    int64  `json:"chat_id"`
	ChatType  string `json:"chat_type"`
	ChatTitle string `json:"chat_title"`
	Sender    string `json:"sender"`
	Username  string `json:"username"`
	Text      string `json:"text"`
	Kind      string `json:"kind"`
	TgMessage int64  `json:"tg_message_id,omitempty"`
}

// DayState 是一天的归档状态。
//
// 放在 archive 包里而不是 store 或 backup：store 要读写它、backup 要判断它，
// 两边都依赖 archive 才不会形成循环依赖。
type DayState struct {
	Day string
	// Records 是这一天归档了多少条消息。
	Records int
	// Bytes 是归档文件大小。
	Bytes int64
	// ArchivedAt 是生成归档文件的时间，0 表示还没归档。
	ArchivedAt int64
	// UploadedAt 是成功上传到远端的时间，0 表示还没上传。
	UploadedAt int64
	// UploadError 是最近一次上传失败的原因，成功后清空。
	UploadError string
	// PurgedAt 是本地消息被删除的时间，0 表示还在库里。
	PurgedAt int64
}

// DayFile 描述一个已生成的归档文件。
type DayFile struct {
	Day       string // 2006-01-02
	Path      string
	Bytes     int64
	Records   int
	CreatedAt int64
}

// FileName 返回某天的归档文件名。
func FileName(day string) string { return day + ".jsonl.gz" }

// DayRange 返回 [from, to) 之间所有日期字符串，升序。
func DayRange(from, to time.Time) []string {
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	to = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, to.Location())
	var out []string
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(DayLayout))
	}
	return out
}

// Writer 负责把某一天的消息写成 .jsonl.gz。
type Writer struct {
	f   *os.File
	gz  *gzip.Writer
	buf *bufio.Writer
	enc *json.Encoder
	n   int
}

// NewWriter 创建归档文件（会覆盖同名文件）。
//
// 先写 .tmp 再 rename：中途失败不会留下一个"看起来完整但其实是半截"的
// gzip 文件，否则上传到远端之后才发现截断，恢复时才知道数据没了。
func NewWriter(dir, day string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建归档目录失败: %w", err)
	}
	final := filepath.Join(dir, FileName(day))
	tmp, err := os.Create(final + ".tmp")
	if err != nil {
		return nil, fmt.Errorf("创建归档文件失败: %w", err)
	}
	gz := gzip.NewWriter(tmp)
	buf := bufio.NewWriterSize(gz, 32<<10)
	return &Writer{f: tmp, gz: gz, buf: buf, enc: json.NewEncoder(buf)}, nil
}

// Add 写一条记录。
func (w *Writer) Add(rec *Record) error {
	if err := w.enc.Encode(rec); err != nil {
		return err
	}
	w.n++
	return nil
}

// Close 落盘并改名，返回最终文件信息。
func (w *Writer) Close(dir, day string) (*DayFile, error) {
	if err := w.buf.Flush(); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.gz.Close(); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.f.Sync(); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.f.Close(); err != nil {
		w.abort()
		return nil, err
	}
	tmpName := w.f.Name()
	final := filepath.Join(dir, FileName(day))
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("归档文件改名失败: %w", err)
	}
	info, err := os.Stat(final)
	if err != nil {
		return nil, err
	}
	return &DayFile{Day: day, Path: final, Bytes: info.Size(), Records: w.n, CreatedAt: time.Now().Unix()}, nil
}

// abort 清掉半截文件。
func (w *Writer) abort() {
	name := w.f.Name()
	w.gz.Close()
	w.f.Close()
	os.Remove(name)
}

// ExportDay 把某一天的聊天记录导出成一个归档文件。
//
// 一天的时间范围按**本地时区**算：用户说的"今天"是他所在时区的今天，
// 用 UTC 切会让他看到昨天的消息落在今天里。库里 created_at 是 unix 秒，
// 所以这里先把本地零点换算成 unix 秒再查。
func ExportDay(ctx context.Context, db *sql.DB, dir, day string, loc *time.Location) (*DayFile, error) {
	start, err := time.ParseInLocation(DayLayout, day, loc)
	if err != nil {
		return nil, fmt.Errorf("日期格式错误: %w", err)
	}
	end := start.AddDate(0, 0, 1)

	w, err := NewWriter(dir, day)
	if err != nil {
		return nil, err
	}
	err = queryRange(ctx, db, start.Unix(), end.Unix(), loc, func(rec *Record) error {
		return w.Add(rec)
	})
	if err != nil {
		w.abort()
		return nil, err
	}
	return w.Close(dir, day)
}

// queryRange 把 [from, to) 内的消息按时间顺序喂给 fn。
//
// 用游标分批而不是一次 Query 全读：一天的群聊可能有几万条，
// 全读进内存没必要，而且 SQLite 单连接下长事务会挡住写入。
func queryRange(ctx context.Context, db *sql.DB, from, to int64, loc *time.Location, fn func(*Record) error) error {
	const batch = 2000
	lastID := int64(0)
	for {
		rows, err := db.QueryContext(ctx,
			`SELECT m.id, m.created_at, m.direction, b.name, m.bridge_id, c.chat_id, c.chat_type,
			        COALESCE(c.title,''), COALESCE(m.sender_name,''), COALESCE(m.sender_username,''),
			        m.text, m.content_kind, COALESCE(m.tg_message_id,0)
			   FROM messages m
			   JOIN conversations c ON c.id=m.conversation_id
			   JOIN bridges b ON b.id=m.bridge_id
			  WHERE m.created_at>=? AND m.created_at<?
			    AND m.id>?
			  ORDER BY m.id
			  LIMIT ?`, from, to, lastID, batch)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var (
				id, createdAt, bridgeID, chatID, tgMessageID int64
				direction, bridgeName, chatType, chatTitle   string
				sender, username, text, kind                 string
			)
			if err := rows.Scan(&id, &createdAt, &direction, &bridgeName, &bridgeID,
				&chatID, &chatType, &chatTitle, &sender, &username, &text, &kind, &tgMessageID); err != nil {
				rows.Close()
				return err
			}
			rec := &Record{
				Time:      time.Unix(createdAt, 0).In(loc).Format("2006-01-02 15:04:05"),
				Direction: direction, Bridge: bridgeName, BridgeID: bridgeID,
				ChatID: chatID, ChatType: chatType, ChatTitle: chatTitle,
				Sender: sender, Username: username, Text: text, Kind: kind,
				TgMessage: tgMessageID,
			}
			if err := fn(rec); err != nil {
				rows.Close()
				return err
			}
			lastID = id
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if n < batch {
			return nil
		}
	}
}

// DaysWithMessages 返回库里存在消息的日期（升序），供后台展示与补归档。
//
// 只扫 min/max 之间的日期，不逐日 COUNT：按月聚合一次就够，
// 具体某天有没有消息等真正归档时才知道（空的那天会生成空文件，直接跳过）。
func DaysWithMessages(ctx context.Context, db *sql.DB, loc *time.Location) ([]string, error) {
	var minT, maxT sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MIN(created_at), MAX(created_at) FROM messages`).Scan(&minT, &maxT); err != nil {
		return nil, err
	}
	if !minT.Valid || !maxT.Valid {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT strftime('%Y-%m-%d', created_at, 'unixepoch', ?) AS day
		   FROM messages ORDER BY day`, offsetModifier(loc))
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

// offsetModifier 把时区换算成 SQLite 的修饰符，例如 "+08:00"。
//
// SQLite 的 unixepoch 默认按 UTC 切日期，直接用会得到 UTC 的"天"。
// 用固定偏移而不是命名时区：SQLite 不认识 IANA 时区名，
// 而归档按天切这件事对夏令时那一天的偏差不敏感（最多差一小时）。
func offsetModifier(loc *time.Location) string {
	_, off := time.Now().In(loc).Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
}

// List 返回目录里已有的归档文件，按日期升序。
func List(dir string) ([]*DayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*DayFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".gz" {
			continue
		}
		day := name[:len(name)-len(".jsonl.gz")]
		if _, err := time.Parse(DayLayout, day); err != nil {
			continue // 不是我们生成的文件，忽略
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, &DayFile{Day: day, Path: filepath.Join(dir, name),
			Bytes: info.Size(), CreatedAt: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

// DayBefore 判断 day 是否早于 cutoff 那一天（不含）。
func DayBefore(day, cutoff string) bool { return day < cutoff }

// CutoffDay 返回"保留 n 天"对应的最早保留日期。
//
// n<=0 表示不清理（返回空串）。n=1 表示只保留今天。
func CutoffDay(now time.Time, keepDays int) string {
	if keepDays <= 0 {
		return ""
	}
	d := now.AddDate(0, 0, -(keepDays - 1))
	return d.Format(DayLayout)
}
