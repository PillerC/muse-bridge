#!/usr/bin/env python3
"""Telegram Bot API helper for the custom.telegram-bot-api connector.

Calls https://api.telegram.org with the registered BOT_TOKEN credential.
The token never appears here: dynamic_credentials.py fetches an hsurr:
surrogate from authd, and Muse swaps in the real token on approved egress
to the registered host. The token travels in the URL path (/bot<token>/...),
so the URL is built with url_with_raw_surrogate below.

Deviation from the bundled helper, verified 2026-11-21: the library's
url_with_surrogate_path_segment percent-encodes the surrogate
(hsurr%3A...), and Sentinel then does not recognize it, so the request
reaches Telegram with a malformed token (404 Not Found). Inserting the
surrogate raw (hsurr:...) swaps correctly and getMe returns 200. The
observed surrogate alphabet is URL path-safe, so no encoding is needed.

Usage:
  python3 telegram-bot.py get-me
  python3 telegram-bot.py send-chat-action --chat-id <id> [--action typing]
  python3 telegram-bot.py send-message --chat-id <id> --text "..." [--reply-to <msg_id>]
  python3 telegram-bot.py send-message --chat-id <id> --text-file /tmp/reply.txt
  python3 telegram-bot.py edit-message --chat-id <id> --message-id <id> --text "..."
  python3 telegram-bot.py edit-message --chat-id <id> --message-id <id> --text-file /tmp/reply.txt

--text-file reads the message text from a file instead of the command line.
Use it for long replies so shell quoting cannot mangle the text.

Prints the provider JSON response. Exit code 0 when ok=true, 1 when the
provider returned ok=false, 2 on credential/helper errors.
"""
import argparse
import json
import re
import sys
import urllib.error
import urllib.request

sys.path.insert(0, "/opt/muse/skills/custom-connector-setup/bin")
from dynamic_credentials import (  # noqa: E402
    DynamicCredentialError,
    dynamic_credential_entry,
    ensure_allowed_url,
    read_json_response,
    url_with_surrogate_path_segment,
)

CREDENTIAL = "custom.telegram-bot-api"
HOSTS = ["api.telegram.org"]
BASE_TEMPLATE = "https://api.telegram.org/bot{}/"
USER_AGENT = "telegram-muse-bridge/1.0"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        raise RuntimeError("redirect refused, verify the endpoint")


_PATH_SAFE = re.compile(r"^[A-Za-z0-9:._-]+$")


def url_with_raw_surrogate(url_template):
    """Insert the surrogate into the URL path without percent-encoding.

    See the module docstring for why the library's encoding builder is
    not used. Falls back to it if a future surrogate ever contains
    characters outside the path-safe alphabet.
    """
    ensure_allowed_url(url_template, HOSTS)
    entry = dynamic_credential_entry(CREDENTIAL)
    placement = entry.get("placement")
    if not (isinstance(placement, dict) and "url_path_segment" in placement):
        raise DynamicCredentialError(
            f"credential is not url_path_segment: {placement!r}"
        )
    surrogate = str(entry["surrogate"]).strip()
    if _PATH_SAFE.match(surrogate):
        return url_template.replace("{}", surrogate, 1)
    return url_with_surrogate_path_segment(
        url_template, CREDENTIAL, allowed_hosts=HOSTS
    )


def call(method, payload=None):
    """Call one Bot API method. payload None means GET, otherwise POST JSON."""
    url = url_with_raw_surrogate(BASE_TEMPLATE + method)
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
    req.add_header("User-Agent", USER_AGENT)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler(), NoRedirect())
    try:
        with opener.open(req, timeout=30) as resp:
            return read_json_response(resp)
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            return {"ok": False, "error_code": e.code, "description": f"HTTP {e.code}"}


def message_text(args):
    """Read the message text from --text or --text-file, whichever is given."""
    if getattr(args, "text_file", None):
        with open(args.text_file, encoding="utf-8") as f:
            return f.read()
    if args.text is not None:
        return args.text
    raise SystemExit("need --text or --text-file")


def main():
    parser = argparse.ArgumentParser(description="Telegram Bot API helper")
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("get-me")
    a = sub.add_parser("send-chat-action")
    a.add_argument("--chat-id", required=True)
    a.add_argument("--action", default="typing")
    s = sub.add_parser("send-message")
    s.add_argument("--chat-id", required=True)
    s.add_argument("--text")
    s.add_argument("--text-file")
    s.add_argument("--reply-to", type=int, default=None)
    e = sub.add_parser("edit-message")
    e.add_argument("--chat-id", required=True)
    e.add_argument("--message-id", required=True, type=int)
    e.add_argument("--text")
    e.add_argument("--text-file")
    args = parser.parse_args()
    try:
        if args.cmd == "get-me":
            result = call("getMe")
        elif args.cmd == "send-chat-action":
            result = call(
                "sendChatAction",
                {"chat_id": int(args.chat_id), "action": args.action},
            )
        elif args.cmd == "send-message":
            payload = {"chat_id": int(args.chat_id), "text": message_text(args)}
            if args.reply_to:
                payload["reply_parameters"] = {"message_id": args.reply_to}
            result = call("sendMessage", payload)
        elif args.cmd == "edit-message":
            result = call(
                "editMessageText",
                {
                    "chat_id": int(args.chat_id),
                    "message_id": args.message_id,
                    "text": message_text(args),
                },
            )
        print(json.dumps(result, ensure_ascii=False))
        return 0 if result.get("ok") else 1
    except DynamicCredentialError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
