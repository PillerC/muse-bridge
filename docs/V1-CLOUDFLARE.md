# Telegram Bot 稳定接入 Muse：Cloudflare Worker + D1 消息队列

让你的 Muse 在 Telegram 上收发消息：Telegram webhook 先落到 Cloudflare
Worker + D1 队列，Muse 的 hook 每 5 秒检测，有新消息时唤醒 Muse 本人认领
并回复。不需要服务器，不暴露公网端口，重启、断网、处理卡住都不会丢消息。

```mermaid
flowchart LR
    TG[Telegram] -- webhook<br/>secret_token --> W[Cloudflare Worker]
    W -- 入队 --> D1[(D1 messages)]
    D1 -- hook 每 5 秒查<br/>/pending-count --> H[Muse hook]
    H -- 有消息唤醒 --> M[Muse 本人<br/>记忆 + 工具]
    M -- GET /poll 认领<br/>Bearer Token --> D1
    M -- 直接调 api.telegram.org<br/>typing + 占位 + 分段编辑 --> TG
```

为什么用拉取而不是让 Telegram 直接打 Muse：

- 不用开公网端口、不配域名证书
- Telegram webhook 有超时和重试风暴问题，Worker 先落库再慢慢消费，削峰填谷
- 处理端崩溃时消息只是租约过期后自动回 `pending`，不会丢也不会重复回复（`lease_id` 保护）

密钥存在 Muse 安全存储里，调用时经代理换成真 token，不经过任何服务器，
也不落在任何文件里。

延迟说明：检测 ≤5 秒，唤醒 + 思考约 37 秒，端到端约 46 秒。瓶颈在每次唤醒
agent 的启动和思考，检测本身很快。

## 一键部署（推荐）

最简单的办法：把下面这段提示词发给你的 Muse，让它帮你把整个项目部署好。

开始之前，请准备好：

- 一个 Telegram Bot：找 [@BotFather](https://t.me/BotFather) `/newbot` 创建，
  记下用户名；token（BOT_TOKEN）先不要发给任何人，部署时 Muse 会给你安全页面链接，
  你在页面里填写
- 一个 Cloudflare 账号（部署时会让你登录，免费额度够用）

然后复制下面的提示词（也可以直接复制 [PROMPT.md](PROMPT.md) 全文），发给你的 Muse：

```text
# 部署 telegram-muse-bridge

请帮我把这个项目部署好：https://github.com/goojia/telegram-muse-bridge

部署完成后，我在 Telegram 上私聊我的 Bot 发消息，你要能收到并回复我；
群里 @ Bot 或引用 Bot 的消息，你也要能回复。

## 我已经准备好的

- Telegram Bot 用户名：@（把这里换成你的 Bot 用户名）
- 一个 Cloudflare 账号（部署 Worker + D1 用，免费额度够用）
- Bot 的 token（BOT_TOKEN）：你需要的时候，我会通过你给我的安全页面填写，
  我不会直接发在聊天里

## 请按以下步骤部署

1. **准备仓库和环境**：把上面那个仓库克隆到你的工作区（如果已经有了就直接用）。
   确认环境里有 Node.js 18+、npm 和 wrangler，没有的话先装好
   （`npm i -g wrangler`）。如果 wrangler 需要登录，用浏览器完成 Cloudflare
   登录，需要我输入账号密码时告诉我。
2. **部署 Worker + D1**：
   - `cd worker && npm install`
   - `wrangler d1 create telegram-queue`，把返回的 database_id 填进
     `wrangler.toml`，并把 `BOT_USERNAME` 改成我的 Bot 用户名
   - `npm run db:migrate` 导入表结构
   - 生成两个随机 token（32 字符以上）：`WEBHOOK_SECRET` 和 `POLLER_TOKEN`，
     用 `wrangler secret put` 分别设置（经 stdin 提供，不要写进任何文件）
   - `npm run deploy`，记下 Worker 地址，然后 `curl <地址>/health` 验证通过
3. **注册两个自定义连接器**（走你自己的 custom_connector_setup 流程，
   密钥存在安全存储里）：
   - 把仓库里 `muse-connectors/` 下的两个目录复制到
     `~/workspace/custom-connectors/` 下
   - 把 `custom.telegram-queue/bin/telegram-queue.py` 里的 Worker 地址占位符
     改成实际的 Worker 地址（`HOSTS` 和 `BASE` 两处）
   - 注册 `telegram-bot-api`：host 为 `api.telegram.org`，api_key，
     放在 URL 路径 `url_path_segment:/bot{}/`；注册时给我安全页面链接，
     我在页面里填 BOT_TOKEN
   - 注册 `telegram-queue`：host 为 Worker 地址，api_key，`bearer_header`；
     注册时给我安全页面链接，我在页面里填 POLLER_TOKEN
   - 注册完后用 `get-me` 和 `stats` 验证两个连接器都能正常调用
   - 注意：`telegram-bot.py` 里代理是原样放进 URL 路径的，不要改成转码，
     否则 Telegram 会返回 404（仓库 README 的"踩过的坑"里有说明）
4. **设置 Telegram webhook**：用连接器调
   `telegram-bot.py set-webhook --url <Worker 地址>/telegram-webhook --secret <WEBHOOK_SECRET>`，
   用返回结果确认成功。
5. **创建 hook**：
   - 把 `hooks/telegram-pending.sh` 复制到 `~/hooks/scripts/`，
     把里面的 `WORKER_URL` 改成实际的 Worker 地址
   - 用 `hooks/worker-prompt.md` 里的指令创建一个 hook（5 秒间隔），
     先 dry run 确认检测脚本工作正常，再启用
6. **测试**：告诉我可以测试了，我会在 Telegram 私聊给 Bot 发一条消息；
   你确认消息被处理并 ack 后，告诉我实测延迟。

## 安全要求（必须遵守）

- 绝不要让我把任何 token 直接发在聊天里；需要我提供 token 时，
  一律用安全页面链接让我填写
- 绝不要把任何密钥写进文件、记忆、日志或对话摘要；生成的随机 token
  只经环境变量或 stdin 临时使用
- 部署完成后告诉我：延迟预期（约 40 秒）、如何使用、以及以后怎么看队列状态

## 如果遇到问题

- 哪一步卡住了就停下来告诉我具体是哪一步、报错是什么，不要跳过
- 不要为了赶进度省略验证步骤
```

部署过程中需要你配合的地方只有两处：在安全页面里填两个 token，
以及 Cloudflare 登录。其他步骤 Muse 都会自己完成。

## 回复规则

| 场景 | 是否回复 |
|---|---|
| 私聊 | 全部回复 |
| 群 / 超群里 `@Bot` | 回复 |
| 群 / 超群里引用 Bot 的消息 | 回复 |
| 群 / 超群其他消息 | 不回复，只入队记为 `skipped` |
| Bot 自己发的消息 | 绝不回复（防自激循环） |
| channel / edited 等 | 不入队处理 |

过滤在 Worker 入队时完成。

## 回复体验

1. **输入中**：处理期间发 `sendChatAction typing`（有效期约 5 秒，需续发）
2. **占位消息**：先发一条 `💭 思考中…` 并引用原消息
3. **工具链**：每调一次工具，正文顶部追加一行 `• 工具名: 简述`
4. **分段编辑**：长回复分 2-3 次渐进编辑占位消息，间隔至少 2 秒，遵守 Telegram 429 `retry_after`
5. **完成**：最终编辑定稿；超 4096 字符的部分分段续发

## 目录结构

```
worker/
  src/index.js      Worker: webhook 入队 / poll / ack / fail / extend / stats / pending-count
  schema.sql        D1 表结构
  wrangler.toml     Worker + D1 配置（敏感值用 wrangler secret 设置，不写在这里）
  package.json
  .dev.vars.example 本地开发用的 secret 模板
muse-connectors/    Muse 自定义连接器（密钥存安全存储）
  custom.telegram-bot-api/
    bin/telegram-bot.py   Bot API helper: typing / 发送 / 编辑 / getMe
    CONNECTOR.md          连接器文档
    connector.yaml
  custom.telegram-queue/
    bin/telegram-queue.py 队列 helper: poll / ack / fail / extend / stats
    CONNECTOR.md
    connector.yaml
hooks/
  telegram-pending.sh   每 5 秒查 /pending-count，有消息唤醒 agent
  worker-prompt.md      唤醒后 agent 的工作指令
```

## 运维

```bash
# 队列状态（pending/processing/done/dead 计数 + 最近死信）
curl https://<你的worker>/stats -H "Authorization: Bearer <POLLER_TOKEN>"

# 看死信详情
wrangler d1 execute telegram-queue \
  --command="SELECT id, chat_id, text, attempts, last_error FROM messages WHERE status='dead' ORDER BY updated_at DESC LIMIT 10;"

# 死信重放（改回 pending 即可被重新拉取）
wrangler d1 execute telegram-queue \
  --command="UPDATE messages SET status='pending', attempts=0, next_retry_at=0 WHERE status='dead' AND id=<id>;"

# 清理已完成旧数据（建议每月一次）
wrangler d1 execute telegram-queue \
  --command="DELETE FROM messages WHERE status IN ('done','skipped') AND created_at < strftime('%s','now') - 30*86400;"
```

## 稳定性清单（已实现）

- **去重**：`update_id UNIQUE` + `INSERT OR IGNORE`，Telegram 重发 webhook 不会重复入队
- **租约**：poll 认领写 `lease_id`，ack/fail 必须带回同一租约；超时未确认自动释放重跑
- **退避重试**：失败后 10s → 20s → 40s… 指数退避，5 次后进 `dead` 不再自动拉取
- **续租**：长回答每 60 秒续租一次，防处理中途被重复认领
- **限速遵守**：edit 节流 1.2 秒/次，429 按 `retry_after` 等待；`message is not modified` 正常忽略
- **无服务器**：没有常驻进程，不存在服务崩溃；消息靠租约过期自愈
- **鉴权**：Telegram 侧 `secret_token`，拉取侧 `Bearer POLLER_TOKEN`，密钥全走 secret / 安全存储
- **唤醒去重**：hook 检测脚本 90 秒静默期 + 数量变化才唤醒，重复唤醒时租约保证不重复回复

## 踩过的坑

- **安全存储的代理不要转码**：Muse 安全存储返回的 `hsurr:` 代理放在 URL 路径里时必须原样放，percent-encoding 之后 Sentinel 就识别不了，请求会带着一个畸形 token 到达 Telegram，返回 404 而不是 401。`telegram-bot.py` 里特意绕过了官方 helper 的转码逻辑，实测验证过。
- **workers.dev 拦截 Python urllib 默认 UA**：Cloudflare 会以 1010 拦截 `Python-urllib/x.y` 的请求。所有调 Worker 的客户端都要自己设一个 User-Agent，`telegram-queue.py` 已处理。
- **typing 只有约 5 秒**：`sendChatAction` 的输入中指示 5 秒就消失，长处理必须循环续发；占位消息是更可靠的"正在处理"信号。
- **edit 有频率限制**：同一条消息约 1 次/秒，太快会 429。流式编辑必须节流，长回复分段编辑时也要留出间隔。
- **租约和静默期要对齐**：租约默认 180 秒，hook 的 90 秒静默期保证 agent 崩溃后能在租约过期前后被重新唤醒；两个值不要随意单改一个。
- **webhook 注入测试需要 secret**：`/telegram-webhook` 校验 `secret_token`，没有它没法用 curl 伪造消息做测试。端到端测试请直接在 Telegram 里发一条真实消息。

## 常见问题

**群里 @ 了没反应？** 按顺序查：`curl /stats` 看有没有 `pending`；hook 日志有没有唤醒记录；`BOT_USERNAME` 是否填对（不带 @）。

**私聊有回复、群里没有？** 多半是过滤规则：确认 @ 的是正确的 Bot 用户名，或引用的是 Bot 发的消息；`/stats` 里 `skipped` 增多说明消息到了但没触发。

**回复一半卡住？** 看 hook 日志和唤醒记录；租约过期后消息会自动重跑一次。

**出现重复回复？** 通常是处理超过租约又没续租成功。检查 `LEASE_SECONDS` 是否大于正常处理时长；租约保证同一时间只有一个 agent 能处理同一条消息。

**Telegram 报 429？** 正常，helper 已按 `retry_after` 退避；如果频繁出现，把编辑间隔调大。

**唤醒后报 credential / surrogate 错误？** 连接器出问题了，在 Muse 里用连接器文档中记录的注册信息重新连接，不要反复重试。

## 安全说明

- 本仓库不含任何真实密钥。所有密钥（`BOT_TOKEN`、`POLLER_TOKEN`、`WEBHOOK_SECRET`）只通过 `wrangler secret` 或 Muse 安全存储设置，`.gitignore` 已排除 `.dev.vars` 和 `.wrangler/`。
- `/pending-count` 是公开端点，但只返回一个数字，不含任何消息内容或身份信息。
- 不要把 `.dev.vars` 或聊天记录里的 token 贴到 issue 里。

## License

MIT，见 [LICENSE](LICENSE)。
