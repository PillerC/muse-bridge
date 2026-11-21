# Telegram Bot API

- `custom.telegram-bot-api` connects Muse to the Telegram Bot API through its REST API. It authenticates with the registered `api_key` credential and calls only `api.telegram.org`.

- Helpers live in `~/workspace/custom-connectors/custom.telegram-bot-api/bin/`. `telegram-bot.py` sends chat actions, sends messages, edits messages and checks the bot identity.

- The bot is the one the registered token belongs to. Its privacy mode setting decides which group messages Telegram delivers to it.

## Connect

- The connection was registered with `credentials.request_api_access` and these details. Reuse them to reconnect:

```json
{
  "provider": "telegram-bot-api",
  "auth_scheme": "api_key",
  "placement": "url_path_segment:/bot{}/",
  "api_hosts": ["api.telegram.org"]
}
```

- Before the first change in a conversation, prove access with a harmless read:

```text
python3 ~/workspace/custom-connectors/custom.telegram-bot-api/bin/telegram-bot.py get-me
```

## Credentials

- The registered connection holds the credential. Helpers import `/opt/muse/skills/custom-connector-setup/bin/dynamic_credentials.py`, which returns an `hsurr:` surrogate and places it where the provider reads the key. Muse swaps in the real credential only on an approved request to a registered host.

- The helper inserts the surrogate into the URL path raw, without percent-encoding. The bundled `url_with_surrogate_path_segment` percent-encodes it and Sentinel then does not recognize it, so Telegram answers 404. This was verified in the reference deployment.

- Never put a key, token, password or cookie in this doc, a helper, a command argument, an environment variable or a log. Never ask the user to paste one in chat.

- Keep redirects disabled on authenticated requests.

- On a 401 or 403, check the request first. Confirm that the helper attached the credential, that the host is registered and that the placement and scopes match.

- Only when a request that carried the credential is still rejected, call `credentials.request_api_access` with the details above and `"reconnect": true`.

## Check the bot identity

- `get-me` reads the bot identity and proves the credential works. It changes nothing.

```text
python3 ~/workspace/custom-connectors/custom.telegram-bot-api/bin/telegram-bot.py get-me
```

- The output has `ok` and a `result` with `id`, `first_name` and `username`. Verified end to end in the reference deployment.

## Show the typing indicator

- `send-chat-action` shows a typing indicator in a chat while Muse works on a reply. It changes what the chat sees but sends no message.

```text
python3 ~/workspace/custom-connectors/custom.telegram-bot-api/bin/telegram-bot.py send-chat-action --chat-id <id> --action typing
```

- Pass the chat id as an integer. Group chat ids are negative. The action lasts about five seconds, so repeat the call for long work. Verified end to end in the reference deployment.

## Send a message

- `send-message` sends a text message to a chat. Use `--reply-to` with a message id to reply to a specific message.

```text
python3 ~/workspace/custom-connectors/custom.telegram-bot-api/bin/telegram-bot.py send-message --chat-id <id> --text "..." --reply-to <message-id>
```

- The output has `ok` and a `result` with the sent `message_id`. Keep that id to edit the message later. Use `--text-file` with a file path instead of `--text` for long messages. Verified end to end in the reference deployment.

## Edit a message

- `edit-message` replaces the text of a message the bot sent. Use it to turn a placeholder message into the finished reply.

```text
python3 ~/workspace/custom-connectors/custom.telegram-bot-api/bin/telegram-bot.py edit-message --chat-id <id> --message-id <id> --text "..."
```

- The output has `ok` and a `result` with the edited message. Use `--text-file` with a file path instead of `--text` for long messages. Verified end to end in the reference deployment.

## Recover from errors

- A 404 Not Found means the token in the URL path was malformed. With this connector it means the surrogate was not swapped, so confirm the helper inserts the surrogate raw and the request goes through the egress proxy.

- A 401 Unauthorized means the stored token is wrong or revoked. Check the request first, then reconnect with the details above.

- A 429 means Telegram rate limited the bot. The response carries `retry_after` in seconds. Wait that long before retrying.

- After a failed or timed-out change, check the current state with a read before trying again, because the first request may have completed.

## Limits

- The connector only calls api.telegram.org. It does not manage the webhook or the message queue, which live in the Cloudflare Worker and are reached through custom.telegram-queue.

- The helper implements four Bot API methods only. Other Bot API methods are untested through this connector.

## Related skills

- Use `muse.skill_search` to find other skills and `muse.connector_search` to find other connectors.
