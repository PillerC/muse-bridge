"""Muse agent 适配层: 把三种后端统一成同一个流式接口。

接口约定:
    async def run(prompt, context, on_tool_call, on_token) -> str
    - on_tool_call(name, detail): Muse 每调一次工具就回调一次, 用于渲染工具链
    - on_token(text): 每产出一小段文本就回调一次, 用于流式输出
    - 返回值: 完整回答全文

后端 (MUSE_BACKEND):
- echo: 测试用, 模拟 2 次工具调用 + 逐字流式, 不调任何外部服务。
  先用它验证 Telegram typing / 工具链 / 流式渲染全链路, 再切真实后端。
- http: 调你 VM 上 Muse webhook (SSE 流式)。事件约定见 config.py。
  你的 Muse 服务只需按约定吐 SSE 事件, poller 不关心内部实现。
- cli: 调本地命令行 (如 `muse --print`), 把 stdout 逐行当 token 流,
  形如 `[tool:name:detail]` 的行当工具调用。适合已有 CLI 的场景。
"""
import asyncio
import json
import logging
from typing import Awaitable, Callable

import httpx

log = logging.getLogger(__name__)

ToolCallback = Callable[[str, str], Awaitable[None]]
TokenCallback = Callable[[str], Awaitable[None]]


async def run_echo(prompt: str, context: dict, on_tool_call: ToolCallback, on_token: TokenCallback) -> str:
    """测试后端: 不依赖 Muse, 验证 Telegram 端到端渲染。"""
    await on_tool_call("parse", f"收到来自 @{context.get('from_username') or 'user'} 的消息")
    await asyncio.sleep(0.6)
    await on_tool_call("demo_search", prompt[:30] or "空消息")
    await asyncio.sleep(0.6)
    reply = (
        f"你好! 我收到你的消息了: “{prompt[:200]}”\n\n"
        "这是 echo 测试模式, 说明 Worker 入队 → poller 拉取 → typing → "
        "工具链 → 流式输出全链路已通。\n"
        "把 MUSE_BACKEND 改成 http 或 cli 即可接真实 Muse。"
    )
    # 逐字流式, 模拟真实 token 流
    for ch in reply:
        await on_token(ch)
        await asyncio.sleep(0.015)
    return reply


async def run_http(
    webhook_url: str,
    prompt: str,
    context: dict,
    on_tool_call: ToolCallback,
    on_token: TokenCallback,
    timeout: float = 300.0,
) -> str:
    """http 后端: POST 到 Muse webhook, 读 SSE 事件流。

    接受两种 SSE 形态:
    1) 标准 SSE: event: tool_call / token / done + data: {...}
    2) 纯 JSON 行: 每行一个 {"type": "tool_call"|"token"|"done", ...}
    也兼容一次性返回 JSON {"text": "...", "tools": [...]} 的非流式服务。
    """
    full: list[str] = []
    async with httpx.AsyncClient(timeout=timeout) as client:
        async with client.stream(
            "POST",
            webhook_url,
            json={
                "prompt": prompt,
                "chat_id": context.get("chat_id"),
                "chat_type": context.get("chat_type"),
                "message_id": context.get("message_id"),
                "from_id": context.get("from_id"),
                "from_username": context.get("from_username"),
                "reply_to_message_id": context.get("reply_to_message_id"),
            },
        ) as resp:
            resp.raise_for_status()
            ctype = resp.headers.get("content-type", "")
            # 非流式 JSON: 一次性返回全文
            if "application/json" in ctype and "text/event-stream" not in ctype:
                body = await resp.aread()
                try:
                    data = json.loads(body.decode("utf-8", "replace"))
                except json.JSONDecodeError:
                    text = body.decode("utf-8", "replace")
                    full.append(text)
                    await on_token(text)
                    return text
                for t in data.get("tools") or []:
                    if isinstance(t, dict):
                        await on_tool_call(t.get("name", "tool"), t.get("detail", ""))
                    else:
                        await on_tool_call(str(t), "")
                text = data.get("text") or data.get("reply") or data.get("answer") or ""
                if text:
                    full.append(text)
                    await on_token(text)
                return "".join(full)

            # 流式: 逐行解析 SSE / JSONL
            current_event = ""
            async for raw_line in resp.aiter_lines():
                line = raw_line.strip()
                if not line:
                    continue
                if line.startswith("event:"):
                    current_event = line[6:].strip()
                    continue
                if line.startswith("data:"):
                    payload = line[5:].strip()
                elif line.startswith("{"):
                    payload = line
                else:
                    continue
                if payload == "[DONE]":
                    break
                try:
                    data = json.loads(payload)
                except json.JSONDecodeError:
                    # data 不是 JSON 时当纯文本 token
                    full.append(payload)
                    await on_token(payload)
                    continue
                etype = data.get("type") or current_event
                if etype in ("tool_call", "tool"):
                    await on_tool_call(
                        data.get("name") or data.get("tool") or "tool",
                        data.get("detail") or data.get("args") or data.get("input") or "",
                    )
                elif etype in ("token", "text", "delta", "chunk"):
                    piece = data.get("text") or data.get("delta") or data.get("content") or ""
                    if piece:
                        full.append(piece)
                        await on_token(piece)
                elif etype in ("done", "end", "complete"):
                    if data.get("text") and not full:
                        full.append(data["text"])
                        await on_token(data["text"])
                    break
                current_event = ""
    return "".join(full)


async def run_cli(
    cmd: str,
    prompt: str,
    context: dict,
    on_tool_call: ToolCallback,
    on_token: TokenCallback,
    timeout: float = 300.0,
) -> str:
    """cli 后端: 子进程调本地 Muse CLI, stdout 逐行流式。

    约定: CLI 输出中形如 `[tool:名字:详情]` 的行视为工具调用,
    其余行视为正文 token。prompt 通过 stdin 传入, 避免命令行泄露与转义问题。
    """
    import shlex

    argv = shlex.split(cmd)
    if not argv:
        raise ValueError("MUSE_CLI_CMD 为空")
    proc = await asyncio.create_subprocess_exec(
        *argv,
        stdin=asyncio.subprocess.PIPE,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    assert proc.stdin and proc.stdout
    proc.stdin.write(prompt.encode("utf-8"))
    await proc.stdin.drain()
    proc.stdin.close()

    full: list[str] = []

    async def _drain_stderr():
        assert proc.stderr
        err = await proc.stderr.read()
        if err:
            log.debug("Muse CLI stderr: %s", err.decode("utf-8", "replace")[:500])

    err_task = asyncio.create_task(_drain_stderr())
    try:
        async with asyncio.timeout(timeout):
            while True:
                line = await proc.stdout.readline()
                if not line:
                    break
                s = line.decode("utf-8", "replace").rstrip("\n")
                if s.startswith("[tool:") and s.endswith("]"):
                    inner = s[6:-1]
                    name, _, detail = inner.partition(":")
                    await on_tool_call(name.strip() or "tool", detail.strip())
                elif s:
                    full.append(s + "\n")
                    await on_token(s + "\n")
    finally:
        err_task.cancel()
        try:
            await proc.wait()
        except Exception:
            proc.kill()
    if proc.returncode not in (0, None):
        raise RuntimeError(f"Muse CLI 退出码 {proc.returncode}")
    return "".join(full).rstrip("\n")


def build_runner(config):
    """按配置返回统一签名的 runner(prompt, context, on_tool_call, on_token)。"""
    backend = config.MUSE_BACKEND

    if backend == "echo":

        async def _echo(prompt, context, on_tool_call, on_token):
            return await run_echo(prompt, context, on_tool_call, on_token)

        return _echo

    if backend == "http":

        async def _http(prompt, context, on_tool_call, on_token):
            return await run_http(
                config.MUSE_WEBHOOK_URL, prompt, context,
                on_tool_call, on_token, timeout=config.MUSE_HTTP_TIMEOUT,
            )

        return _http

    if backend == "cli":

        async def _cli(prompt, context, on_tool_call, on_token):
            return await run_cli(
                config.MUSE_CLI_CMD, prompt, context,
                on_tool_call, on_token, timeout=config.MUSE_CLI_TIMEOUT,
            )

        return _cli

    raise ValueError(f"未知 MUSE_BACKEND: {backend}")
