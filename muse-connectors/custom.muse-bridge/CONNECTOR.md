# Muse Bridge

- `custom.muse-bridge` 连接一台自建的 Muse Bridge。它替代 v1 的 `custom.telegram-queue` **和** `custom.telegram-bot-api`：收消息和发回复都走桥，不再需要机器人 token。

- 桥持有机器人 token、保存双向聊天记录。Muse 只拿两个值：**接入地址**和**桥令牌**。

- Helper 在 `~/workspace/custom-connectors/custom.muse-bridge/bin/muse-bridge.py`。

## Connect

- 向用户索取「接入地址」和「桥令牌」（后台建桥时生成）。注册连接：

```json
{
  "provider": "muse-bridge",
  "auth_scheme": "api_key",
  "placement": "bearer_header",
  "api_hosts": ["<接入地址的域名>"]
}
```

- 接入地址就是后台详情页显示的 `https://…`，helper 会把它作为 BASE。换桥时只改 `MUSE_BRIDGE_URL` 或 helper 顶部的 `BASE`。

- 先做一次无害读，确认连通：

```text
python3 ~/workspace/custom-connectors/custom.muse-bridge/bin/muse-bridge.py stats
```

## Credentials

- 令牌走 `dynamic_credentials` 的 `hsurr:` 代理，只在访问已注册 host 时由 Muse 替换成真值。

- **不要**把令牌写进文档、脚本、命令行参数、环境变量或日志。**不要**向用户索要机器人 token——桥里已经有了。

- 认证请求禁止跟随重定向。

## 主流程

1. 检查有没有待处理消息：

```text
python3 .../muse-bridge.py pending-count
```

2. 认领（带租约，租约要覆盖你完成回复的时间）：

```text
python3 .../muse-bridge.py poll --limit 5 --lease-seconds 180
```

输出里有 `lease_id` 和 `messages`。每条消息包含 `id`、`message_id`、`chat_id`、`chat_type`、`from_name`、`from_username`、`text`、`content_kind`、`content_hint`、`reply_to_message_id`、`is_mention`、`is_reply_to_bot`、`attempts`、`lease_id`。

- 没有消息就结束这一轮，不要回复任何东西。
- `content_kind` 不是 `text` 时，`content_hint` 里有可读描述（例如 `[图片] 1280x720，配文：…`），`text` 可能为空。不要假装看见了图片内容。

3. 回复。把回答写进文件再发，避免 shell 转义问题：

```text
python3 .../muse-bridge.py reply --id <id> --lease-id <lease_id> --text-file /tmp/reply.txt
```

- 桥会自动完成：typing 保活、4096 字符分段、429 退避、Markdown 解析失败时回退纯文本、写做出站记录、**自动 ack**。发完不用再调 `ack`。
- 需要"思考中…"占位时先发一次 `--mode draft`，再发 `--mode final` 覆盖同一条消息。

4. 只有在你确实没能完成回复时才调用 `fail`：

```text
python3 .../muse-bridge.py fail --id <id> --lease-id <lease_id> --error "模型超时"
```

- 桥按 10s / 20s / 40s… 退避重试，超过上限进死信，后台可以重放。

5. 长任务在租约过半时续期：

```text
python3 .../muse-bridge.py extend --id <id> --lease-id <lease_id> --lease-seconds 180
```

## 注意

- `--parse-mode` 默认 `Markdown`。如果回答里有大量下划线或未配对的 `*`、`_`，桥会自动退回纯文本重发一次。
- 收到 409 `lease_mismatch` 表示租约过期或消息已被别的 agent 认领：**不要重试发送**，丢掉这条，继续下一条。
- 一座桥对应一个机器人。多座桥就注册多个连接，各自的令牌互不相通。

## Limits

- 桥只转发文本。图片、语音、文件会记录元信息，但不会下载内容。
- 群聊记录是否完整取决于 Telegram 的 privacy mode；需要完整记录时，让用户去 BotFather 对机器人执行 `/setprivacy` → Disable。
