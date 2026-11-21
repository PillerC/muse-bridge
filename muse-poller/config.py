"""VM 侧 poller 配置: 全部来自环境变量 / .env, 不把密钥写进代码。"""
import os


def _get(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def _get_int(name: str, default: int) -> int:
    try:
        return int(os.environ.get(name, "") or default)
    except ValueError:
        return default


def _get_float(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, "") or default)
    except ValueError:
        return default


class Config:
    # --- Worker 队列 ---
    WORKER_URL = _get("WORKER_URL", "").rstrip("/")  # 如 https://telegram-queue.<your-subdomain>.workers.dev
    POLLER_TOKEN = _get("POLLER_TOKEN")
    POLL_INTERVAL = _get_float("POLL_INTERVAL", 5.0)  # 每 5 秒拉取一次
    POLL_BATCH_SIZE = _get_int("POLL_BATCH_SIZE", 5)
    LEASE_SECONDS = _get_int("LEASE_SECONDS", 180)
    MAX_CONCURRENT = _get_int("MAX_CONCURRENT", 3)  # 同时处理的消息数

    # --- Telegram ---
    BOT_TOKEN = _get("BOT_TOKEN")
    BOT_USERNAME = _get("BOT_USERNAME", "").lstrip("@").lower()
    BOT_ID = _get_int("BOT_ID", 0) or None  # 可选, 有它引用判定更准

    # --- Muse 后端: echo | http | cli ---
    MUSE_BACKEND = _get("MUSE_BACKEND", "echo").lower()
    # http 模式: 你 VM 上 Muse webhook 的地址 (SSE 流式)
    # 约定: POST {prompt, chat_id, user, message_id} -> SSE 事件:
    #   event: tool_call  data: {"name": "...", "detail": "..."}
    #   event: token     data: {"text": "..."}
    #   event: done      data: {"text": "全文 (可选)"}
    MUSE_WEBHOOK_URL = _get("MUSE_WEBHOOK_URL", "")
    MUSE_HTTP_TIMEOUT = _get_float("MUSE_HTTP_TIMEOUT", 300.0)
    # cli 模式: 调用的命令, 例如: muse --print --stream
    MUSE_CLI_CMD = _get("MUSE_CLI_CMD", "")
    MUSE_CLI_TIMEOUT = _get_float("MUSE_CLI_TIMEOUT", 300.0)

    # --- 流式渲染 ---
    EDIT_THROTTLE_SEC = _get_float("EDIT_THROTTLE_SEC", 1.2)  # Telegram edit 限速, 约 1 次/秒
    TYPING_INTERVAL_SEC = _get_float("TYPING_INTERVAL_SEC", 4.0)  # typing 有效期约 5 秒
    MAX_MESSAGE_LEN = 4096  # Telegram 单条上限

    # --- 其他 ---
    LOG_LEVEL = _get("LOG_LEVEL", "INFO")
    # 私聊是否也要求 @ /引用才回? 默认 False (私聊全回)。设为 True 则私聊也只回 @ /引用。
    PRIVATE_REQUIRES_TRIGGER = _get("PRIVATE_REQUIRES_TRIGGER", "false").lower() in ("1", "true", "yes")

    @classmethod
    def validate(cls) -> list[str]:
        errors = []
        if not cls.WORKER_URL:
            errors.append("WORKER_URL 未设置")
        if not cls.POLLER_TOKEN:
            errors.append("POLLER_TOKEN 未设置")
        if not cls.BOT_TOKEN:
            errors.append("BOT_TOKEN 未设置")
        if not cls.BOT_USERNAME:
            errors.append("BOT_USERNAME 未设置 (不带 @)")
        if cls.MUSE_BACKEND == "http" and not cls.MUSE_WEBHOOK_URL:
            errors.append("MUSE_BACKEND=http 但 MUSE_WEBHOOK_URL 未设置")
        if cls.MUSE_BACKEND == "cli" and not cls.MUSE_CLI_CMD:
            errors.append("MUSE_BACKEND=cli 但 MUSE_CLI_CMD 未设置")
        if cls.MUSE_BACKEND not in ("echo", "http", "cli"):
            errors.append(f"未知 MUSE_BACKEND: {cls.MUSE_BACKEND}")
        return errors
