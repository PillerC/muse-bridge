#!/usr/bin/env bash
# 每 5 秒检查 Telegram 队列是否有待处理消息, 有则唤醒 agent 回复。
# 只调公开的 /pending-count (仅返回一个数字, 无消息内容), 不含任何密钥。
set -euo pipefail
source "$HATCH_HOOK_RUNTIME"

WORKER_URL="https://telegram-queue.<your-subdomain>.workers.dev"  # 改成你的 Worker 地址
STATE_FILE="$HOME/hooks/state/telegram-pending.json"
DRY_RUN="${HATCH_HOOK_DRY_RUN:-0}"

resp=$(curl --fail --silent --show-error --max-time 10 "$WORKER_URL/pending-count" || true)
if [ -z "$resp" ]; then
  log "fetch_failed" '{}'
  silent "worker unreachable"
  exit 0
fi

pending=$(printf '%s' "$resp" | jq -r '.pending // 0' 2>/dev/null || echo 0)
now=$(date +%s)

# 读上次状态 (last_wake_at, last_count), 缺失则默认为 0
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

# 有待处理消息: 距上次唤醒超过 90 秒, 或数量比上次多 (新消息到了), 就唤醒。
# 90 秒静默期避免同一个长处理反复唤醒多个 agent; 租约机制保证重复唤醒也不会重复回复。
if [ $((now - last_wake)) -ge 90 ] || [ "$pending" -gt "$last_count" ]; then
  write_state "$now" "$pending"
  wake "telegram_pending" "{\"pending\":$pending,\"detected_at\":$now}"
  exit 0
fi

write_state "$last_wake" "$pending"
silent "already woken recently"
