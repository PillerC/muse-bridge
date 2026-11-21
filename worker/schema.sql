-- Telegram -> Muse 消息队列 D1 schema
-- 部署: wrangler d1 execute telegram-queue --file=./schema.sql

CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  update_id     INTEGER NOT NULL UNIQUE,          -- Telegram update_id, 去重键
  chat_id       INTEGER NOT NULL,
  chat_type     TEXT NOT NULL,                    -- private | group | supergroup | channel
  chat_title    TEXT,
  message_id    INTEGER NOT NULL,
  from_id       INTEGER,
  from_username TEXT,
  from_name     TEXT,
  text          TEXT NOT NULL DEFAULT '',
  reply_to_message_id INTEGER,
  reply_to_from_id    INTEGER,
  reply_to_username   TEXT,
  is_mention    INTEGER NOT NULL DEFAULT 0,       -- 是否 @ 本 Bot
  is_reply_to_bot INTEGER NOT NULL DEFAULT 0,     -- 是否引用本 Bot 的消息
  should_reply  INTEGER NOT NULL DEFAULT 0,       -- 是否需要 Muse 回复
  status        TEXT NOT NULL DEFAULT 'pending',  -- pending | processing | done | dead | skipped
  attempts      INTEGER NOT NULL DEFAULT 0,
  lease_id      TEXT,                             -- 本次认领的租约 ID, 防误删 ack
  claimed_at    INTEGER,                          -- 认领时间戳 (秒)
  lease_seconds INTEGER NOT NULL DEFAULT 120,
  next_retry_at INTEGER NOT NULL DEFAULT 0,       -- 下次可拉取时间 (秒), 用于退避
  last_error    TEXT,
  raw_json      TEXT NOT NULL,                    -- 完整 Telegram update, 供排障/重放
  created_at    INTEGER NOT NULL,                 -- 入队时间戳 (秒)
  updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_poll
  ON messages(status, next_retry_at, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_chat
  ON messages(chat_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_status
  ON messages(status, updated_at);

-- 可选: 简单的统计 / 排障视图
-- SELECT status, COUNT(*) FROM messages GROUP BY status;
