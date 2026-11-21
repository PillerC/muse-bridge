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
