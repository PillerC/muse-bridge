#!/usr/bin/env python3
"""Muse Bridge helper.

与 v1 的 custom.telegram-queue 保持同样的命令和响应格式，只是 host 换成自己的桥。
区别只有一处：回复不再直连 api.telegram.org（Muse 拿不到 bot token），
而是调用桥的 /reply，由桥负责分段、typing 保活、429 退避和落库。

Usage:
  python3 muse-bridge.py stats
  python3 muse-bridge.py pending-count
  python3 muse-bridge.py poll [--limit 5] [--lease-seconds 180]
  python3 muse-bridge.py reply --id 1 --lease-id <id> --text "..." [--mode draft|final]
  python3 muse-bridge.py reply --id 1 --lease-id <id> --text-file reply.txt
  python3 muse-bridge.py ack --ids 1,2 --lease-id <id>
  python3 muse-bridge.py fail --id 1 --lease-id <id> --error "..."
  python3 muse-bridge.py extend --id 1 --lease-id <id> [--lease-seconds 180]

环境变量：
  MUSE_BRIDGE_URL    例如 https://bridge.example.com（或写进下面的 BASE）
  MUSE_BRIDGE_TOKEN  桥在后台签发的令牌；未设置时走 dynamic_credentials 代理

Exit code: 0 成功，1 桥返回 ok=false，2 凭证/参数错误。
"""
import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("MUSE_BRIDGE_URL", "https://bridge.example.com").rstrip("/")
HOSTS = [urllib.parse.urlparse(BASE).hostname or "bridge.example.com"]
USER_AGENT = "telegram-muse-bridge/2.0"
DIRECT_TOKEN = os.environ.get("MUSE_BRIDGE_TOKEN", "")

try:  # Muse 环境里有这个模块；本地调试没有也能跑。
    sys.path.insert(0, "/opt/muse/skills/custom-connector-setup/bin")
    from dynamic_credentials import (  # type: ignore
        DynamicCredentialError,
        add_surrogate_to_request,
        read_json_response,
    )
except Exception:  # pragma: no cover
    DynamicCredentialError = Exception

    def add_surrogate_to_request(req, credential, allowed_hosts):
        if DIRECT_TOKEN:
            req.add_header("Authorization", "Bearer " + DIRECT_TOKEN)
        else:
            raise RuntimeError("没有可用凭证：请设置 MUSE_BRIDGE_TOKEN 或在 Muse 中注册连接")

    def read_json_response(resp):
        return json.loads(resp.read().decode("utf-8", "replace"))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args):
        raise RuntimeError("redirect refused, verify the endpoint")


def call(method, path, payload=None, params=None):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("User-Agent", USER_AGENT)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    add_surrogate_to_request(req, "custom.muse-bridge", allowed_hosts=HOSTS)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler(), NoRedirect())
    try:
        with opener.open(req, timeout=60) as resp:
            return read_json_response(resp)
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            return {"ok": False, "error": f"HTTP {e.code}"}


def main():
    parser = argparse.ArgumentParser(description="Muse Bridge helper")
    sub = parser.add_subparsers(dest="cmd", required=True)

    sub.add_parser("stats")
    sub.add_parser("pending-count")

    p = sub.add_parser("poll")
    p.add_argument("--limit", type=int, default=5)
    p.add_argument("--lease-seconds", type=int, default=180)

    r = sub.add_parser("reply")
    r.add_argument("--id", required=True, type=int)
    r.add_argument("--lease-id", required=True)
    r.add_argument("--text", default="")
    r.add_argument("--text-file", default="")
    r.add_argument("--mode", choices=["draft", "final"], default="final")
    r.add_argument("--parse-mode", default="Markdown")

    a = sub.add_parser("ack")
    a.add_argument("--ids", required=True)
    a.add_argument("--lease-id", required=True)

    f = sub.add_parser("fail")
    f.add_argument("--id", required=True, type=int)
    f.add_argument("--lease-id", required=True)
    f.add_argument("--error", default="failed")

    x = sub.add_parser("extend")
    x.add_argument("--id", required=True, type=int)
    x.add_argument("--lease-id", required=True)
    x.add_argument("--lease-seconds", type=int, default=180)

    args = parser.parse_args()
    try:
        if args.cmd == "poll":
            result = call("GET", "/poll",
                          params={"limit": args.limit, "lease_seconds": args.lease_seconds})
        elif args.cmd == "reply":
            text = args.text
            if args.text_file:
                with open(args.text_file, encoding="utf-8") as fh:
                    text = fh.read()
            if not text.strip():
                print(json.dumps({"ok": False, "error": "回复内容为空"}, ensure_ascii=False))
                return 2
            result = call("POST", "/reply", {
                "id": args.id,
                "lease_id": args.lease_id,
                "text": text,
                "mode": args.mode,
                "parse_mode": args.parse_mode,
            })
        elif args.cmd == "ack":
            ids = [int(i) for i in args.ids.split(",") if i.strip()]
            result = call("POST", "/ack", {"ids": ids, "lease_id": args.lease_id})
        elif args.cmd == "fail":
            result = call("POST", "/fail", {
                "id": args.id, "lease_id": args.lease_id, "error": args.error})
        elif args.cmd == "extend":
            result = call("POST", "/extend", {
                "id": args.id, "lease_id": args.lease_id,
                "lease_seconds": args.lease_seconds})
        elif args.cmd == "stats":
            result = call("GET", "/stats")
        else:
            result = call("GET", "/pending-count")
        print(json.dumps(result, ensure_ascii=False))
        return 0 if result.get("ok") else 1
    except DynamicCredentialError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
