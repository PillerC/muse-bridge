# Muse Bridge

Muse ↔ Telegram 的多租户通信中枢。一座桥 = 一个 Telegram 机器人 + 一个 Muse 接入点，
消息双向落库，管理后台可以查记录、导出、看队列、重放死信。

自建、单进程、单文件二进制 + 一个 SQLite。常驻内存约 50MB，磁盘约 20MB。

```text
Telegram ──webhook（带 secret）──► ┌─────────────────────────┐
                                   │  Muse Bridge            │
Muse ──poll/reply（Bearer 令牌）──►│  SQLite：双向聊天流水    │
                                   │  + 租约队列 + 后台       │
浏览器 ──域名首页（登录后）───► └─────────────────────────┘
                                        │
                                        └──► api.telegram.org（桥代发）
```

机制很简单，全在一个进程里：

- **收消息**：Telegram webhook → 桥入库 → hook 每 5 秒查 `/pending-count` 唤醒 Muse → Muse `/poll` 带租约认领。
- **发消息**：Muse 不直连 Telegram，而是调桥的 `/reply`。这样机器人 token 只在桥里，Muse 永远看不到，同时出站消息也进了记录。
- **多租户**：每座桥有独立的机器人、独立令牌、独立记录，互不可见。

## 为什么发消息要过一次桥

如果让 Muse 直连 `api.telegram.org`，可以省一次转发。代价是出站消息没有记录，
而且 bot token 必须放在 Muse 侧。现在为了让「完整聊天记录」成立，出站必须经过桥。

桥把原来压在 prompt 里的活接了过来：认领即显示「输入中」、4096 字符分段、429 退避、
Markdown 解析失败回退纯文本、写入库、自动 ack。Muse 只需要产出一段文本。

## 快速开始

要求很低，但有两个前提：**64 位 Linux（或 macOS）** 和 **一个解析到它的域名**
（webhook 需要有效 HTTPS 证书）。**80 和 443 端口要从公网可达**。

### 方式一：一键安装（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/goojia/muse-bridge/main/install.sh | sudo sh
```

装完按提示填域名，或者一步到位：

```bash
curl -fsSL https://raw.githubusercontent.com/goojia/muse-bridge/main/install.sh \
  | sudo sh -s -- --domain bridge.example.com --email you@example.com
```

脚本会：下载对应平台的二进制 → 校验 sha256 → 装到 `/opt/muse-bridge` →
生成含主密钥的配置（600 权限）→ 注册 systemd 或 OpenRC 服务 → 启动并验证。

**幂等**：重复执行不会覆盖配置、不会重新生成主密钥（换了等于所有机器人 token 作废）。
已装过时会提示加 `--force` 才覆盖二进制。

常用选项：

| 选项 | 说明 |
|---|---|
| `--domain <域名>` | webhook 用的域名，不填则装完手动改配置 |
| `--email <邮箱>` | Let's Encrypt 通知邮箱 |
| `--prefix <目录>` | 安装目录，默认 `/opt/muse-bridge` |
| `--from-source` | 从源码编译安装（需要 git 和 Go 1.27+） |
| `--version <tag>` | 装指定版本，默认最新 |
| `--base-url <地址>` | 自建镜像/内网分发源 |
| `--no-service` | 只装二进制和配置，不注册服务 |
| `--dry-run` | 只打印要做什么，不改动任何东西 |
| `--uninstall` | 卸载（默认保留数据；加 `--purge` 连数据一起删） |

也可以先下载下来看看再执行：

```bash
curl -fsSLO https://raw.githubusercontent.com/goojia/muse-bridge/main/install.sh
less install.sh          # 先读一遍
sudo sh install.sh --domain bridge.example.com
```

### 方式二：从源码编译

```bash
git clone https://github.com/goojia/muse-bridge.git
cd muse-bridge
sudo sh install.sh --from-source --domain bridge.example.com
```

或者手动编译再按下面的「手动安装」部署：

```bash
cd bridge
ALL=1 bash scripts/build.sh        # 产出 dist/ 下各平台二进制
# 或只编当前平台：
GOOS=linux GOARCH=amd64 bash scripts/build.sh
```

### 方式三：手动安装

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

后台就是域名首页 `https://<域名>/`。**首次打开会让你创建管理员账号**（用户名 + 至少 8 位密码），
建完直接进后台，没有默认密码这回事。
也因此**部署完要立刻打开创建**：还没建号的实例，谁先访问谁就能成为管理员。

```text
https://bridge.example.com/
```

进程同时监听 `BRIDGE_ADDR`（默认 `127.0.0.1:8080`），域名和证书出问题时
可以 `ssh -L 8080:127.0.0.1:8080` 走隧道直接访问。

填两样东西：桥名字、机器人 token（@BotFather 拿）。slug 由服务端随机生成，
不让人填——它只出现在 webhook 地址里，手填只会制造拼错和撞名的机会。点创建后桥会：

1. 调 `getMe` 校验 token，顺手记下 bot 的 id 和 username；
2. 用 `BRIDGE_SECRET` 加密 token 存库，之后任何界面都不回显；
3. 生成 `webhook_secret` 和一枚桥令牌；
4. 自动调 `setWebhook` 指向 `https://<域名>/tg/<slug>`。

详情页上会分两块给出**接入提示词**和**令牌**，各有独立的复制按钮：

- 提示词里只有桥地址、桥标识和接口用法，**不含令牌**——它是要整段转发出去的东西；
- 令牌单独一段，在这页随时能再复制，不用重新签发。

### 3. 让 Muse 接入

详情页的「接入 Muse」卡里，把**提示词**整段复制发给 Muse，再单独把**令牌**给它
（提示词里不含令牌，是可以随手转发的部分）。提示词里有桥地址、桥标识和接口用法，
Muse 照它写一个 HTTP 客户端就能跑，不需要本仓库提供任何客户端代码。

之后用它自检：

```bash
curl -H "Authorization: Bearer <令牌>" https://<域名>/stats     # 确认连通
curl -H "Authorization: Bearer <令牌>" "https://<域名>/poll?limit=5"
```

hook 脚本用 `hooks/telegram-pending.sh`，只改 `MUSE_BRIDGE_URL`。

## API

Muse 用 `Authorization: Bearer <桥令牌>` 调用。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/pending-count?bridge=<slug>` | 待处理数。**可匿名**，hook 用；`bridge` 或令牌必须给一个，两者都没有返回 400 |
| GET | `/poll?limit=5&lease_seconds=180` | 认领，返回 `messages[]` + `lease_id` |
| POST | `/ack` | `{ids:[], lease_id}` |
| POST | `/fail` | `{id, lease_id, error}`，指数退避，超限进死信 |
| POST | `/extend` | `{id, lease_id, lease_seconds}` 续租 |
| GET | `/stats` | 状态分布 + 最近死信 |
| POST | `/reply` | `{id, lease_id, text, mode:draft\|final, parse_mode}`，默认自动 ack |

`/poll` 返回的每条消息都带自己的 `lease_id`，避免两批消息混在一起时 ack 用错租约。

`/pending-count` 必须指明是哪座桥：带 `?bridge=<slug>`（hook 用这个，匿名可调）
或带 Bearer 令牌。**两者都没有时返回 400**，不会退回到"所有桥合计"。

早期版本在不带参数时返回合计，那是个陷阱：任何一座桥有消息，所有装了 hook 的
agent 都会被叫醒，醒来 `poll` 却只领得到自己那座桥的消息，等于白烧一轮推理。
现在让它明确失败，配错 hook 会立刻暴露。

`slug` 只是公开标识（详情页标题旁就印着），只能读到一个数字，不是凭据。

`/reply` 的任务 id 字段是 `id`；`mode` 只能是 `draft` 或 `final`，省略按 `final` 处理
（`draft` 建占位消息、后续调用编辑同一条，`final` 定稿并自动 ack）。字段名写错会被拒（400），
不会静默当成别的字段。

## 管理后台

- **总览**：今日消息、待处理、处理中、死信，以及所有桥的状态
- **桥详情**：接入提示词与令牌（复制 / 轮转）、会话记录、设置（名字、用户 ID、收消息方式、暂停 / 删除）
- **聊天记录**：按会话看双向消息，带任务状态
- **导出**：按桥 / 会话 / 时间范围导出 CSV（带 BOM，Excel 直接开）或 JSON
- **死信重放**：修好问题后一键放回队列
- **账号**：改后台的用户名和密码。账号只由首次访问时创建，env 不参与；
  忘记密码就设 `BRIDGE_ADMIN_RESET=1` 并用 env 写上要重置成的账号密码，重启覆盖回去
- **升级**：查 GitHub Releases、下载当前平台的二进制、校验 sha256、替换自身文件后原地重启；
  也可以打开定时升级，每天到点自动升级。见下节

## 在线升级

后台「升级」页可以把正在跑的程序换成 GitHub Releases 里的新版本，不用登服务器。

### 修改功能的推荐流程

服务器上跑的是**发布版二进制**，不在服务器上编译。改功能的流程是：

```bash
# 1. 本地改代码、跑测试
cd bridge && go test ./...

# 2. 提交并推送
git add -A && git commit -m "feat: ..."
git push origin main

# 3. 打 tag 触发发布（GitHub Actions 自动编译四平台 + 发 Release）
git tag v1.3.1 && git push origin v1.3.1

# 4. 等 Actions 跑完，在服务器后台「升级」页点「立即升级」
#    或者打开定时升级，让它自己换
```

服务器不需要 Go 工具链，也不需要 git——只下载一个 14MB 的二进制并校验替换。
这样「本地开发 → 发布 → 服务器升级」三件事彻底解耦，服务器上永远跑的是
和 Release 完全一致的东西。

```text
后台「升级」→ 检查更新 → 立即升级
                │
                ├─ GET api.github.com/repos/goojia/muse-bridge/releases
                ├─ 下载 bridge-<os>-<arch>  +  checksums.txt
                ├─ 校验 sha256（不符就中止，旧文件一个字节都不动）
                ├─ 旧版本备份成 bridge.bak，rename 原子替换
                └─ syscall.Exec 换成新二进制 → PID 不变，systemd/OpenRC 无感
```

几个刻意的取舍：

- **只认 GitHub Releases**，不在用户机器上 `git pull` + `go build`：用户不需要 Go 工具链和 git，
  也不会因为本地改动而编译失败。发布产物由 `.github/workflows/release.yml` 在打 tag 时自动产出
  （`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64` + `checksums.txt`）。
- **必须校验 sha256**：下载下来的是要直接执行的文件，只靠 HTTPS 不足以信任。
  发布里没有 `checksums.txt` 时宁可拒绝升级。
- **exec 而不是 exit**：`exit(0)` 依赖服务管理器的自动重启配置，用户用 `nohup` 起的话升级就等于停服。
  exec 保留 PID 和 cgroup，任何启动方式都不用改配置。
- **定时升级默认关闭**：半夜自动换掉正在跑的程序，这件事必须由用户自己打开。
- **自己编译的版本不参与定时升级**：版本号是 `dev` 或提交哈希时和发布号没有大小关系，
  自动升级无从判断"是不是已经最新"，每天重下一遍只是浪费流量。手动点「立即升级」仍然可用，
  升一次之后就变成正式版本号，定时升级随之正常。
- **失败不动旧版本**：下载、校验、替换任何一步出错都保持原样继续跑旧版本，页面给出失败原因。
  升级完重启后发现版本没变，会明确记一条"升级未生效"，并保留 `bridge.bak` 可回滚。

相关的环境变量（都有合理默认，通常不用配）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_UPGRADE_REPO` | `goojia/muse-bridge` | 从哪个仓库取发布 |
| `BRIDGE_UPGRADE_API` | `https://api.github.com` | GitHub API 基址，企业版或镜像可改 |
| `BRIDGE_UPGRADE_DISABLED` | 空 | 设为 `1` 彻底关掉在线升级（连入口都不给） |

如果二进制所在目录不可写（比如放在只读挂载或 `/usr/local/bin` 且服务账号无权限），
升级会明确报"目录不可写"而不是留下半坏状态。

发版流程：`git tag v1.2.3 && git push origin v1.2.3`，等 Actions 跑完，
各平台二进制和 `checksums.txt` 就挂在 Release 上了。

## 归档与在线备份

聊天记录可以按天切成独立文件，自动上传到对象存储或网盘，本地只保留最近若干天。

```text
每天 00:00 后第一轮（每小时检查一次）
   │
   ├─ 归档：把还没归档的日子写成 YYYY-MM-DD.jsonl.gz
   ├─ 上传：把所有"已归档但未上传"的文件传上去
   └─ 清理：只删"已上传成功"且早于保留期的本地记录
```

后台「备份」页可以手动执行一轮、单独补传某一天、设置 S3 或 WebDAV 目标、
设置本地保留天数。

几个刻意的取舍：

- **一天一个文件**：`YYYY-MM-DD.jsonl.gz`，JSON Lines 加 gzip。流式读写，
  不用整月进内存；`gzip -dc 2026-11-22.jsonl.gz | head` 就能看。
  备份时也只需要传一天，不用每次重传整库。
- **归档不删数据**。删不删本地是保留策略的事，两件事分开：
  用户可能只想归档不想删，也可能想删但不想归档。
- **上传成功才允许删**。这是这个功能的安全底线：
  某天上传失败，那天的本地消息一律保留。否则一次网络抖动就是永久丢数据。
  没开在线备份时也禁止开自动清理，同样是为了这条底线。
- **按本地时区切天**。用户说的"今天"是他所在时区的今天，用 UTC 切会错位。
- **凭据加密存储**，与机器人 token 同一把钥匙，任何接口都不回显。
  表单里留空表示"不修改已保存的值"。

S3 兼容存储（AWS S3 / MinIO / Cloudflare R2 / 阿里云 OSS / 腾讯云 COS）走
自己实现的 SigV4 签名，WebDAV（Nextcloud / 坚果云 / 群晖）走标准 HTTP 方法。
都不引第三方 SDK——AWS SDK 会给单文件二进制加十几 MB，而我们只需要
PUT / DELETE / PROPFIND 三件事。

| 变量 | 默认 | 说明 |
|---|---|---|
| `BRIDGE_ARCHIVE_DIR` | `<数据目录>/archive` | 归档文件存放目录 |
| `BRIDGE_BACKUP_DISABLED` | 空 | 设为 `1` 关掉归档与在线备份 |

## 安全模型

- 机器人 token：AES-256-GCM 加密，密钥只在环境变量里，任何接口都不回显。
- 桥令牌：库里有 SHA-256 摘要（鉴权用）和 AES-256-GCM 密文（后台再次复制用）。
  同一时刻只有最新一枚有效：点「轮转令牌」先生成新的、再把旧的全部作废，
  中间不留零令牌的瞬间，所以换令牌不会让正在跑的 Muse 断在半路。
  密文与机器人 token 同一把钥匙，所以密钥泄露等于两者一起泄露——
  这也是把 `BRIDGE_SECRET` 只放环境变量、不进库的原因。
- webhook：`X-Telegram-Bot-Api-Secret-Token` 必须匹配，不匹配直接 403。**没有 fail-open**。
- 后台：就是域名首页，未登录只有登录页；CSP 只允许同源自带脚本（复制按钮）、登录失败按 IP 限流。
- 允许列表：按「发消息的人」（@userinfobot 给的数字 ID）过滤，不看会话。
  名单外的人无论私聊还是在群里 @ 都不会入库，也不排队。
  留空表示谁都不收（fail-closed），不存在"忘了配就等于全网可用"。
- 多租户：令牌决定桥，跨桥访问一律按不存在处理。

## 已知限制

- **群聊记录是否完整取决于 Telegram 的 privacy mode**。bot 默认只收到 @ 它的消息；要完整记录得去 BotFather 执行 `/setprivacy` → Disable。
- **记录从建桥开始**，历史消息 Telegram 不补发。
- 只转发文本。图片/语音/文件记录元信息（`content_hint`），不下载内容。
- 桥是回复路径上的单点。它挂了就发不出去——换来的是完整记录和 token 不外流。

## 仓库结构

```text
install.sh        一键安装脚本（预编译二进制 / 源码编译）
bridge/           v2 服务端（Go）
  cmd/bridge/     入口
  internal/       config / store / domain / tg / server / poller / secret
                  upgrade / version / archive（按天归档）/ backup（S3、WebDAV）
  scripts/        build.sh 交叉编译（ALL=1 出全部平台 + checksums.txt）
  dist/           构建产物（不进 git）
.github/          发布流水线（打 tag 自动编译并上传 Release）
hooks/            Muse hook 脚本（telegram-pending.sh）
deploy/           OpenRC 服务定义 + 环境变量模板
docs/             DESIGN.md（设计与取舍）、DEPLOY.md（运维）
```

## 开发

```bash
cd bridge
go test ./...                 # 单元 + 端到端（假 Telegram 端点、假 GitHub Releases）
GOOS=linux GOARCH=amd64 bash scripts/build.sh   # 单平台
ALL=1 bash scripts/build.sh                     # 发布用的全部平台 + checksums.txt
```

测试覆盖：过滤规则（含 `@muse` vs `@musebot`、UTF-16 emoji 边界）、租约回收、
并发认领、退避序列、跨桥隔离、webhook 鉴权、重复投递去重、draft/final 编辑、
长消息分段、后台页面渲染、版本号比较、升级包校验与原子替换、升级失败不动旧文件、
定时升级触发、回滚。
