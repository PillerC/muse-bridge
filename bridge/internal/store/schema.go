package store

// schema 是 v2 的初始表结构。
//
// 与 v1 的关键差别：
//   - messages 是双向聊天流水（in / out），不再承担队列职责；
//   - jobs 只引用 messages，独立管理认领与重试状态；
//   - 不回复的消息也写 messages（needs_reply=0），只是不建 job，
//     这样"消息到了但没触发"可以直接查表，不需要额外的 skipped 状态；
//   - 桥令牌独立成表，一座桥可签发多枚，便于换机和轮换。
const schema = `
CREATE TABLE IF NOT EXISTS bridges (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  slug            TEXT    NOT NULL UNIQUE,
  name            TEXT    NOT NULL,
  bot_token_enc   TEXT    NOT NULL,
  bot_id          INTEGER,
  bot_username    TEXT,
  webhook_secret  TEXT    NOT NULL,
  allowed_chat_ids TEXT,
  mode            TEXT    NOT NULL DEFAULT 'poll',
  last_update_id  INTEGER NOT NULL DEFAULT 0,
  status          TEXT    NOT NULL DEFAULT 'active',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS bridge_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id    INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  label        TEXT,
  token_hash   TEXT    NOT NULL UNIQUE,
  token_prefix TEXT    NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_bridge ON bridge_tokens(bridge_id);

CREATE TABLE IF NOT EXISTS conversations (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id       INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  chat_id         INTEGER NOT NULL,
  chat_type       TEXT    NOT NULL,
  title           TEXT,
  last_message_at INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  UNIQUE (bridge_id, chat_id)
);
CREATE INDEX IF NOT EXISTS idx_conv_bridge ON conversations(bridge_id, last_message_at DESC);

CREATE TABLE IF NOT EXISTS messages (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  bridge_id       INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  direction       TEXT    NOT NULL,
  tg_message_id   INTEGER,
  tg_parts        TEXT,
  reply_to_tg_id  INTEGER,
  sender_id       INTEGER,
  sender_name     TEXT,
  sender_username TEXT,
  text            TEXT    NOT NULL DEFAULT '',
  content_kind    TEXT    NOT NULL DEFAULT 'text',
  content_hint    TEXT,
  media_json      TEXT,
  update_id       INTEGER,
  is_mention      INTEGER NOT NULL DEFAULT 0,
  is_reply_to_bot INTEGER NOT NULL DEFAULT 0,
  needs_reply     INTEGER NOT NULL DEFAULT 0,
  reply_reason    TEXT,
  idempotency_key TEXT,
  raw_json        TEXT,
  created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_msg_conv   ON messages(conversation_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_msg_bridge ON messages(bridge_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_msg_idem ON messages(bridge_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
-- Telegram 重投同一个 update 时靠它挡住，避免重复入队；比 v1 的 INSERT OR IGNORE 更精确，
-- 不会把 NOT NULL / CHECK 失败也误当成"已去重"。
CREATE UNIQUE INDEX IF NOT EXISTS idx_msg_update ON messages(bridge_id, update_id)
  WHERE update_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS jobs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id    INTEGER NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
  bridge_id     INTEGER NOT NULL REFERENCES bridges(id) ON DELETE CASCADE,
  status        TEXT    NOT NULL DEFAULT 'pending',
  attempts      INTEGER NOT NULL DEFAULT 0,
  lease_id      TEXT,
  claimed_at    INTEGER,
  lease_seconds INTEGER NOT NULL DEFAULT 180,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  -- draft 阶段创建的占位消息在 Telegram 里的 id，final 复用它做最后一次编辑。
  reply_tg_id   INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_poll  ON jobs(status, next_retry_at, id);
CREATE INDEX IF NOT EXISTS idx_jobs_lease ON jobs(lease_id);
CREATE INDEX IF NOT EXISTS idx_jobs_bridge ON jobs(bridge_id, status);

CREATE TABLE IF NOT EXISTS admins (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  token_hash TEXT    NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_exp ON sessions(expires_at);
`
