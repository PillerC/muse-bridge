"""Telegram Bot API 客户端: typing、发送、节流编辑、流式渲染。

关键约束 (来自 Telegram 实际限速):
- sendChatAction typing 有效期约 5 秒, 需每 4 秒续发一次。
- editMessageText 约 1 次/秒/条消息, 过快会 429, 必须节流 + 遵守 retry_after。
- 文本内容不变时 edit 会报 "message is not modified", 属正常, 直接忽略。
- 单条消息上限 4096 字符, 超长要分段续发。
"""
import asyncio
import html
import logging
import time

import httpx

log = logging.getLogger(__name__)


class TelegramClient:
    def __init__(self, bot_token: str, timeout: float = 30.0):
        self._base = f"https://api.telegram.org/bot{bot_token}"
        self._client = httpx.AsyncClient(base_url=self._base, timeout=timeout)

    async def close(self):
        await self._client.aclose()

    async def _call(self, method: str, payload: dict, retries: int = 3) -> dict | None:
        """调 Bot API, 429 按 retry_after 退避重试, 返回 result 或 None。"""
        for attempt in range(retries):
            try:
                resp = await self._client.post(f"/{method}", json=payload)
                data = resp.json()
            except (httpx.HTTPError, ValueError) as e:
                log.warning("Telegram %s 网络异常 (%s), 重试 %d/%d", method, e, attempt + 1, retries)
                await asyncio.sleep(1.0 * (attempt + 1))
                continue
            if data.get("ok"):
                return data.get("result")
            err = data.get("description", "")
            params = data.get("parameters") or {}
            # 429 限速: 按官方要求等待
            if resp.status_code == 429 and "retry_after" in params:
                wait = float(params["retry_after"]) + 0.5
                log.warning("Telegram 429, 等 %.1fs 后重试 %s", wait, method)
                await asyncio.sleep(wait)
                continue
            # 编辑内容未变: 正常, 不算错
            if "message is not modified" in err:
                return None
            log.warning("Telegram %s 失败: %s", method, err)
            return None
        return None

    async def send_chat_action(self, chat_id: int, action: str = "typing") -> None:
        await self._call("sendChatAction", {"chat_id": chat_id, "action": action}, retries=1)

    async def send_message(self, chat_id: int, text: str, reply_to_message_id: int | None = None) -> dict | None:
        payload: dict = {"chat_id": chat_id, "text": text[:4096]}
        if reply_to_message_id:
            payload["reply_parameters"] = {"message_id": reply_to_message_id}
        return await self._call("sendMessage", payload)

    async def edit_message(self, chat_id: int, message_id: int, text: str) -> None:
        await self._call(
            "editMessageText",
            {"chat_id": chat_id, "message_id": message_id, "text": text[:4096]},
        )


class TypingLoop:
    """处理期间持续显示 "输入中…", 每 4 秒续发一次, 直到 stop。"""

    def __init__(self, tg: TelegramClient, chat_id: int, interval: float = 4.0):
        self._tg = tg
        self._chat_id = chat_id
        self._interval = interval
        self._task: asyncio.Task | None = None

    async def start(self):
        async def _loop():
            while True:
                try:
                    await self._tg.send_chat_action(self._chat_id, "typing")
                except Exception:
                    pass
                await asyncio.sleep(self._interval)

        self._task = asyncio.create_task(_loop())

    async def stop(self):
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None


class StreamingRenderer:
    """流式渲染器: 一条占位消息, 随 Muse 输出增量编辑。

    渲染格式 (纯文本, 不用 parse_mode, 避免 Markdown 特殊字符打断流式):
        🔧 工具调用
        • search: 查天气…
        • read: config.py
        ─────────────
        正文流式内容…▍

    - 工具链在顶部实时追加, 正文在下方流式增长, 光标 ▍ 表示还在输出。
    - edit 节流到 EDIT_THROTTLE_SEC, 避免 429。
    - 完成时去掉 ▍, 做最后一次编辑; 超长时余下内容分段续发。
    """

    def __init__(
        self,
        tg: TelegramClient,
        chat_id: int,
        reply_to_message_id: int | None,
        throttle_sec: float = 1.2,
        max_len: int = 4096,
    ):
        self._tg = tg
        self._chat_id = chat_id
        self._reply_to = reply_to_message_id
        self._throttle = throttle_sec
        self._max_len = max_len
        self._placeholder_id: int | None = None
        self._tools: list[str] = []
        self._body: str = ""
        self._last_edit = 0.0
        self._lock = asyncio.Lock()

    async def start(self, initial: str = "💭 思考中…") -> None:
        msg = await self._tg.send_message(self._chat_id, initial, self._reply_to)
        if msg:
            self._placeholder_id = msg["message_id"]

    def add_tool_call(self, name: str, detail: str = "") -> None:
        line = f"• {name}" + (f": {detail}" if detail else "")
        # 去重连续重复的工具行, 避免刷屏
        if not self._tools or self._tools[-1] != line:
            self._tools.append(line)

    def append_token(self, text: str) -> None:
        self._body += text

    def _render(self, streaming: bool) -> str:
        parts: list[str] = []
        if self._tools:
            parts.append("🔧 工具调用")
            parts.extend(self._tools[-8:])  # 只保留最近 8 条, 防头部占满
            parts.append("─" * 12)
        body = self._body or ("思考中…" if streaming else "")
        if streaming:
            body += "▍"
        parts.append(body)
        text = "\n".join(parts)
        # 预留空间给后续编辑, 占位消息控制在上限内
        return text[: self._max_len - 100]

    async def refresh(self, force: bool = False) -> None:
        if not self._placeholder_id:
            return
        async with self._lock:
            now = time.monotonic()
            if not force and now - self._last_edit < self._throttle:
                return
            self._last_edit = now
            await self._tg.edit_message(self._chat_id, self._placeholder_id, self._render(streaming=True))

    async def finish(self, final_text: str | None = None) -> None:
        """完成: 去掉光标做最终编辑, 超长部分分段续发。"""
        if final_text is not None:
            self._body = final_text
        if not self._placeholder_id:
            # 占位消息没发出来 (网络抖动), 直接发全文
            await self._send_long(self._render(streaming=False))
            return
        async with self._lock:
            full = self._render(streaming=False)
            await self._tg.edit_message(self._chat_id, self._placeholder_id, full)
        # 正文超出单条上限的部分, 分段续发
        overflow = self._overflow_text()
        if overflow:
            await self._send_long(overflow)

    def _overflow_text(self) -> str:
        # 估算: 工具头 + 正文总长超过上限时, 返回被截掉的正文尾部
        header_len = sum(len(t) + 1 for t in self._tools) + 30
        budget = self._max_len - 100 - header_len
        if len(self._body) > budget > 0:
            return self._body[budget:]
        return ""

    async def _send_long(self, text: str) -> None:
        for i in range(0, len(text), 4000):
            await self._tg.send_message(self._chat_id, text[i : i + 4000], None)

    async def fail(self, error: str = "处理失败, 请稍后重试") -> None:
        if self._placeholder_id:
            tools = "\n".join(self._tools[-5:])
            text = f"⚠️ {error}"
            if tools:
                text = f"🔧 工具调用\n{tools}\n{'─' * 12}\n{text}"
            await self._tg.edit_message(self._chat_id, self._placeholder_id, text[:4096])
        else:
            await self._tg.send_message(self._chat_id, f"⚠️ {error}", self._reply_to)


def escape_html(s: str) -> str:
    return html.escape(s)
