# 运维手册

面向已经跑起来的部署。首次部署看 README 的「快速开始」。

## 安装

三种方式，任选其一：

```bash
# 1. 一键安装（推荐）：下载预编译二进制 + 校验 + 装服务
curl -fsSL https://raw.githubusercontent.com/goojia/muse-bridge/main/install.sh \
  | sudo sh -s -- --domain bridge.example.com --email you@example.com

# 2. 从源码编译安装
git clone https://github.com/goojia/muse-bridge.git && cd muse-bridge
sudo sh install.sh --from-source --domain bridge.example.com

# 3. 手动：编译后按下面的「目录与文件」自己放
cd bridge && GOOS=linux GOARCH=amd64 bash scripts/build.sh
```

一键安装默认装到 `/opt/muse-bridge`，用 `--prefix` 可以改。
它幂等：重复执行不会覆盖配置、不会重新生成主密钥。

卸载：

```bash
sudo sh install.sh --uninstall           # 保留数据和配置
sudo sh install.sh --uninstall --purge   # 连数据一起删
```

## 目录与文件

| 路径 | 内容 |
|---|---|
| `/data/bridge/bridge` | 二进制（14MB，静态链接） |
| `/data/bridge-data/bridge.db` | SQLite：桥、令牌、会话、消息、任务、会话 |
| `/data/bridge-data/certs/` | autocert 证书缓存，续期靠它，删了会重新申请 |
| `/etc/muse-bridge.env` | 全部配置，`chmod 600`。含主密钥和后台密码 |
| `/etc/init.d/muse-bridge` | OpenRC 服务定义 |
| `/data/log/muse-bridge.log` | 标准输出（启动、巡检、错误） |

## 常用命令

```bash
rc-service muse-bridge status
rc-service muse-bridge restart
tail -f /data/log/muse-bridge.log
curl -s https://<域名>/health
```

## systemd（Debian / Ubuntu）

上面的路径只是约定，换一个目录、换一个服务管理器同样能跑。Debian / Ubuntu 上整套装在一个目录里最省事，下面以 `/opt/muse-bridge` 为例：

| 路径 | 内容 |
|---|---|
| `/opt/muse-bridge/bridge` | 二进制 |
| `/opt/muse-bridge/bridge-data/` | `bridge.db` + `certs/`，`chmod 700` |
| `/opt/muse-bridge/muse-bridge.env` | 配置，`chmod 600` |
| `/opt/muse-bridge/log/` | `muse-bridge.log` / `muse-bridge.err` |

```ini
# /etc/systemd/system/muse-bridge.service
[Unit]
Description=Muse Bridge (Telegram <-> Muse)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/opt/muse-bridge/muse-bridge.env
ExecStart=/opt/muse-bridge/bridge
Restart=always
RestartSec=5
LimitNOFILE=65535
StandardOutput=append:/opt/muse-bridge/log/muse-bridge.log
StandardError=append:/opt/muse-bridge/log/muse-bridge.err

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now muse-bridge
systemctl status muse-bridge --no-pager
journalctl -u muse-bridge -n 50 --no-pager   # 日志写的是文件，这个只用于排查启动失败
```

env 里的 `BRIDGE_DATA_DIR` 要跟着改成 `/opt/muse-bridge/bridge-data`。迁移整机时把 `bridge.db`（连 `-wal` 一起）、
`certs/`、env 三样拷过去即可：`certs/` 带着就不用重新申请证书，`BRIDGE_SECRET` 不变则已加密的 bot token 仍能解开。
**先停旧机的服务再拷库**，别在运行时复制 `.db`。

日志目前不轮转。单条日志都很短，但长期跑建议加一个 cron：

```bash
# /etc/periodic/daily/muse-bridge-logrotate
#!/bin/sh
[ -f /data/log/muse-bridge.log ] || exit 0
tail -n 5000 /data/log/muse-bridge.log > /data/log/muse-bridge.log.tmp
mv /data/log/muse-bridge.log.tmp /data/log/muse-bridge.log
rc-service muse-bridge restart
```

## 访问管理后台

后台就是域名首页 `https://<域名>/`，直接浏览器访问并登录。

```bash
https://bridge.example.com/
```

首次访问首页会引导创建管理员账号（`/setup`），建完直接进后台；没有默认账号密码。
之后在右上角「账号」页可以改用户名和密码。部署完请立刻打开创建，别把未初始化的实例晾在公网。

后台对公网开放的三道防线：

- 会话 cookie 校验（`HttpOnly` + `Secure` + `SameSite=Lax`，12 小时过期）
- 登录失败限流：同一 IP 5 分钟内失败 10 次即锁定，按纯 IP 计数（不是 RemoteAddr，避免换端口绕过）
- 严格 CSP（后台页面不含任何 JS，`script-src` 等同于禁用）+ `X-Frame-Options: DENY`

`127.0.0.1:8080` 的监听仍然保留，作为 TLS 异常时的兜底入口，可用 SSH 隧道访问。

改密码：后台「账号」页，填当前密码 + 新密码保存，所有登录立即失效，用新密码重登。

**忘记密码**：在 env 里写上 `BRIDGE_ADMIN_USER` / `BRIDGE_ADMIN_PASSWORD`，再加一行 `BRIDGE_ADMIN_RESET=1`，重启；
启动日志会说明这次是用 env 覆盖。登进去后把 `BRIDGE_ADMIN_RESET` 删掉，否则每次重启都会重置。

## 服务器部署约定

生产服务器（`bridge.cjjia.com`）跑的是**发布版二进制**，装在一键安装脚本的默认位置：

| 路径 | 内容 |
|---|---|
| `/opt/muse-bridge/bridge` | 二进制（来自 GitHub Releases，不在服务器上编译） |
| `/opt/muse-bridge/bridge-data/` | 数据库、证书、归档 |
| `/opt/muse-bridge/muse-bridge.env` | 配置（含主密钥） |
| `/opt/muse-bridge/log/` | 日志 |

**改功能的流程**：本地改 → 推 GitHub → 打 tag 触发 Actions 发版 → 后台「升级」页点一下。
服务器上不装 Go、不装 git、不编译。详见 README 的「修改功能的推荐流程」。

## 升级

两条路，日常用第一条。

### 后台一键升级（推荐）

登录后台 →「升级」→「检查更新」→ 勾选确认 →「立即升级」。

程序自己完成：查 GitHub Releases → 下载当前平台的 `bridge-<os>-<arch>` →
用 `checksums.txt` 校验 sha256 → 备份旧文件为 `bridge.bak` → rename 原子替换 →
`syscall.Exec` 换成新二进制。**PID 不变**，systemd / OpenRC / nohup 起的都不用改配置。

也可以在同一个页面打开「定时升级」，设成每天凌晨自动升级。

几点运维须知：

- 升级会重启进程，正在处理的回复会被打断（消息不会丢，租约过期后自动回到队列）。
- 失败不动旧版本：下载、校验、替换任何一步出错都继续跑旧版本，页面显示原因。
- 升级后如果日志里出现「上次升级未生效」，说明 exec 后版本没变，
  可以点页面上的「回滚到上一个版本」用 `bridge.bak` 恢复。
- 二进制目录必须对服务账号可写（`/data/bridge`、`/opt/muse-bridge` 通常没问题；
  装在 `/usr/local/bin` 且用非 root 跑的话会报「目录不可写」）。
- 想彻底关掉：env 里加 `BRIDGE_UPGRADE_DISABLED=1`，后台连入口都不显示。
- 仓库还没发过 Release（或当前平台没出包）时，检查会提示"没有找到适合当前平台的发布"。

发版由 `.github/workflows/release.yml` 负责：`git tag v1.2.3 && git push origin v1.2.3`，
Actions 跑完测试后交叉编译四个平台并连同 `checksums.txt` 发到 Release。

### 手动替换

```bash
# 本地
cd bridge && GOOS=linux GOARCH=amd64 bash scripts/build.sh
# 上传覆盖
scp -P 22022 dist/bridge-linux-amd64 root@<服务器>:/data/bridge/bridge
# 服务器
chmod +x /data/bridge/bridge && rc-service muse-bridge restart
```

数据库迁移是自动的：启动时执行 `CREATE TABLE IF NOT EXISTS` 和缺失列的补齐。
升级前建议先备份（见下）。

### 回滚

后台升级过的，`bridge.bak` 就是上一个版本：

```bash
# 方式一：后台「升级」页点「回滚到上一个版本」，然后重启服务
# 方式二：手动
mv /data/bridge/bridge.bak /data/bridge/bridge && rc-service muse-bridge restart
```

## 归档与在线备份

后台「备份」页可以做三件事：按天归档、上传到 S3/WebDAV、按保留天数清理本地。

### 归档文件

每天一个 `YYYY-MM-DD.jsonl.gz`，放在 `BRIDGE_ARCHIVE_DIR`（默认 `<数据目录>/archive`）。
JSON Lines 加 gzip，可以直接看：

```bash
gzip -dc /data/bridge-data/archive/2026-11-22.jsonl.gz | head -3
gzip -dc /data/bridge-data/archive/2026-11-22.jsonl.gz | jq -r '.text' | head
```

归档只是生成文件，**不会删数据库里的消息**。删不删本地由保留策略决定。

### 配置备份目标

S3 兼容存储填 Endpoint / Region / Bucket / Access Key / Secret Key；
MinIO 这类自建服务通常还要勾 path-style。WebDAV 填目录地址和账号密码，
目录必须先在网盘里建好（程序不会替你在别人网盘里建目录）。

凭据用 `BRIDGE_SECRET` 加密后存库，界面不回显；表单里留空表示不修改。

### 保留天数与自动清理

勾上「自动清理本地聊天记录」并填天数，更早的消息会被删除。

**只有当天已成功上传到远端才会删。** 上传失败的日子一律保留，
这是硬性保护——否则一次网络抖动就是永久丢数据。
也因此：没启用在线备份时，程序不允许开自动清理。

清理只删 `messages` 和 `jobs`，不删 `conversations`：
会话本身是轻量元信息，删了会让后台会话列表整段消失，
用户会以为"聊天记录凭空没了"。

想彻底关掉归档和备份：env 里加 `BRIDGE_BACKUP_DISABLED=1`。

### 恢复

归档文件是纯文本流，恢复不依赖程序：

```bash
gzip -dc 2026-11-22.jsonl.gz | jq -r '[.time, .direction, .bridge, .sender, .text] | @tsv'
```

需要重新入库时，按 `.time` 换回 unix 秒写进 `messages` 表即可
（注意 `bridge_id` / `conversation_id` 要对应上）。

## 数据库备份

按天归档只覆盖聊天记录，**桥配置、令牌、账号不在归档里**。
整机迁移或灾备仍需要备份数据库本身。

数据库是单文件，WAL 模式下直接拷 `.db` 可能拿到不一致的快照，用 SQLite 的备份语法：

```bash
sqlite3 /data/bridge-data/bridge.db ".backup '/data/backup/bridge-$(date +%F).db'"
```

没有 `sqlite3` 命令时，停服务后拷 `bridge.db`、`bridge.db-wal`、`bridge.db-shm` 三个文件也可以。

主密钥不在数据库里。**`/etc/muse-bridge.env` 必须单独备份**，否则换了机器所有机器人 token 都得重新填。

## 排障

**webhook 一直收不到消息**

```bash
curl "https://api.telegram.org/bot<token>/getWebhookInfo"
```

看 `last_error_message`。常见原因：

- `SSL error` / `Connection refused`：443 不通，或证书没签下来。检查安全组和 `certs/` 目录。
- `Wrong response from the webhook: 403`：`webhook_secret` 不匹配。在桥详情里把「接收方式」切到长轮询保存一次、再切回 webhook，桥会重新注册。
- `409 Conflict`：还挂着旧 webhook，或同时开了长轮询。后台切一次模式即可同步。

**证书申请失败**

`autocert` 需要 80 端口从公网可达（HTTP-01）。检查：`curl http://<域名>/.well-known/acme-challenge/test`。
如果 80 被挡，可以只保留 443 并改用 DNS-01（需要另配，当前未实现）。

**消息一直 pending，Muse 收不到**

1. `curl https://<域名>/pending-count` 看数字；
2. 数字大于 0 但 Muse 不动 → 检查 hook 是否在跑、`MUSE_BRIDGE_URL` 是否写对；
3. Muse 报 401 → 令牌被轮转或写错，在桥详情点「轮转令牌」把新的发给 Muse；
4. Muse 报 409 → 租约过期，说明它处理太慢，把 `--lease-seconds` 调大或加 `extend`。

**死信堆积**

后台「死信」里能看到 `last_error`。修好原因后点「重放」。如果是 Telegram 侧的永久错误
（bot 被踢、chat 不存在），重放也不会成功，直接在库里把对应桥的记录清理掉更合适。

## 磁盘

1GB 的盘要留意。粗算：1 万条消息约 6MB，10 万条约 60MB。

```bash
du -sh /data/bridge-data /data/log
```

需要清理历史时（先备份）：

```sql
-- 删除 90 天前的消息和对应任务
DELETE FROM jobs WHERE message_id IN (SELECT id FROM messages WHERE created_at < strftime('%s','now') - 90*86400);
DELETE FROM messages WHERE created_at < strftime('%s','now') - 90*86400;
VACUUM;
```

`VACUUM` 期间会占用约等于库大小的临时空间，盘紧的时候分批删。
