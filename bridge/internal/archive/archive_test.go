package archive

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newTestDB 造一个只含归档查询所需列的最小库。
//
// 不调 store.Open：archive 包不该反向依赖 store，
// 而这里要测的是"按时间范围查消息"这段 SQL，独立造表更清楚。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
CREATE TABLE bridges (
  id INTEGER PRIMARY KEY, slug TEXT, name TEXT
);
CREATE TABLE conversations (
  id INTEGER PRIMARY KEY, bridge_id INTEGER, chat_id INTEGER,
  chat_type TEXT, title TEXT
);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY, bridge_id INTEGER, conversation_id INTEGER,
  direction TEXT, tg_message_id INTEGER, sender_name TEXT,
  sender_username TEXT, text TEXT, content_kind TEXT, created_at INTEGER
);
INSERT INTO bridges (id, slug, name) VALUES (1, 'demo', '演示桥');
INSERT INTO conversations (id, bridge_id, chat_id, chat_type, title)
  VALUES (1, 1, 999, 'private', '家');
`); err != nil {
		t.Fatal(err)
	}
	return db
}

func addMessage(t *testing.T, db *sql.DB, id int64, at time.Time, dir, text string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO messages (id, bridge_id, conversation_id, direction, tg_message_id,
		                       sender_name, sender_username, text, content_kind, created_at)
		 VALUES (?,1,1,?,?,?,?,?, 'text', ?)`,
		id, dir, id, "家", "jia", text, at.Unix()); err != nil {
		t.Fatal(err)
	}
}

// 按天归档的核心：一天一个文件，只包含那一天的消息。
func TestExportDaySplitsByDay(t *testing.T) {
	db := newTestDB(t)
	loc := time.FixedZone("CST", 8*3600)
	d1 := time.Date(2026, 11, 20, 10, 0, 0, 0, loc)
	d2 := time.Date(2026, 11, 21, 10, 0, 0, 0, loc)
	addMessage(t, db, 1, d1, "in", "第一天")
	addMessage(t, db, 2, d1.Add(time.Hour), "out", "第一天回复")
	addMessage(t, db, 3, d2, "in", "第二天")

	dir := t.TempDir()
	f1, err := ExportDay(context.Background(), db, dir, "2026-11-20", loc)
	if err != nil {
		t.Fatal(err)
	}
	if f1.Records != 2 {
		t.Fatalf("11-20 应有 2 条，得到 %d", f1.Records)
	}
	f2, err := ExportDay(context.Background(), db, dir, "2026-11-21", loc)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Records != 1 {
		t.Fatalf("11-21 应有 1 条，得到 %d", f2.Records)
	}
	if f1.Bytes == 0 || f2.Bytes == 0 {
		t.Fatal("归档文件不应为空")
	}

	// 文件名必须是 YYYY-MM-DD.jsonl.gz，升级/上传都靠它对齐。
	if filepath.Base(f1.Path) != "2026-11-20.jsonl.gz" {
		t.Fatalf("文件名不对: %s", filepath.Base(f1.Path))
	}
	if _, err := os.Stat(f1.Path); err != nil {
		t.Fatalf("归档文件应存在: %v", err)
	}
}

// 边界：本地时区的零点归属。UTC 下的 23:30 在东八区是次日 07:30，
// 必须算到次日，否则用户会看到"昨天的消息跑到今天"。
func TestExportDayUsesLocalTimezone(t *testing.T) {
	db := newTestDB(t)
	loc := time.FixedZone("CST", 8*3600)
	// UTC 2026-11-20 23:30 = 东八区 2026-11-21 07:30
	at := time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC)
	addMessage(t, db, 1, at, "in", "跨零点")

	dir := t.TempDir()
	f20, err := ExportDay(context.Background(), db, dir, "2026-11-20", loc)
	if err != nil {
		t.Fatal(err)
	}
	if f20.Records != 0 {
		t.Fatalf("东八区 11-20 不应包含这条，得到 %d", f20.Records)
	}
	f21, err := ExportDay(context.Background(), db, dir, "2026-11-21", loc)
	if err != nil {
		t.Fatal(err)
	}
	if f21.Records != 1 {
		t.Fatalf("东八区 11-21 应包含这条，得到 %d", f21.Records)
	}
}

func TestExportDayEmpty(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	f, err := ExportDay(context.Background(), db, dir, "2026-11-22", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if f.Records != 0 {
		t.Fatalf("空的一天应为 0 条，得到 %d", f.Records)
	}
}

// 归档文件必须是合法 gzip + JSON Lines，别人拿 gzip -dc 就能读。
func TestArchiveIsValidGzipJSONL(t *testing.T) {
	db := newTestDB(t)
	loc := time.UTC
	addMessage(t, db, 1, time.Date(2026, 11, 20, 10, 0, 0, 0, loc), "in", "hello")
	dir := t.TempDir()
	f, err := ExportDay(context.Background(), db, dir, "2026-11-20", loc)
	if err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("不是合法 gzip: %v", err)
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	lines := 0
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("不是合法 JSONL: %v (%s)", err, sc.Text())
		}
		if rec.Text != "hello" {
			t.Fatalf("内容不对: %q", rec.Text)
		}
		if rec.Bridge != "演示桥" {
			t.Fatalf("桥名不对: %q", rec.Bridge)
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != 1 {
		t.Fatalf("应有 1 行，得到 %d", lines)
	}
}

func TestListFindsArchiveFiles(t *testing.T) {
	dir := t.TempDir()
	// 造两个文件（内容无所谓）。
	for _, day := range []string{"2026-11-20", "2026-11-21"} {
		w, err := NewWriter(dir, day)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add(&Record{Text: "x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Close(dir, day); err != nil {
			t.Fatal(err)
		}
	}
	// 混入一个无关文件，必须被忽略。
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("应找到 2 个归档文件，得到 %d", len(files))
	}
	// 升序。
	if files[0].Day != "2026-11-20" || files[1].Day != "2026-11-21" {
		t.Fatalf("排序不对: %s, %s", files[0].Day, files[1].Day)
	}
}

func TestListMissingDir(t *testing.T) {
	files, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("目录不存在不应报错: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("应返回空，得到 %d", len(files))
	}
}

func TestDayRange(t *testing.T) {
	from := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	days := DayRange(from, to)
	want := []string{"2026-11-20", "2026-11-21", "2026-11-22"}
	if len(days) != len(want) {
		t.Fatalf("应得 %d 天，得到 %d: %v", len(want), len(days), days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Fatalf("第 %d 天应为 %s，得到 %s", i, want[i], days[i])
		}
	}
}

func TestCutoffDay(t *testing.T) {
	now := time.Date(2026, 11, 22, 15, 0, 0, 0, time.Local)
	cases := []struct {
		keep int
		want string
	}{
		{0, ""},           // 不清理
		{1, "2026-11-22"}, // 只保留今天
		{7, "2026-11-16"}, // 保留 7 天（含今天）
		{30, "2026-10-24"},
	}
	for _, c := range cases {
		if got := CutoffDay(now, c.keep); got != c.want {
			t.Fatalf("CutoffDay(keep=%d)=%q，期望 %q", c.keep, got, c.want)
		}
	}
}

func TestDaysWithMessages(t *testing.T) {
	db := newTestDB(t)
	loc := time.UTC
	addMessage(t, db, 1, time.Date(2026, 11, 20, 10, 0, 0, 0, loc), "in", "a")
	addMessage(t, db, 2, time.Date(2026, 11, 22, 10, 0, 0, 0, loc), "in", "b")

	days, err := DaysWithMessages(context.Background(), db, loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-20", "2026-11-22"}
	if len(days) != len(want) {
		t.Fatalf("应得 %v，得到 %v", want, days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Fatalf("应得 %v，得到 %v", want, days)
		}
	}
}

// 归档中途失败不能留下一个"看起来完整"的半截文件。
func TestWriterAbortCleansTemp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(&Record{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	w.abort()

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("abort 后不应留下任何文件，发现 %s", e.Name())
	}
}
