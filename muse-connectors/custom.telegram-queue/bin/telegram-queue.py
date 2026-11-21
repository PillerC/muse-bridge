#!/usr/bin/env python3
"""Telegram Worker queue helper for the custom.telegram-queue connector.

Calls https://telegram-queue.<your-subdomain>.workers.dev (the Cloudflare Worker + D1 message
queue) with the registered POLLER_TOKEN credential as a Bearer header.
The token never appears here: dynamic_credentials.py fetches an hsurr:
surrogate from authd, and Muse swaps in the real token on approved egress
to the registered host.

Usage:
  python3 telegram-queue.py poll [--limit 5] [--lease-seconds 180]
  python3 telegram-queue.py ack --ids 1,2 --lease-id <uuid>
  python3 telegram-queue.py fail --id 1 --lease-id <uuid> --error "..."
  python3 telegram-queue.py extend --id 1 --lease-id <uuid> [--lease-seconds 180]
  python3 telegram-queue.py stats
  python3 telegram-queue.py pending-count

Prints the Worker JSON response. Exit code 0 when ok=true, 1 when the
Worker returned ok=false, 2 on credential/helper errors.

Note: workers.dev returns 1010 for Python urllib's default User-Agent,
so this helper sends its own User-Agent header.
"""
import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, "/opt/muse/skills/custom-connector-setup/bin")
from dynamic_credentials import (  # noqa: E402
    DynamicCredentialError,
    add_surrogate_to_request,
    read_json_response,
)

CREDENTIAL = "custom.telegram-queue"
HOSTS = ["telegram-queue.<your-subdomain>.workers.dev"]
BASE = "https://telegram-queue.<your-subdomain>.workers.dev"
USER_AGENT = "telegram-muse-bridge/1.0"


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
    add_surrogate_to_request(req, CREDENTIAL, allowed_hosts=HOSTS)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler(), NoRedirect())
    try:
        with opener.open(req, timeout=30) as resp:
            return read_json_response(resp)
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "replace"))
        except Exception:
            return {"ok": False, "error": f"HTTP {e.code}"}


def main():
    parser = argparse.ArgumentParser(description="Telegram Worker queue helper")
    sub = parser.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("poll")
    p.add_argument("--limit", type=int, default=5)
    p.add_argument("--lease-seconds", type=int, default=180)
    a = sub.add_parser("ack")
    a.add_argument("--ids", required=True, help="comma-separated message ids")
    a.add_argument("--lease-id", required=True)
    f = sub.add_parser("fail")
    f.add_argument("--id", required=True, type=int)
    f.add_argument("--lease-id", required=True)
    f.add_argument("--error", default="failed")
    x = sub.add_parser("extend")
    x.add_argument("--id", required=True, type=int)
    x.add_argument("--lease-id", required=True)
    x.add_argument("--lease-seconds", type=int, default=180)
    sub.add_parser("stats")
    sub.add_parser("pending-count")
    args = parser.parse_args()
    try:
        if args.cmd == "poll":
            result = call(
                "GET",
                "/poll",
                params={"limit": args.limit, "lease_seconds": args.lease_seconds},
            )
        elif args.cmd == "ack":
            ids = [int(i) for i in args.ids.split(",") if i.strip()]
            result = call("POST", "/ack", {"ids": ids, "lease_id": args.lease_id})
        elif args.cmd == "fail":
            result = call(
                "POST",
                "/fail",
                {"id": args.id, "lease_id": args.lease_id, "error": args.error},
            )
        elif args.cmd == "extend":
            result = call(
                "POST",
                "/extend",
                {
                    "id": args.id,
                    "lease_id": args.lease_id,
                    "lease_seconds": args.lease_seconds,
                },
            )
        elif args.cmd == "stats":
            result = call("GET", "/stats")
        elif args.cmd == "pending-count":
            result = call("GET", "/pending-count")
        print(json.dumps(result, ensure_ascii=False))
        return 0 if result.get("ok") else 1
    except DynamicCredentialError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
