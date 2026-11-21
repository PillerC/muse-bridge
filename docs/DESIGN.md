# telegram-muse-bridge 重构设计

> 状态：待评审
> 目标版本：v2.0
> 取代：Cloudflare Worker + D1 版本（v1）

## 1. 定位变化

v1 是**无状态转发器**：Telegram 消息进队列，Muse 取走，然后自己直连 Telegram 回复，
桥不参与回复路径，因此也留不下出站记录。

v2 是**有状态的通信中枢**：桥持有机器人凭证，Muse 只和桥对话。回复必须经过桥，
所以桥能记录完整往返。同时支持多座桥并行，每座桥是一个独立租户。

| 维度 | v1 | v2 |
|---|---|---|
| 运行环境 | Cloudflare Worker + D1 | 自有服务器，单进程 + SQLite |
| 租户 | 单 bot、单 Muse、共享令牌 | 多桥，每桥独立令牌，互相隔离 |
| 机器人凭证 | Muse 侧持有 | 只在桥内加密保存，Muse 不可见 |
| 回复路径 | Muse 直连 api.telegram.org | Muse → 桥 → Telegram |
| 消息留存 | 仅入站 | 入站 + 出站全量 |
| 运维入口 | curl + SQL | Web 管理后台 |

**不变的部分**（用户明确要求）：桥依旧是 Muse 与 Telegram 之间的消息桥梁；
群聊只回 @ 和引用；返回给 Muse 的取活方式仍是"轮询待办 + 认领租约"。

## 2. 技术选型

| 层 | 选择 | 理由 |
|---|---|---|
| 服务端 | Go 1.22+（标准库 net/http + modernc SQLite） | 目标机 435MB 内存 / 1GB 盘，Go 单二进制是唯一可行解；见下方「技术选型变更」 |
| 数据库 | SQLite（better-sqlite3，WAL） | 单机自托管零运维；仓储层隔离，后续可换 Postgres |
| 管理后台 | Go html/template + go:embed + 少量原生 JS | 服务器上没有构建管线，模板方案零构建、产物内嵌 |
| 部署 | 单二进制 + OpenRC + autocert | 进程自己管 TLS，省掉 Caddy（省 40MB 磁盘、20MB 内存） |
| 测试 | Vitest + 真实 SQLite 临时库 | 状态机、幂等、并发认领都要可复现 |

## 3. 进程内模块划分

```text
src/
  server.ts                  启动、装配、优雅关停
  config.ts                  环境变量解析与校验（启动即 fail-fast）
  db/
    index.ts                 连接、WAL、迁移执行
    migrations/001_init.sql
    repos/bridges.ts  conversations.ts  messages.ts  jobs.ts  tokens.ts
  domain/
    classify.ts              过滤规则（纯函数，无 IO）
    queue.ts                 认领 / 续租 / 回收 / 确认 / 失败 状态机
    sender.ts                出站：分段、占位、编辑、限速、429 退避
    telegram.ts              Bot API 客户端（超时、重试、错误分类）
    crypto.ts                AES-256-GCM 加解密、令牌哈希
  routes/
    webhook.ts               POST /tg/:slug
    muse/                    ping pending claim ack fail extend reply typing
    admin/                   auth bridges conversations messages export jobs
bridge/internal/server/templates/  服务端渲染的管理后台
```

约束：`domain/` 不感知 HTTP，`routes/` 不写 SQL，所有 SQL 只在 `db/repos/`。

## 4. 数据模型

```sql
CREATE TABLE bridges (
  id              INTEGER PRIMARY KEY,
  slug            TEXT NOT NULL UNIQUE,
  name            TEXT NOT NULL,
  bot_token_enc   TEXT NOT NULL,          -- AES-256-GCM，永不回显
  bot_id          INTEGER,
  bot_username    TEXT,
  webhook_secret  TEXT NOT NULL,          -- 随机 48 字符
  allowed_chat_ids TEXT,                  -- 逗号分隔，空表示不限制
  status          TEXT NOT NULL DEFAULT 'active',  -- active | paused
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE bridge_tokens (
  id           INTEGER PRIMARY KEY,
  bridge_id    INTEGER NOT NULL REFERENCES bridges(id),
  label        TEXT,                      -- 便于识别是哪台 Muse
  token_hash   TEXT NOT NULL UNIQUE,      -- sha256(明文)
  token_prefix TEXT NOT NULL,             -- 明文前 12 位，供界面辨认
  last_used_at INTEGER,
  revoked_at   INTEGER,
  created_at   INTEGER NOT NULL
);

CREATE TABLE conversations (
  id              INTEGER PRIMARY KEY,
  bridge_id       INTEGER NOT NULL REFERENCES bridges(id),
  chat_id         INTEGER NOT NULL,
  chat_type       TEXT NOT NULL,
  title           TEXT,
  last_message_at INTEGER NOT NULL,
  UNIQUE (bridge_id, chat_id)
);

CREATE TABLE messages (
  id              INTEGER PRIMARY KEY,
  bridge_id       INTEGER NOT NULL,
  conversation_id INTEGER NOT NULL REFERENCES conversations(id),
  direction       TEXT NOT NULL,          -- in | out
  tg_message_id   INTEGER,                -- 出站为第一条消息的 id
  tg_parts        TEXT,                   -- 出站分段 id 的 JSON 数组
  reply_to_tg_id  INTEGER,
  sender_id       INTEGER,
  sender_name     TEXT,
  text            TEXT NOT NULL DEFAULT '',
  content_kind    TEXT NOT NULL DEFAULT 'text',
  media_json      TEXT,                   -- 图片/语音等原始元信息
  needs_reply     INTEGER NOT NULL DEFAULT 0,
  idempotency_key TEXT,
  raw_json        TEXT,
  created_at      INTEGER NOT NULL
);

CREATE TABLE jobs (
  id            INTEGER PRIMARY KEY,
  message_id    INTEGER NOT NULL UNIQUE REFERENCES messages(id),
  status        TEXT NOT NULL DEFAULT 'pending',  -- pending|processing|done|dead
  attempts      INTEGER NOT NULL DEFAULT 0,
  lease_id      TEXT,
  claimed_at    INTEGER,
  lease_seconds INTEGER NOT NULL DEFAULT 180,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE admins (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL
);

CREATE TABLE admin_sessions (
  id         INTEGER PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
```

索引：

```sql
CREATE INDEX idx_jobs_poll   ON jobs(status, next_retry_at);
CREATE INDEX idx_jobs_lease  ON jobs(lease_id);
CREATE INDEX idx_conv_bridge ON conversations(bridge_id, last_message_at DESC);
CREATE INDEX idx_msg_conv    ON messages(conversation_id, id DESC);
CREATE INDEX idx_msg_bridge  ON messages(bridge_id, created_at DESC);
CREATE UNIQUE INDEX idx_msg_idem ON messages(bridge_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
```

设计要点：

- **记录与队列分离。** `messages` 是聊天流水，两个方向都在里面；`jobs` 只是引用它的待办。
  清理队列不会误删历史，这是 v2 最重要的结构性修正。
- **不再需要 `skipped` 状态。** 不回复的消息照常写入 `messages`（`needs_reply=0`），
  但不建 job。后台查"漏触发的 @ "直接查这张表。
- **令牌独立成表。** 一座桥可以签发多枚令牌（主用 / 备用 / 换机），各自可单独吊销。

## 5. Muse 侧 API

鉴权：`Authorization: Bearer <令牌>`。令牌决定桥，不接受任何 `bridge_id` 参数，
避免越权。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/v1/ping` | 校验令牌，返回桥名与机器人用户名 |
| GET | `/v1/pending-count` | 只返回待办数量，供 hook 判断是否唤醒 |
| POST | `/v1/claim` | 认领待办，返回消息体 + `lease_id` |
| POST | `/v1/jobs/:id/extend` | 续租 |
| POST | `/v1/jobs/:id/ack` | 完成 |
| POST | `/v1/jobs/:id/fail` | 失败（退避重试 / 进死信） |
| POST | `/v1/messages/:id/reply` | **出站唯一入口**，桥负责发往 Telegram |
| POST | `/v1/messages/:id/typing` | 转发 sendChatAction |

### reply 语义

```jsonc
POST /v1/messages/42/reply
Idempotency-Key: 42-a1b2c3          // 必填，重复提交返回上次结果
{ "text": "...", "mode": "draft" }   // draft | final
```

- `mode=draft`：第一次调用创建占位消息（默认引用原消息，内容 `💭 思考中…`），
  之后同一条消息的 draft 调用**编辑同一条** Telegram 消息。桥内部做 1.2 秒节流与
  429 `retry_after` 等待。
- `mode=final`：编辑定稿；超过 4096 字符自动按段落切分续发，返回所有分段 id。
- 每次调用都落库一条 outbound 记录（draft 只更新，final 落定稿）。
- 出站失败不阻塞入站：桥对外返回统一错误码，Muse 可安全重试。

这样 v1 的"typing + 占位 + 渐进编辑"体验完整保留，但可靠性从 prompt 挪进了代码。

## 6. 管理侧 API

鉴权：登录后签 httpOnly + SameSite=Lax 的会话 Cookie。

| 方法 | 路径 | 用途 |
|---|---|---|
| POST | `/api/auth/login` | 密码登录，失败计入限流 |
| GET | `/api/bridges` | 桥列表（含机器人用户名、待办数、今日消息数） |
| POST | `/api/bridges` | 建桥，入参 `{name, bot_token}` |
| PATCH | `/api/bridges/:id` | 改名、启停、白名单 |
| POST | `/api/bridges/:id/tokens` | 签发新令牌，明文仅本次返回 |
| DELETE | `/api/bridges/:id/tokens/:tid` | 吊销令牌 |
| GET | `/api/bridges/:id/conversations` | 会话列表 |
| GET | `/api/conversations/:id/messages` | 消息分页（游标 `before_id`） |
| GET | `/api/bridges/:id/messages` | 跨会话搜索（关键字 + 时间范围） |
| GET | `/api/bridges/:id/export` | 导出 CSV / JSON（流式） |
| GET | `/api/bridges/:id/jobs` | 待办与死信 |
| POST | `/api/jobs/:id/replay` | 死信重放 |

## 7. 建桥流程

1. 后台填：桥名 + 机器人 token。
2. 桥用该 token 调 `getMe`。失败即拒绝，不落库。
3. 生成 `slug`、`webhook_secret`；调 `setWebhook` 指向
   `https://<域名>/tg/<slug>`，带上 `secret_token` 与 `allowed_updates:["message"]`。
4. 生成 `api_token`，只存哈希，明文仅在响应里出现一次。
5. 页面显示「接入地址 + 令牌」，可复制、可再签一枚。
6. Muse 侧配置这两个值即完成接入，全程接触不到机器人 token。

失败可回滚：setWebhook 失败时不创建桥，或创建为 `paused` 并提示原因。

## 8. 消息流转

入站：

1. Telegram `POST /tg/:slug`，校验 `X-Telegram-Bot-Api-Secret-Token`。
   **secret 不匹配直接 401，不做任何降级**（v1 在未配置时 fail-open，是漏洞）。
2. `update_id` 在该桥内去重（唯一索引 + `ON CONFLICT DO NOTHING`），
   并区分"重复"与"约束失败"——后者必须报错，不能当成去重静默吞掉。
3. 写 `conversations`（upsert）与 `messages(direction=in)`。
4. `classify()` 判定是否回复：私聊全回；群/超群仅 @ 或引用机器人；
   机器人自己的消息不回；channel 与 edit 忽略。白名单不通过则不建 job。
5. 需要回复则建 `jobs(status=pending)`。

出站：

1. Muse 认领 → `processing` + `lease_id`。
2. Muse 调 `reply` → 桥调 Bot API → 写 `messages(direction=out)`。
3. Muse 调 `ack` → `done`。

自愈（v1 的死锁修复）：`reap()` 统一负责回收过期租约，
由 `/v1/pending-count`、`/v1/claim` 和后台巡检三个入口共用。
回收时 `attempts += 1`，超过上限进 `dead`。v1 只在 claim 里回收，
而 claim 只在 pending-count > 0 时才被调用，导致 agent 崩溃后消息永久卡死。

## 9. 安全设计

- 机器人 token 用 `ENCRYPTION_KEY`（32 字节）做 AES-256-GCM 加密后入库，
  任何接口不回显，只允许"重新生成"。
- Muse 令牌只存 sha256，签发时明文展示一次。
- 后台密码存 bcrypt 哈希；登录失败 5 次锁定 10 分钟。
- 限流：Muse API 按桥 120 次/分钟；webhook 按 slug 600 次/分钟。
- 所有出站 HTTP 只允许 `api.telegram.org`，禁用重定向。
- `.env` 与数据库文件不入库；备份说明里明确"数据库备份等同机器人凭证"。

## 10. 管理后台页面

1. **登录**
2. **桥列表**：卡片式，显示机器人 @用户名、待办数、今日收发条数、状态
3. **建桥向导**：单页表单，成功后展示接入信息（含一键复制）
4. **桥详情**：基本信息、白名单编辑、令牌列表（签发 / 吊销 / 最后使用时间）、
   暂停开关、webhook 状态
5. **会话列表**：按最后消息时间倒序，显示群名 / 用户名、消息数、最后一条摘要
6. **会话详情**：聊天气泡视图，左右区分进出，支持向上翻页、关键字过滤、
   显示工具链与时间戳、复制单条
7. **全局搜索**：关键字 + 时间范围 + 方向过滤
8. **队列与死信**：状态计数、死信列表、一键重放
9. **导出**：按会话或按桥导出 CSV / JSON，字段可选

## 11. 部署

```yaml
services:
  app:
    build: .
    environment:
      - DATABASE_PATH=/data/bridge.db
      - ENCRYPTION_KEY=${ENCRYPTION_KEY}
      - ADMIN_USERNAME=${ADMIN_USERNAME}
      - ADMIN_PASSWORD_HASH=${ADMIN_PASSWORD_HASH}
      - PUBLIC_BASE_URL=https://bridge.example.com
    volumes: [ "./data:/data" ]
  caddy:
    image: caddy:2
    ports: [ "80:80", "443:443" ]
    volumes: [ "./Caddyfile:/etc/caddy/Caddyfile", "caddy_data:/data" ]
```

`PUBLIC_BASE_URL` 决定写入 Telegram 的 webhook 地址，必须是外部可访问的 HTTPS。

## 12. 里程碑

| 阶段 | 内容 | 可验收 |
|---|---|---|
| M1 | 骨架、配置、迁移、仓储层、`classify` / `queue` 单测 | 纯本地跑测试，不接 Telegram |
| M2 | Bot API 客户端、webhook 入口、建桥闭环 | 建桥后 Telegram 能打进来并落库 |
| M3 | Muse API 全套 + 出站 sender + 幂等 | curl 模拟 Muse 完成一次完整往返 |
| M4 | 管理后台 | 浏览器里建桥、看记录、导出 |
| M5 | Docker 部署、文档、v1→v2 迁移说明 | 一台干净服务器从零跑起来 |

## 13. 已知限制（需要在文档中向使用者说明）

1. **群聊记录天然不完整。** Bot 在群里默认只收到 @ 和回复它的消息。
   要完整记录需对每个 bot 在 BotFather 执行 `/setprivacy` → Disable。
2. **桥是回复路径上的单点。** 换来完整记录，代价是桥故障会阻断出站；
   通过幂等键 + Muse 侧重试缓解，不追求消除。
3. **记录起点是建桥时刻**，Telegram 不补发历史消息。
4. **只有经桥发送的消息才在记录里。** 若有人绕过桥直连 Bot API 发消息，记录会断。
5. **单机架构。** SQLite 不支持多实例共享，横向扩展需要换 Postgres（仓储层已隔离）。

---

## 技术选型变更：Node + React → Go

初版设计按"一台常规服务器"假设选了 Node + TypeScript + React + Docker。
拿到目标机实测数据后这个前提不成立，故选型推翻重来。

实测环境（阿里云，香港）：

| 项 | 数值 |
|---|---|
| 系统 | Alpine 3.23，2 vCPU |
| 内存 | 435MB，无 swap |
| 磁盘 | 1GB（根分区可用 350MB + 数据分区 213MB） |
| 已有负载 | 无（x-ui 已清理） |

三条硬约束否掉了原方案：

1. **磁盘**：Docker 镜像层加 `node_modules` 至少 600MB，1GB 盘装不下。
2. **内存**：`vite build` / `tsc` 峰值 500MB–1GB，无 swap 必然 OOM。构建只能放在本地。
3. **依赖**：`better-sqlite3` 需要 python + gcc 编译，Alpine 上还要装一整条工具链。

Go 方案的代价与收益：

- 二进制 14MB（`-s -w`），常驻 RSS 约 10MB，无运行时依赖；
- `modernc.org/sqlite` 是纯 Go 实现，不需要 cgo；
- `html/template` + `go:embed` 把后台打进二进制，服务器上没有构建步骤；
- `autocert` 让进程自己申请和续期证书，省掉 Caddy 或 nginx。

代价是后台交互比 React 弱。这个后台的核心是"看记录、导出、重放死信"，
服务端渲染完全够用；真需要复杂交互时再加独立前端，不影响服务端。

原设计里关于数据模型、租约、幂等、draft/final 两段式回复的部分全部保留——
这些与技术栈无关。
