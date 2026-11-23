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
  -- 允许使用这座桥的 Telegram 用户 ID，逗号分隔。
  -- 存的是"发消息的人"（@userinfobot 给出的那个数字），不是会话 ID：
  -- 群里谁 @ 机器人都能触发太危险，空的含义也从"不限制"改成了"谁都不收"。
  allowed_user_ids TEXT,
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
  -- 查令牌用摘要，展示用密文。摘要不可逆，所以"以后还能复制令牌"
  -- 只能靠留一份 AES-GCM 密文（与机器人 token 同一把钥匙）。
  token_hash   TEXT    NOT NULL UNIQUE,
  token_prefix TEXT    NOT NULL,
  token_enc    TEXT,
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

-- 每日归档状态。一天一行，记录"归档了吗、上传了吗、本地删了吗"。
-- 文件系统里的文件不可信（可能被手动删、可能上传失败），
-- 所以状态以这张表为准，它决定了哪天可以安全删除本地数据。
CREATE TABLE IF NOT EXISTS archive_state (
  day          TEXT PRIMARY KEY,
  records      INTEGER NOT NULL DEFAULT 0,
  bytes        INTEGER NOT NULL DEFAULT 0,
  archived_at  INTEGER NOT NULL DEFAULT 0,
  uploaded_at  INTEGER NOT NULL DEFAULT 0,
  upload_error TEXT    NOT NULL DEFAULT '',
  purged_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_archive_upload ON archive_state(uploaded_at, day);

-- 键值配置表：升级设置这类"全实例只有一份"的状态放这里，
-- 不必为一个开关加一列、也不必动 bridges 这种核心表。
-- 值一律存字符串，由调用方自己解析，升级配置只是其中一类。
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
`
