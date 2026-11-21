"""VM 侧 poller: 每 5 秒从 Worker /poll 拉取新消息, 调 Muse 处理并回 Telegram。

运行: python3 poller.py (需先配好 .env, 见 .env.example)
部署: 用 systemd 托管, 见 muse-telegram-poller.service

主循环:
  1. GET /poll 认领一批 pending 消息 (带 lease_id)
  2. 每条消息二次过滤 (Worker 已过滤一次, 这里防脏数据/配置漂移)
  3. 发 typing 循环 + 占位消息, 调 Muse 流式生成
     - 工具调用 -> 渲染器顶部追加工具链, 节流编辑
     - token -> 追加正文, 节流编辑
  4. 成功 POST /ack, 失败 POST /fail (Worker 退避重试, 超次进 dead)
  5. 长任务每 60 秒 POST /extend 续租

并发: 信号量限制同时处理数; 同一 chat 串行 (per-chat 锁), 保序且避免
同一群里多条回复交错编辑。
"""
import asyncio
import logging
import signal
import time
from collections import defaultdict

import httpx

from config import Config
from muse_agent import build_runner
from telegram_client import StreamingRenderer, TelegramClient, TypingLoop

logging.basicConfig(
    level=getattr(logging, Config.LOG_LEVEL.upper(), logging.INFO),
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
# 压掉 httpx 每次轮询的 INFO 日志, 只留 poller 自己的
logging.getLogger("httpx").setLevel(logging.WARNING)
logging.getLogger("httpcore").setLevel(logging.WARNING)
log = logging.getLogger("poller")

# ---------------------------------------------------------------- 过滤
# 与 worker/src/index.js 的 classifyMessage 保持一致, 两侧各算一次。


def should_reply(msg: dict, bot_username: str, bot_id: int | None, private_requires_trigger: bool = False) -> bool:
    """群里只回 @ 和引用; 私聊默认全回。"""
    chat_type = msg.get("chat_type", "")
    # Worker 已算好的字段优先用, 缺失时按文本兜底
    is_mention = bool(msg.get("is_mention"))
    is_reply = bool(msg.get("is_reply_to_bot"))
    if not is_mention and bot_username:
        text = (msg.get("text") or "").lower()
        if f"@{bot_username.lower()}" in text:
            is_mention = True
    if chat_type == "private":
        return (is_mention or is_reply) if private_requires_trigger else True
    if chat_type in ("group", "supergroup"):
        return is_mention or is_reply
    return False


# ---------------------------------------------------------------- Worker 队列客户端


class QueueClient:
    def __init__(self, worker_url: str, token: str):
        self._client = httpx.AsyncClient(
            base_url=worker_url, headers={"Authorization": f"Bearer {token}"}, timeout=30.0
        )

    async def close(self):
        await self._client.aclose()

    async def poll(self, limit: int, lease_seconds: int) -> tuple[list[dict], str | None]:
        resp = await self._client.get("/poll", params={"limit": limit, "lease_seconds": lease_seconds})
        resp.raise_for_status()
        data = resp.json()
        return data.get("messages", []), data.get("lease_id")

    async def ack(self, ids: list[int], lease_id: str) -> None:
        if not ids:
            return
        resp = await self._client.post("/ack", json={"ids": ids, "lease_id": lease_id})
        resp.raise_for_status()

    async def fail(self, msg_id: int, lease_id: str, error: str) -> None:
        resp = await self._client.post(
            "/fail", json={"id": msg_id, "lease_id": lease_id, "error": error[:500]}
        )
        resp.raise_for_status()

    async def extend(self, msg_id: int, lease_id: str, lease_seconds: int) -> bool:
        try:
            resp = await self._client.post(
                "/extend",
                json={"id": msg_id, "lease_id": lease_id, "lease_seconds": lease_seconds},
            )
            return resp.status_code == 200
        except Exception as e:
            log.warning("续租失败 id=%s: %s", msg_id, e)
            return False


# ---------------------------------------------------------------- 处理


class Poller:
    def __init__(self, config: type[Config]):
        self.cfg = config
        self.queue = QueueClient(config.WORKER_URL, config.POLLER_TOKEN)
        self.tg = TelegramClient(config.BOT_TOKEN)
        self.runner = build_runner(config)
        self.sem = asyncio.Semaphore(config.MAX_CONCURRENT)
        self.chat_locks: dict[int, asyncio.Lock] = defaultdict(asyncio.Lock)
        self.running = True
        self._stats = {"polled": 0, "replied": 0, "skipped": 0, "failed": 0}

    async def close(self):
        await self.queue.close()
        await self.tg.close()

    def _strip_mention(self, text: str) -> str:
        """去掉正文里的 @Bot, 再把多余空白压掉, 让 Muse 拿到干净的 prompt。"""
        if self.cfg.BOT_USERNAME and text:
            text = text.replace(f"@{self.cfg.BOT_USERNAME}", "").strip()
        return text or "你好"

    async def handle_one(self, msg: dict, lease_id: str) -> None:
        msg_id = msg["id"]
        chat_id = msg["chat_id"]
        # 同一 chat 串行, 避免回复乱序
        async with self.chat_locks[chat_id]:
            await self._handle_one_inner(msg, lease_id)

    async def _handle_one_inner(self, msg: dict, lease_id: str) -> None:
        msg_id = msg["id"]
        chat_id = msg["chat_id"]
        # 二次过滤: 万一 Worker 配置漂移或脏数据, 这里兜底, 直接 ack 不回复
        if not should_reply(
            msg, self.cfg.BOT_USERNAME, self.cfg.BOT_ID, self.cfg.PRIVATE_REQUIRES_TRIGGER
        ):
            log.info("跳过无触发消息 id=%s chat=%s", msg_id, chat_id)
            self._stats["skipped"] += 1
            await self.queue.ack([msg_id], lease_id)
            return

        prompt = self._strip_mention(msg.get("text") or "")
        context = {
            "chat_id": chat_id,
            "chat_type": msg.get("chat_type"),
            "message_id": msg.get("message_id"),
            "from_id": msg.get("from_id"),
            "from_username": msg.get("from_username"),
            "reply_to_message_id": msg.get("reply_to_message_id"),
        }
        log.info("处理 id=%s chat=%s from=@%s: %s", msg_id, chat_id, msg.get("from_username"), prompt[:80])

        typing = TypingLoop(self.tg, chat_id, interval=self.cfg.TYPING_INTERVAL_SEC)
        renderer = StreamingRenderer(
            self.tg,
            chat_id,
            reply_to_message_id=msg.get("message_id"),
            throttle_sec=self.cfg.EDIT_THROTTLE_SEC,
            max_len=self.cfg.MAX_MESSAGE_LEN,
        )
        # 续租任务: 长回答每 60 秒续一次, 防租约过期被重复认领
        stop_extend = asyncio.Event()

        async def _extend_loop():
            while not stop_extend.is_set():
                await asyncio.sleep(60)
                if stop_extend.is_set():
                    break
                ok = await self.queue.extend(msg_id, lease_id, self.cfg.LEASE_SECONDS)
                if not ok:
                    log.warning("续租失败 id=%s, 租约可能已丢, 继续处理但 ack 可能失效", msg_id)

        extend_task = asyncio.create_task(_extend_loop())
        await typing.start()
        try:
            await renderer.start()
            # 流式回调: 工具调用和 token 都触发节流刷新
            async def on_tool(name: str, detail: str):
                renderer.add_tool_call(name, detail)
                await renderer.refresh()

            async def on_token(piece: str):
                renderer.append_token(piece)
                await renderer.refresh()

            started = time.monotonic()
            full = await self.runner(prompt, context, on_tool, on_token)
            if not (full or "").strip():
                full = "我处理完了, 但没有生成出内容, 请再试一次。"
            await renderer.finish(full)
            log.info("回复完成 id=%s 耗时 %.1fs 字数 %d", msg_id, time.monotonic() - started, len(full))
            self._stats["replied"] += 1
            await self.queue.ack([msg_id], lease_id)
        except Exception as e:
            log.exception("处理失败 id=%s: %s", msg_id, e)
            self._stats["failed"] += 1
            try:
                await renderer.fail("处理失败了, 稍后会自动重试一次")
            except Exception:
                pass
            try:
                await self.queue.fail(msg_id, lease_id, str(e))
            except Exception as e2:
                log.warning("上报 fail 也失败 id=%s: %s (租约过期后会自动释放重试)", msg_id, e2)
        finally:
            stop_extend.set()
            extend_task.cancel()
            try:
                await extend_task
            except asyncio.CancelledError:
                pass
            await typing.stop()

    async def run_forever(self) -> None:
        log.info(
            "poller 启动: worker=%s interval=%.0fs batch=%d backend=%s",
            self.cfg.WORKER_URL, self.cfg.POLL_INTERVAL, self.cfg.POLL_BATCH_SIZE, self.cfg.MUSE_BACKEND,
        )
        while self.running:
            try:
                messages, lease_id = await self.queue.poll(self.cfg.POLL_BATCH_SIZE, self.cfg.LEASE_SECONDS)
                if messages:
                    log.info("拉到 %d 条新消息", len(messages))
                    self._stats["polled"] += len(messages)
                    async with self.sem:
                        # 批内并发受 sem + per-chat 锁双重约束
                        await asyncio.gather(
                            *(self.handle_one(m, lease_id) for m in messages),
                            return_exceptions=True,
                        )
            except httpx.HTTPError as e:
                log.warning("拉取失败 (下个周期重试): %s", e)
            except Exception:
                log.exception("主循环异常 (下个周期重试)")
            # 固定间隔, 用 sleep 而非定时器漂移累积; 收到停止信号时尽快退出
            for _ in range(int(self.cfg.POLL_INTERVAL * 10)):
                if not self.running:
                    break
                await asyncio.sleep(0.1)
        log.info("poller 退出, 统计: %s", self._stats)


async def main():
    errors = Config.validate()
    if errors:
        for e in errors:
            log.error("配置错误: %s", e)
        log.error("请复制 .env.example 为 .env 并填好, 再启动")
        raise SystemExit(1)
    poller = Poller(Config)
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, lambda: setattr(poller, "running", False))
        except NotImplementedError:
            pass
    try:
        await poller.run_forever()
    finally:
        await poller.close()


if __name__ == "__main__":
    asyncio.run(main())
