# Telegram Queue

- `custom.telegram-queue` connects Muse to the Telegram message queue through its REST API. It authenticates with the registered `api_key` credential as a Bearer header and calls only the Worker host below.

- Helpers live in `~/workspace/custom-connectors/custom.telegram-queue/bin/`. `telegram-queue.py` polls, acknowledges, fails, extends and inspects queued Telegram messages.

- The queue is a Cloudflare Worker backed by D1. Telegram delivers messages to the Worker webhook, the Worker filters them and stores the ones to answer as pending, and Muse claims them through this connector.

## Connect

- The connection was registered with `credentials.request_api_access` and these details. Reuse them to reconnect. Replace the host with your own Worker host:

```json
{
  "provider": "telegram-queue",
  "auth_scheme": "api_key",
  "placement": "bearer_header",
  "api_hosts": ["telegram-queue.<your-subdomain>.workers.dev"]
}
```

- Before the first change in a conversation, prove access with a harmless read:

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py stats
```

## Credentials

- The registered connection holds the credential. Helpers import `/opt/muse/skills/custom-connector-setup/bin/dynamic_credentials.py`, which returns an `hsurr:` surrogate and places it where the provider reads the key. Muse swaps in the real credential only on an approved request to a registered host.

- Never put a key, token, password or cookie in this doc, a helper, a command argument, an environment variable or a log. Never ask the user to paste one in chat.

- Keep redirects disabled on authenticated requests.

- On a 401 or 403, check the request first. Confirm that the helper attached the credential, that the host is registered and that the placement and scopes match.

- Only when a request that carried the credential is still rejected, call `credentials.request_api_access` with the details above and `"reconnect": true`.

## Check queue stats

- `stats` reads counts by status and recent dead messages. It changes nothing.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py stats
```

- The output has `ok`, `counts` with one entry per status and `recent_dead` with the latest dead messages. Verified end to end in the reference deployment.

## Check the pending count

- `pending-count` reads only the number of pending messages. It changes nothing.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py pending-count
```

- The output has `ok` and `pending`. The hook polling script reads this endpoint directly with curl because the script has no credentials. Verified end to end in the reference deployment.

## Claim messages

- `poll` claims the oldest pending messages under a lease and marks them processing. Claim with a lease long enough for the reply work.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py poll --limit 5 --lease-seconds 180
```

- The output has `ok`, a `messages` array and a top level `lease_id`. Each message has `id`, `chat_id`, `chat_type`, `message_id`, `from_username`, `from_name`, `text`, `reply_to_message_id`, `is_mention`, `is_reply_to_bot` and `attempts`. Keep the `lease_id` because ack, fail and extend all need it. Verified end to end in the reference deployment.

## Acknowledge messages

- `ack` marks claimed messages done after the reply went out. Pass the ids as a comma separated list with the lease id from poll.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py ack --ids 1,2 --lease-id <uuid>
```

- The output has `ok` and the number of messages acknowledged. Acking with a wrong or expired lease id acknowledges nothing. Verified end to end in the reference deployment.

## Mark a message failed

- `fail` returns a claimed message to pending with backoff, or marks it dead after too many attempts. Use it only for temporary errors.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py fail --id 1 --lease-id <uuid> --error "..."
```

- The output has `ok`, the new `status` and `attempts`. Do not put secrets in the error text. Untested through this connector.

## Extend a lease

- `extend` renews the lease on a claimed message while long work continues. Call it before the lease expires so no other worker claims the message.

```text
python3 ~/workspace/custom-connectors/custom.telegram-queue/bin/telegram-queue.py extend --id 1 --lease-id <uuid> --lease-seconds 180
```

- The output has `ok` and the new lease. Untested through this connector.

## Recover from errors

- A 401 or 403 means the stored credential was rejected. Check the request first, then reconnect with the details above.

- A 404 means the endpoint path is wrong or the lease no longer matches. Check the helper command and the lease id.

- A poll that returns no messages means another worker claimed them or the queue is empty. End the run without replying.

- After a failed or timed-out change, check the current state with a read before trying again, because the first request may have completed.

## Limits

- The connector only calls the Worker host. It does not send Telegram messages, which go through custom.telegram-bot-api.

- The queue only fills through the Telegram webhook, which requires the Telegram secret token. Test with a real Telegram message.

## Related skills

- Use `muse.skill_search` to find other skills and `muse.connector_search` to find other connectors.
