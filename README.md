# Muse Bridge

Muse ↔ Telegram 的多租户通信中枢。一座桥 = 一个 Telegram 机器人 + 一个 Muse 接入点，
消息双向落库，管理后台可以查记录、导出、看队列、重放死信。

自建、单进程、单文件二进制 + 一个 SQLite。常驻内存约 10MB，磁盘约 20MB。

```text
Telegram ──webhook（带 secret）──► ┌─────────────────────────┐
                                   │  Muse Bridge            │
Muse ──poll/reply（Bearer 令牌）──►│  SQLite：双向聊天流水    │
                                   │  + 租约队列 + 后台       │
浏览器 ──域名首页（登录后）───► └─────────────────────────┘
                                        │
                                        └──► api.telegram.org（桥代发）
```

和 v1（Cloudflare Worker）相比，变的不是机制，是位置和职责：

- **收消息**：Telegram webhook → 桥入库 → hook 每 5 秒查 `/pending-count` 唤醒 Muse → Muse `/poll` 带租约认领。和 v1 一模一样。
- **发消息**：Muse 不再直连 Telegram，改为调桥的 `/reply`。这样机器人 token 只在桥里，Muse 永远看不到，同时出站消息也进了记录。
- **多租户**：每座桥有独立的机器人、独立令牌、独立记录，互不可见。

## 为什么发消息要过一次桥

v1 让 Muse 直连 `api.telegram.org`，省了一次转发。代价是出站消息没有记录，
而且 bot token 必须放在 Muse 侧。现在为了让"完整聊天记录"成立，出站必须经过桥。

桥把原来压在 prompt 里的活接了过来：认领即显示「输入中」、4096 字符分段、429 退避、
Markdown 解析失败回退纯文本、写入库、自动 ack。Muse 只需要产出一段文本。

## 快速开始

### 1. 服务器

要求很低，但有两个前提：**64 位 Linux** 和 **一个解析到它的域名**（webhook 需要有效 HTTPS 证书）。

```bash
mkdir -p /data/bridge /data/bridge-data /data/log
# 上传 dist/bridge-linux-amd64 到 /data/bridge/bridge
chmod +x /data/bridge/bridge
```

生成主密钥（加密机器人 token 用，丢了就得重建所有桥）：

```bash
head -c 32 /dev/urandom | base64
```

写 `/etc/muse-bridge.env`（`chmod 600`），模板见 `deploy/muse-bridge.env.example`：

```ini
BRIDGE_DATA_DIR=/data/bridge-data
BRIDGE_ADDR=127.0.0.1:8080
BRIDGE_PUBLIC_ADDR=0.0.0.0:443
BRIDGE_PUBLIC_URL=https://bridge.example.com
BRIDGE_TLS=autocert
BRIDGE_ACME_EMAIL=you@example.com
BRIDGE_ADMIN_USER=admin
BRIDGE_ADMIN_PASSWORD=<强密码>
BRIDGE_SECRET=<上面生成的 base64>
BRIDGE_MODE=webhook
```

启动（Alpine/OpenRC）：

```bash
cp deploy/muse-bridge.openrc /etc/init.d/muse-bridge && chmod +x /etc/init.d/muse-bridge
rc-update add muse-bridge default
rc-service muse-bridge start
```

证书由进程自己通过 `autocert` 申请和续期，不需要 nginx 或 certbot。
**80 端口要对外开放**（ACME HTTP-01 挑战）。

### 2. 进后台建一座桥

后台就是域名首页 `https://<域名>/`，用环境变量里的账号密码登录。

```bash
https://bridge.example.com/
# 另有一个只绑 127.0.0.1:8080 的兜底入口，TLS 出问题时可用 SSH 隧道访问
```

填三样东西：桥名字、slug、机器人 token（@BotFather 拿）。点创建后桥会：

1. 调 `getMe` 校验 token，顺手记下 bot 的 id 和 username；
2. 用 `BRIDGE_SECRET` 加密 token 存库，之后任何界面都不回显；
3. 生成 `webhook_secret` 和一枚桥令牌；
4. 自动调 `setWebhook` 指向 `https://<域名>/tg/<slug>`。

页面上会出现**接入地址**和**令牌**（明文只显示一次），把它们发给 Muse。

### 3. 让 Muse 接入

把 `muse-connectors/custom.muse-bridge/` 装进 Muse，注册连接时填域名，令牌走凭证代理。
之后：

```bash
python3 muse-bridge.py stats          # 确认连通
python3 muse-bridge.py poll           # 认领
python3 muse-bridge.py reply --id 1 --lease-id <id> --text-file reply.txt
```

细节见 `muse-connectors/custom.muse-bridge/CONNECTOR.md`。
hook 脚本用 `hooks/telegram-pending.sh`，只改 `MUSE_BRIDGE_URL`。

## API

Muse 用 `Authorization: Bearer <桥令牌>` 调用。前四个与 v1 的响应格式完全一致，
所以旧 connector 换掉 host 就能跑。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/pending-count` | 待处理数。**可匿名**，hook 用；带令牌时只统计该桥 |
| GET | `/poll?limit=5&lease_seconds=180` | 认领，返回 `messages[]` + `lease_id` |
| POST | `/ack` | `{ids:[], lease_id}` |
| POST | `/fail` | `{id, lease_id, error}`，指数退避，超限进死信 |
| POST | `/extend` | `{id, lease_id, lease_seconds}` 续租 |
| GET | `/stats` | 状态分布 + 最近死信 |
| POST | `/reply` | `{id, lease_id, text, mode:draft\|final, parse_mode}`，默认自动 ack |

`/poll` 返回的每条消息都带自己的 `lease_id`，避免两批消息混在一起时 ack 用错租约。

## 管理后台

- **总览**：今日消息、待处理、处理中、死信，以及所有桥的状态
- **桥详情**：接入地址、webhook 状态、令牌签发与吊销、白名单、暂停/恢复、删除
- **聊天记录**：按会话看双向消息，带任务状态
- **导出**：按桥 / 会话 / 时间范围导出 CSV（带 BOM，Excel 直接开）或 JSON
- **死信重放**：修好问题后一键放回队列

## 安全模型

- 机器人 token：AES-256-GCM 加密，密钥只在环境变量里，任何接口都不回显。
- 桥令牌：只存 SHA-256 摘要，可多枚并存（换机不断服），可单独吊销。
- webhook：`X-Telegram-Bot-Api-Secret-Token` 必须匹配，不匹配直接 403。**没有 fail-open**。
- 后台：只绑 `127.0.0.1`，公网端口上不存在后台路由。
- 多租户：令牌决定桥，跨桥访问一律按不存在处理。

## 已知限制

- **群聊记录是否完整取决于 Telegram 的 privacy mode**。bot 默认只收到 @ 它的消息；要完整记录得去 BotFather 执行 `/setprivacy` → Disable。
- **记录从建桥开始**，历史消息 Telegram 不补发。
- 只转发文本。图片/语音/文件记录元信息（`content_hint`），不下载内容。
- 桥是回复路径上的单点。它挂了就发不出去——换来的是完整记录和 token 不外流。

## 仓库结构

```text
bridge/           v2 服务端（Go）
  cmd/bridge/     入口
  internal/       config / store / domain / tg / server / poller / secret
  scripts/        build.sh 交叉编译
  dist/           构建产物（不进 git）
web/              （保留）v1 的前端占位
worker/           v1 Cloudflare Worker，已弃用，仅作对照
muse-connectors/
  custom.muse-bridge/   v2 连接器（收 + 发）
  custom.telegram-queue/  v1 连接器，已弃用
  custom.telegram-bot-api/ v1 直连 Telegram，已弃用
hooks/            Muse hook 脚本
deploy/           OpenRC 服务定义 + 环境变量模板
docs/             DESIGN.md（设计）、V1-CLOUDFLARE.md（v1 部署）、DEPLOY.md（运维）
```

## 开发

```bash
cd bridge
go test ./...                 # 单元 + 端到端（假 Telegram 端点）
GOOS=linux GOARCH=amd64 bash scripts/build.sh
```

测试覆盖：过滤规则（含 `@muse` vs `@musebot`、UTF-16 emoji 边界）、租约回收、
并发认领、退避序列、跨桥隔离、webhook 鉴权、重复投递去重、draft/final 编辑、
长消息分段、后台页面渲染。
