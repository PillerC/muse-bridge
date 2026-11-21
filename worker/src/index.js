/**
 * Telegram -> Muse 消息队列 (Cloudflare Worker + D1)
 *
 * 数据流:
 *   Telegram --webhook--> Worker /telegram-webhook --入队--> D1
 *   VM 上的 Muse poller --每 5 秒 GET /poll--> Worker --认领--> D1
 *   poller 处理完 --POST /ack 或 /fail--> Worker 更新状态
 *   poller 直接调 api.telegram.org 回复 (typing / 流式编辑), 不经过 Worker,
 *   避免 Worker 成为回复路径上的瓶颈和单点。
 *
 * 稳定性设计:
 *  - update_id UNIQUE 去重: Telegram 重发 webhook 时 INSERT OR IGNORE, 返回 ok
 *  - 租约 (lease): poll 认领时写 lease_id + claimed_at, 超时未 ack/fail 自动释放回 pending
 *  - 退避重试: fail 时 next_retry_at = now + 2^attempts * 10s, 超 MAX_ATTEMPTS 进 dead
 *  - 过滤在 Worker 和 poller 两侧各做一次 (defense in depth), 群里只回 @ 和引用
 */

const JSON_HEADERS = { "content-type": "application/json; charset=utf-8" };

function nowSec() {
  return Math.floor(Date.now() / 1000);
}

function json(data, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: JSON_HEADERS });
}

function checkPollerAuth(request, env) {
  if (!env.POLLER_TOKEN) return false;
  const auth = request.headers.get("authorization") || "";
  return auth === `Bearer ${env.POLLER_TOKEN}`;
}

function normalizeUsername(name) {
  return (name || "").replace(/^@/, "").toLowerCase();
}

/**
 * 判断一条消息是否需要 Muse 回复。
 * 规则 (与 muse-poller/poller.py 的 should_reply 保持一致):
 *  - 私聊: 一律回复
 *  - 群组/超级群: 只有 @ 本 Bot, 或引用 (reply) 本 Bot 的消息才回复
 *  - channel 等其他类型: 不回复
 *  - Bot 自己发的消息: 不回复 (防自激循环)
 */
function classifyMessage(msg, env) {
  const botUsername = normalizeUsername(env.BOT_USERNAME || "");
  const botId = env.BOT_ID ? Number(env.BOT_ID) : null;

  const chatType = msg?.chat?.type || "";
  const from = msg?.from || {};
  const text = msg?.text || msg?.caption || "";
  const entities = [...(msg?.entities || []), ...(msg?.caption_entities || [])];

  // Bot 自己的消息绝不回复
  if (from.is_bot && (normalizeUsername(from.username) === botUsername || (botId && from.id === botId))) {
    return { shouldReply: false, isMention: false, isReplyToBot: false, reason: "self_message" };
  }

  // 是否 @ 本 Bot: 优先看 entities (mention / text_mention), 再看文本包含
  let isMention = false;
  for (const e of entities) {
    if (e.type === "mention") {
      const mentioned = text.slice(e.offset, e.offset + e.length);
      if (normalizeUsername(mentioned) === botUsername) isMention = true;
    }
    if (e.type === "text_mention" && e.user) {
      if (normalizeUsername(e.user.username) === botUsername) isMention = true;
      if (botId && e.user.id === botId) isMention = true;
    }
  }
  if (!isMention && botUsername && text.toLowerCase().includes(`@${botUsername}`)) {
    isMention = true;
  }

  // 是否引用本 Bot 的消息
  const replyTo = msg?.reply_to_message;
  let isReplyToBot = false;
  if (replyTo?.from) {
    if (normalizeUsername(replyTo.from.username) === botUsername) isReplyToBot = true;
    if (botId && replyTo.from.id === botId) isReplyToBot = true;
  }

  if (chatType === "private") {
    return { shouldReply: true, isMention, isReplyToBot, reason: "private" };
  }
  if (chatType === "group" || chatType === "supergroup") {
    if (isMention) return { shouldReply: true, isMention, isReplyToBot, reason: "mention" };
    if (isReplyToBot) return { shouldReply: true, isMention, isReplyToBot, reason: "reply_to_bot" };
    return { shouldReply: false, isMention, isReplyToBot, reason: "group_no_trigger" };
  }
  return { shouldReply: false, isMention, isReplyToBot, reason: `unsupported_chat_${chatType}` };
}

async function handleTelegramWebhook(request, env) {
  // 1. 校验 Telegram secret_token (setWebhook 时设置的 secret_token)
  if (env.WEBHOOK_SECRET) {
    const secret = request.headers.get("x-telegram-bot-api-secret-token") || "";
    if (secret !== env.WEBHOOK_SECRET) {
      return json({ ok: false, error: "bad_secret" }, 401);
    }
  }

  let update;
  try {
    update = await request.json();
  } catch {
    return json({ ok: false, error: "bad_json" }, 400);
  }

  const updateId = update.update_id;
  if (typeof updateId !== "number") {
    return json({ ok: false, error: "missing_update_id" }, 400);
  }

  // 只处理普通消息; edited_message / channel_post / callback_query 等直接 ack 丢弃,
  // 如需支持 inline 按钮, 在这里扩展分支即可。
  const msg = update.message;
  if (!msg) {
    return json({ ok: true, ignored: update?.edited_message ? "edited_message" : "non_message_update" });
  }

  const text = msg.text || msg.caption || "";
  // 空文本且无 caption (纯图片/贴纸等): 仍入队, 让 Muse 决定回什么, 但标记出来
  const cls = classifyMessage(msg, env);
  const ts = nowSec();
  const status = cls.shouldReply ? "pending" : "skipped";

  try {
    const result = await env.DB.prepare(
      `INSERT OR IGNORE INTO messages
        (update_id, chat_id, chat_type, chat_title, message_id,
         from_id, from_username, from_name, text,
         reply_to_message_id, reply_to_from_id, reply_to_username,
         is_mention, is_reply_to_bot, should_reply, status,
         raw_json, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
    )
      .bind(
        updateId,
        msg.chat.id,
        msg.chat.type || "",
        msg.chat.title || null,
        msg.message_id,
        msg.from?.id || null,
        msg.from?.username || null,
        [msg.from?.first_name, msg.from?.last_name].filter(Boolean).join(" ") || null,
        text,
        msg.reply_to_message?.message_id || null,
        msg.reply_to_message?.from?.id || null,
        msg.reply_to_message?.from?.username || null,
        cls.isMention ? 1 : 0,
        cls.isReplyToBot ? 1 : 0,
        cls.shouldReply ? 1 : 0,
        status,
        JSON.stringify(update),
        ts,
        ts
      )
      .run();

    // INSERT OR IGNORE: 重发时 changes=0, 视为已去重
    const deduped = result.meta.changes === 0;
    return json({ ok: true, status, reason: cls.reason, deduped });
  } catch (err) {
    console.error("D1 insert failed", err);
    // 返回 500 让 Telegram 稍后重试; 去重键保证重试不产生重复入队
    return json({ ok: false, error: "db_error" }, 500);
  }
}

/**
 * GET /poll?limit=10&lease_seconds=120
 * 1) 释放过期租约: processing 且 claimed_at + lease_seconds < now -> pending
 * 2) 取最早的 pending 且 next_retry_at <= now 的消息, 写入 lease_id + claimed_at
 * 返回的每条消息都带 lease_id, ack/fail 必须带回相同的 lease_id 才生效。
 */
async function handlePoll(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  const url = new URL(request.url);
  const limit = Math.min(Math.max(Number(url.searchParams.get("limit") || env.POLL_DEFAULT_LIMIT || 10), 1), 50);
  const leaseSeconds = Math.min(Math.max(Number(url.searchParams.get("lease_seconds") || 120), 30), 600);
  const ts = nowSec();
  const leaseId = crypto.randomUUID();

  try {
    // 释放过期租约 (poller 崩溃 / 超时未 ack 时的自愈)
    await env.DB.prepare(
      `UPDATE messages SET status='pending', lease_id=NULL, claimed_at=NULL, updated_at=?
       WHERE status='processing' AND claimed_at IS NOT NULL
         AND claimed_at + lease_seconds < ?`
    )
      .bind(ts, ts)
      .run();

    const rows = await env.DB.prepare(
      `SELECT id, update_id, chat_id, chat_type, chat_title, message_id,
              from_id, from_username, from_name, text,
              reply_to_message_id, reply_to_from_id, reply_to_username,
              is_mention, is_reply_to_bot, attempts, created_at
       FROM messages
       WHERE status='pending' AND next_retry_at <= ?
       ORDER BY created_at ASC LIMIT ?`
    )
      .bind(ts, limit)
      .all();

    const messages = rows.results || [];
    if (messages.length === 0) {
      return json({ ok: true, messages: [], lease_id: leaseId });
    }

    // 认领: 只有 status 仍为 pending 才更新成功, 防两个 poller 同时抢到同一条
    const ids = messages.map((m) => m.id);
    const placeholders = ids.map(() => "?").join(",");
    await env.DB.prepare(
      `UPDATE messages SET status='processing', lease_id=?, claimed_at=?, lease_seconds=?, updated_at=?
       WHERE id IN (${placeholders}) AND status='pending'`
    )
      .bind(leaseId, ts, leaseSeconds, ts, ...ids)
      .run();

    // 重新读一次, 只返回真正认领成功的 (排除并发抢走的)
    const claimed = await env.DB.prepare(
      `SELECT id, update_id, chat_id, chat_type, chat_title, message_id,
              from_id, from_username, from_name, text,
              reply_to_message_id, reply_to_from_id, reply_to_username,
              is_mention, is_reply_to_bot, attempts, created_at
       FROM messages WHERE lease_id=? AND status='processing' ORDER BY created_at ASC`
    )
      .bind(leaseId)
      .all();

    return json({ ok: true, messages: claimed.results || [], lease_id: leaseId });
  } catch (err) {
    console.error("poll failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

async function handleAck(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return json({ ok: false, error: "bad_json" }, 400);
  }
  const ids = Array.isArray(body.ids) ? body.ids : [];
  const leaseId = body.lease_id;
  if (ids.length === 0 || !leaseId) {
    return json({ ok: false, error: "missing_ids_or_lease" }, 400);
  }
  const placeholders = ids.map(() => "?").join(",");
  try {
    const r = await env.DB.prepare(
      `UPDATE messages SET status='done', lease_id=NULL, claimed_at=NULL, updated_at=?
       WHERE id IN (${placeholders}) AND lease_id=? AND status='processing'`
    )
      .bind(nowSec(), ...ids, leaseId)
      .run();
    return json({ ok: true, acked: r.meta.changes });
  } catch (err) {
    console.error("ack failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

async function handleFail(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return json({ ok: false, error: "bad_json" }, 400);
  }
  const id = body.id;
  const leaseId = body.lease_id;
  if (!id || !leaseId) {
    return json({ ok: false, error: "missing_id_or_lease" }, 400);
  }
  const maxAttempts = Number(env.MAX_ATTEMPTS || 5);
  const ts = nowSec();
  try {
    const row = await env.DB.prepare(
      `SELECT attempts FROM messages WHERE id=? AND lease_id=? AND status='processing'`
    )
      .bind(id, leaseId)
      .first();
    if (!row) {
      return json({ ok: false, error: "not_found_or_lease_mismatch" }, 404);
    }
    const attempts = (row.attempts || 0) + 1;
    if (attempts >= maxAttempts) {
      await env.DB.prepare(
        `UPDATE messages SET status='dead', attempts=?, last_error=?, lease_id=NULL, claimed_at=NULL, updated_at=?
         WHERE id=?`
      )
        .bind(attempts, String(body.error || "failed").slice(0, 500), ts, id)
        .run();
      return json({ ok: true, status: "dead", attempts });
    }
    // 指数退避: 10s, 20s, 40s, 80s... 上限 10 分钟
    const backoff = Math.min(10 * 2 ** (attempts - 1), 600);
    await env.DB.prepare(
      `UPDATE messages SET status='pending', attempts=?, last_error=?,
              next_retry_at=?, lease_id=NULL, claimed_at=NULL, updated_at=?
       WHERE id=?`
    )
      .bind(attempts, String(body.error || "failed").slice(0, 500), ts + backoff, ts, id)
      .run();
    return json({ ok: true, status: "pending", attempts, retry_in_sec: backoff });
  } catch (err) {
    console.error("fail failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

/** POST /extend {id, lease_id, lease_seconds}: 长任务续租, 防处理中途被别的 poller 抢走 */
async function handleExtend(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return json({ ok: false, error: "bad_json" }, 400);
  }
  if (!body.id || !body.lease_id) {
    return json({ ok: false, error: "missing_id_or_lease" }, 400);
  }
  const leaseSeconds = Math.min(Math.max(Number(body.lease_seconds || 120), 30), 600);
  try {
    const r = await env.DB.prepare(
      `UPDATE messages SET claimed_at=?, lease_seconds=?, updated_at=?
       WHERE id=? AND lease_id=? AND status='processing'`
    )
      .bind(nowSec(), leaseSeconds, nowSec(), body.id, body.lease_id)
      .run();
    if (r.meta.changes === 0) {
      return json({ ok: false, error: "not_found_or_lease_mismatch" }, 404);
    }
    return json({ ok: true });
  } catch (err) {
    console.error("extend failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

/**
 * GET /pending-count: 公开端点, 只返回待处理消息数, 不返回任何消息内容。
 * 用途: 让无密钥的检测脚本 (如 Muse hook) 每 5 秒判断是否有新消息,
 * 有新消息时再唤醒 agent, agent 用 POLLER_TOKEN 调 /poll 认领。
 * 泄露面仅为一个数字, 不含 chat_id、文本或任何身份信息。
 */
async function handlePendingCount(request, env) {
  try {
    const row = await env.DB.prepare(
      `SELECT COUNT(*) AS c FROM messages WHERE status='pending' AND next_retry_at <= ?`
    )
      .bind(nowSec())
      .first();
    return json({ ok: true, pending: row?.c || 0 });
  } catch (err) {
    console.error("pending-count failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

async function handleStats(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  try {
    const rows = await env.DB.prepare(
      `SELECT status, COUNT(*) AS c FROM messages GROUP BY status`
    ).all();
    const dead = await env.DB.prepare(
      `SELECT id, chat_id, text, attempts, last_error, updated_at
       FROM messages WHERE status='dead' ORDER BY updated_at DESC LIMIT 10`
    ).all();
    return json({ ok: true, counts: rows.results || [], recent_dead: dead.results || [] });
  } catch (err) {
    console.error("stats failed", err);
    return json({ ok: false, error: "db_error" }, 500);
  }
}

/** POST /admin/set-webhook {webhook_url}: 帮你调 Bot API 设置 webhook, 需要 BOT_TOKEN */
async function handleSetWebhook(request, env) {
  if (!checkPollerAuth(request, env)) {
    return json({ ok: false, error: "unauthorized" }, 401);
  }
  if (!env.BOT_TOKEN) {
    return json({ ok: false, error: "BOT_TOKEN_not_configured" }, 500);
  }
  let body;
  try {
    body = await request.json();
  } catch {
    return json({ ok: false, error: "bad_json" }, 400);
  }
  const webhookUrl = body.webhook_url;
  if (!webhookUrl) {
    return json({ ok: false, error: "missing_webhook_url" }, 400);
  }
  const params = {
    url: webhookUrl,
    allowed_updates: ["message"],
    drop_pending_updates: body.drop_pending_updates === true,
  };
  if (env.WEBHOOK_SECRET) params.secret_token = env.WEBHOOK_SECRET;
  const resp = await fetch(`https://api.telegram.org/bot${env.BOT_TOKEN}/setWebhook`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(params),
  });
  const data = await resp.json();
  return json(data, resp.status);
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const path = url.pathname;

    if (request.method === "POST" && path === "/telegram-webhook") {
      return handleTelegramWebhook(request, env);
    }
    if (request.method === "GET" && path === "/poll") {
      return handlePoll(request, env);
    }
    if (request.method === "POST" && path === "/ack") {
      return handleAck(request, env);
    }
    if (request.method === "POST" && path === "/fail") {
      return handleFail(request, env);
    }
    if (request.method === "POST" && path === "/extend") {
      return handleExtend(request, env);
    }
    if (request.method === "GET" && path === "/stats") {
      return handleStats(request, env);
    }
    if (request.method === "POST" && path === "/admin/set-webhook") {
      return handleSetWebhook(request, env);
    }
    if (request.method === "GET" && path === "/pending-count") {
      return handlePendingCount(request, env);
    }
    if (request.method === "GET" && path === "/health") {
      try {
        await env.DB.prepare("SELECT 1").first();
        return json({ ok: true, db: "up", time: nowSec() });
      } catch {
        return json({ ok: false, db: "down" }, 500);
      }
    }
    if (request.method === "GET" && path === "/") {
      return json({
        ok: true,
        service: "telegram-muse-bridge",
        endpoints: ["POST /telegram-webhook", "GET /poll", "POST /ack", "POST /fail", "POST /extend", "GET /stats", "GET /pending-count", "GET /health"],
      });
    }
    return json({ ok: false, error: "not_found" }, 404);
  },
};
