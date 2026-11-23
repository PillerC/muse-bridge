#!/usr/bin/env bash
# 每 5 秒检查桥里是否有待处理消息，有则唤醒 agent 回复。
# Muse 里的 hook 名建议用 muse-bridge-pending，与桥详情页的接入提示词保持一致。
#
# 只调用匿名的 /pending-count，只返回一个数字，不携带任何令牌。
#
# bridge=<slug> 是必填的：服务端不再提供"所有桥合计"这个兜底，
# 不带参数会返回 400。以前那个兜底会让所有 agent 被同一座桥的消息叫醒，
# 醒来 poll 却只领得到自己的消息，等于白跑一轮。
set -euo pipefail
source "$HATCH_HOOK_RUNTIME"

BRIDGE_URL="${MUSE_BRIDGE_URL:-https://bridge.example.com}"
BRIDGE_SLUG="${MUSE_BRIDGE_SLUG:-}"
STATE_FILE="$HOME/hooks/state/telegram-pending.json"
DRY_RUN="${HATCH_HOOK_DRY_RUN:-0}"

# 没有 slug 就别发请求了：发出去必然 400，不如在本地就报清楚。
if [ -z "$BRIDGE_SLUG" ]; then
  log "missing_slug" '{}'
  silent "MUSE_BRIDGE_SLUG 未设置，无法查询待处理数"
  exit 0
fi

count_url="$BRIDGE_URL/pending-count?bridge=$BRIDGE_SLUG"

resp=$(curl --fail --silent --show-error --max-time 10 "$count_url" || true)
if [ -z "$resp" ]; then
  log "fetch_failed" '{}'
  silent "bridge unreachable"
  exit 0
fi

pending=$(printf '%s' "$resp" | jq -r '.pending // 0' 2>/dev/null || echo 0)
now=$(date +%s)

last_wake=0
last_count=0
if [ -f "$STATE_FILE" ]; then
  last_wake=$(jq -r '.last_wake_at // 0' "$STATE_FILE" 2>/dev/null || echo 0)
  last_count=$(jq -r '.last_count // 0' "$STATE_FILE" 2>/dev/null || echo 0)
fi

write_state() {
  if [ "$DRY_RUN" = "1" ]; then return 0; fi
  mkdir -p "$(dirname "$STATE_FILE")"
  printf '{"last_wake_at":%s,"last_count":%s}' "$1" "$2" > "$STATE_FILE.tmp"
  mv "$STATE_FILE.tmp" "$STATE_FILE"
}

if [ "$pending" -le 0 ]; then
  write_state "$last_wake" 0
  silent "no pending messages"
  exit 0
fi

# 90 秒静默期避免同一个长处理反复唤醒；租约机制保证重复唤醒也不会重复回复。
if [ $((now - last_wake)) -ge 90 ] || [ "$pending" -gt "$last_count" ]; then
  write_state "$now" "$pending"
  wake "telegram_pending" "{\"pending\":$pending,\"detected_at\":$now}"
  exit 0
fi

write_state "$last_wake" "$pending"
silent "already woken recently"
