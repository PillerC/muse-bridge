package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// v2.1 及以前的库没有 allowed_user_ids，只有 allowed_chat_ids。
// Open 必须能直接把老库升上来，否则升级二进制就等于服务起不来。
func TestMigrateAddsAllowedUserIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 造一个只含旧列的老库。
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE bridges (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  slug TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  bot_token_enc TEXT NOT NULL,
  bot_id INTEGER,
  bot_username TEXT,
  webhook_secret TEXT NOT NULL,
  allowed_chat_ids TEXT,
  mode TEXT NOT NULL DEFAULT 'poll',
  last_update_id INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO bridges (slug,name,bot_token_enc,webhook_secret,allowed_chat_ids,mode,status,created_at,updated_at)
VALUES ('solo','单人桥','x','s','12345','poll','active',1,1),
       ('group','群桥','x','s','-1001234567890','poll','active',1,1),
       ('empty','空桥','x','s','','poll','active',1,1);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级老库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	cols, err := st.tableColumns(ctx, "bridges")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["allowed_user_ids"] {
		t.Fatal("迁移后应有 allowed_user_ids 列")
	}

	// 私聊的会话 ID 就是对方用户 ID，正数可以安全搬过来。
	b, err := st.GetBridgeBySlug(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedUserIDs != "12345" {
		t.Fatalf("正数旧值应迁移为用户 ID，得到 %q", b.AllowedUserIDs)
	}

	// 群 ID 是负数，说明当初按会话配的，无法等价成"人"，留空等人自己填。
	b, err = st.GetBridgeBySlug(ctx, "group")
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedUserIDs != "" {
		t.Fatalf("负数旧值不该迁移，得到 %q", b.AllowedUserIDs)
	}

	// 再启动一次必须幂等，不能因为列已存在就报错。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重复启动应幂等: %v", err)
	}
	st2.Close()
}

func TestOpenNewDatabaseHasUserIDs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cols, err := st.tableColumns(context.Background(), "bridges")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["allowed_user_ids"] {
		t.Fatal("新库应直接带 allowed_user_ids 列")
	}
}

// 首个发送者绑定必须是原子的：两个人同时发消息，只能有一个成功。
func TestClaimFirstSender(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	id, err := st.CreateBridge(ctx, &Bridge{
		Slug: "b", Name: "b", BotTokenEnc: "x", WebhookSecret: "s",
		Mode: "poll", Status: "active", CreatedAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if ok, err := st.ClaimFirstSender(ctx, id, 111, 2); err != nil || !ok {
		t.Fatalf("第一次绑定应成功: %v %v", ok, err)
	}
	if ok, err := st.ClaimFirstSender(ctx, id, 222, 3); err != nil || ok {
		t.Fatalf("已绑定后不该再改: %v %v", ok, err)
	}
	b, err := st.GetBridgeByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.AllowedUserIDs != "111" {
		t.Fatalf("应保持首个发送者 111，实际 %q", b.AllowedUserIDs)
	}
}
