# 运维手册

面向已经跑起来的部署。首次部署看 README 的「快速开始」。

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

账号密码在 `/etc/muse-bridge.env`（`BRIDGE_ADMIN_USER` / `BRIDGE_ADMIN_PASSWORD`）。

后台对公网开放的三道防线：

- 会话 cookie 校验（`HttpOnly` + `Secure` + `SameSite=Lax`，12 小时过期）
- 登录失败限流：同一 IP 5 分钟内失败 10 次即锁定，按纯 IP 计数（不是 RemoteAddr，避免换端口绕过）
- 严格 CSP（后台页面不含任何 JS，`script-src` 等同于禁用）+ `X-Frame-Options: DENY`

`127.0.0.1:8080` 的监听仍然保留，作为 TLS 异常时的兜底入口，可用 SSH 隧道访问。

改密码：改 `BRIDGE_ADMIN_PASSWORD` 后 `rc-service muse-bridge restart`。
密码以环境变量为准，重启时会同步到数据库。

## 升级

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

## 备份

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
- `Wrong response from the webhook: 403`：`webhook_secret` 不匹配。在后台点「重新注册 webhook」。
- `409 Conflict`：还挂着旧 webhook，或同时开了长轮询。后台切一次模式即可同步。

**证书申请失败**

`autocert` 需要 80 端口从公网可达（HTTP-01）。检查：`curl http://<域名>/.well-known/acme-challenge/test`。
如果 80 被挡，可以只保留 443 并改用 DNS-01（需要另配，当前未实现）。

**消息一直 pending，Muse 收不到**

1. `curl https://<域名>/pending-count` 看数字；
2. 数字大于 0 但 Muse 不动 → 检查 hook 是否在跑、`MUSE_BRIDGE_URL` 是否写对；
3. Muse 报 401 → 令牌被吊销或写错，后台重新签发；
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
