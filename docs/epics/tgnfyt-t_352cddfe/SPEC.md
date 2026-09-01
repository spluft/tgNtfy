# tgNtfy v1 — Binding Implementation SPEC

**Epic:** t_352cddfe — tgNtfy v1 Go gate with TG forum-group delivery
**Authoritative input:** `docs/epics/tgnfyt-t_54a6debb/RESEARCH.md` (research epic t_54a6debb, on main)
**Project:** tgNtfy (`github.com/spluft/tgNtfy`) · board `spluft` · default branch `main`
**Feature branch:** `feature/tgnfyt-t_352cddfe-gate-v1`
**Status:** BINDING for backend / qa / integrate / acceptance. Any deviation requires a spec amendment, not a worker decision.
**Language:** English (user convention). Code identifiers English; user-facing TG strings English (Nikolay prefers English).

---

## 0. Frozen owner decisions (MUST NOT be contradicted anywhere in this spec or its implementation)

| # | Decision | Binding consequence |
|---|----------|--------------------|
| **D-1** | Delivery topology = **OPTION C**: personal private **FORUM** group per user, topics = services, from day one. Ritual: `/setup` step-by-step → user creates a private group, adds the bot as admin with `can_manage_topics`, then `/connect <code>` in that group. Bot creates one topic per subscribed service via `createForumTopic`; every event is delivered with `message_thread_id`. The data model keeps the `delivery_mode` field; in v1 **group is the only PRODUCTION mode** — DM fallback exists **only** for pre-setup smoke testing. | §4 data model, §7 delivery, §11 `/setup` + `/connect` flows, QA forum tests. |
| **D-2** | **No NATS adapter inside the gate.** HTTP ingest only. | `internal/ingest` is the only ingest path; no NATS client dependency anywhere. |
| **D-3** | **`job_progress` events are dropped in v1** (dropped at ingest, counted, logged at debug). | §3.3 catalog has no `job_progress` entry; §8.4 drop rule. |
| **D-4** | **kidsEdu is out of scope for v1.** Its event contract remains reserved in research §3.3 only; no catalog entry, no adapter. | §3.3 catalog has no `kidsedu` entries; §16 out-of-scope. |

### Architect's binding decisions (made in this spec; workers must follow)

| # | Decision | Justification (one line) |
|---|----------|--------------------------|
| A-1 | Bot API library: **`github.com/go-telegram/bot` v1.25.0** (NOT `telego/tg`, NOT `go-telegram-bot-api`). | Stack consistency: goVpnWork (v1.19.0) and goRecomendarr (v1.22.0) already run this library; verified v1.25.0 exposes `CreateForumTopic` (`ChatID`, `Name`, `IconColor`) returning `models.ForumTopic{MessageThreadID}` and `SendMessageParams.MessageThreadID` — everything D-1 requires. |
| A-2 | SQLite driver: **`modernc.org/sqlite` v1.57.0** (pure Go, no cgo). | Frozen by the card; matches research §5 and the spluft static-binary Dockerfile pattern. |
| A-3 | **Admin surface = CLI subcommands of the same binary** (`tgntfy admin …`), NOT HTTP admin endpoints. | No extra network attack surface; the HTTP surface stays exactly: `POST /v1/events`, `POST /v1/link`, `GET /api/health`, `GET /metrics`. Tokens/links are low-frequency ops — a CLI run via `docker exec` is sufficient for v1. |
| A-4 | **Unknown `service`/`type` combinations are accepted (200)**, stored, counted in `catalog_unknown_total`, logged at warn. Unknown `service` in `POST /v1/link` → 400. | Forward-compatible with future service versions; the catalog gates *rendering/subscription* semantics, not ingest. |
| A-5 | Topic→service mapping lives in a dedicated table **`group_topics`** (not overloaded onto `subscriptions`). | Keeps research §5's six-table model intact and gives one clean place to store `message_thread_id` per (user, service). |
| A-6 | One-time codes: **two separate tables** — `link_codes` (identity binding, service-side) and `connect_codes` (group binding, `/setup` ritual). Same mechanics (6 digits, 10 min TTL, single use). | Keeps the two rituals independent and auditable; a leaked code in one flow cannot be replayed in the other. |
| A-7 | Ingest rate limit = **two counters per service**: sliding 1-second window (burst, cap **30**) AND sliding 60-second window (sustained, cap **100**); breach → `429` + `Retry-After: 1`. | Deterministic, in-memory, testable — satisfies research §3.1 "30 ev/s burst, 100/мин". |
| A-8 | Coalescing = **5s tumbling window per (user, service, type)**: the first event of a key arms a 5s timer; further events of the same key before the timer fire join the batch; timer fire flushes the batch as ONE message; batch cap 20 (flush early if reached). | 60 same-type events over 10s → flush at t=5s (≤30 ev) + flush at t=10s (≤30 ev) = **≤2 messages**, satisfying the AC verbatim. |
| A-9 | Rendered TG messages are **plain text** (`parse_mode` unset), one event = one line, link as its own last line; batch = summary header + one line per event. | No escaping bugs, no HTML/Markdown injection from service-provided `title`/`text`. |
| A-10 | Delivery retry = **exponential backoff 5s × 2^n (capped 5 min), max 5 attempts**; TG 429 responses use the returned `retry_after` verbatim (and do NOT consume an attempt counter increment beyond the base retry). | Research §5 NFR, verbatim. |

---

## 1. Business description

spluft runs five self-hosted services on 192.168.1.200 (Portainer): goYoutube,
goMailClient, goRecomendarr, goVpnWork, kidsEdu. Today their notifications are
inconsistent: goRecomendarr and goVpnWork have product-specific Telegram bots,
the rest have none. Operators therefore either miss events (video failed, VPN
down, mail sync broken) or maintain three different notification mechanisms.

**tgNtfy** is one unified **notification gate**:

1. Every service posts its events to a single HTTP endpoint with one JSON
   envelope and a per-service token — adding a new service is a `POST`, zero
   gate changes (frozen contract, §3).
2. The gate owns **multi-user routing**: each Telegram user self-serves which
   services and which event types they want, per-service mute, and their own
   delivery destination.
3. Delivery is a **personal private Telegram forum group per user**, one
   **topic per service** (frozen D-1): full privacy (no shared chat), native
   per-service mute/sound via Telegram topics, one tap per service.
4. The gate is the single accountable delivery point: idempotent ingest,
   rate-limited, coalesced (bursts become one message), retried with backoff,
   and auditable (`/status`, `/undelivered`, Prometheus metrics).

**User value:** one bot, one group, everything I care about; silence I control;
nothing of user A ever appears in user B's group (isolation is a first-class
guarantee, §13). **Operational value:** a single ~15 MB container, stable
contract for the four Go services, and an observable pipeline (events_in →
coalesce → queue → deliveries_ok/fail).

---

## 2. Use cases

Actors: **User** (Telegram human, e.g. Nikolay), **Service** (goyoutube, gomail,
gorecomendarr, govpn — HTTP client), **Admin** (Nikolay, runs `tgntfy admin` CLI).

### UC-1 — Setup ritual: user provisions their personal forum group (D-1)
- **Actor:** User. **Goal:** personal private forum group becomes their delivery target.
- **Main flow:**
  1. User: `/start` → welcome; bot explains the model ("one private group, one topic per service") and offers `/setup`.
  2. User: `/setup` → bot starts a **step-by-step script** (state machine, §11.3):
     - Step 1 text: "Create a **new private group** in Telegram (any name, e.g. 'my tgntfy'). Add members: **only you**."
     - Step 2 text: "Add the bot as **admin** of the group with the permission **Manage topics** (can_manage_topics)."
     - Step 3: bot tells the user to send the connect code in the group: "Send in the group: `/connect <code>`" (a 6-digit `connect_code` is generated, 10 min TTL).
  3. In the group the user sends `/connect 123456` (or `/connect@tgntfy_bot 123456`).
  4. Gate: code valid (connect_codes), chat `is_forum` = true, bot is admin with `can_manage_topics` (verified via `getChat` → member status Administrator with `can_manage_topics=true`), sender is an **admin** of the group (else error, step 4a).
  5. Gate: `users.delivery_mode := 'group'`, `users.group_chat_id := <chat_id>`; creates one **topic per linked service** via `createForumTopic(chat_id, display_name)` → stores `group_topics(user_id, service, message_thread_id)`; sends confirmation in the group ("Setup complete. Topics created: goYouTube, Mail, …").
- **Alternative flows:**
  - 4a. Sender not admin in the group → "Only a group admin can complete setup." State stays in step 3; code not consumed.
  - 4b. Bot lacks `can_manage_topics` → "I can see the group but am missing the **Manage topics** admin right — please grant it, then resend `/connect <code>`" (code not consumed, user may re-run `/setup` for a fresh code).
  - 4c. Chat is not a forum (user created a regular group) → "This group has **topics disabled**. Create a forum group (group settings → Topics → on) or a new one, then resend."
  - 4d. Code expired/wrong → "Code not found or expired. Run `/setup` again in the DM." (state machine re-issues on `/setup`).
- **Error flow:** any `getChat`/`createForumTopic` transport failure → 429-aware retry once; on failure, "Telegram is busy — retry `/connect <code>` in a minute" (code kept, TTL untouched on transport errors).

### UC-2 — Identity linking (service user ↔ Telegram user)
- **Actor:** User + Service. **Goal:** gate knows "service user 17 of goyoutube = this Telegram user".
- **Main flow (code-link, goRecomendarr pattern):**
  1. User in DM: `/link` → bot: "Which service?" inline keyboard (registered services). User picks `goyoutube` (or any registered service).
  2. Gate creates a **6-digit one-time `link_code`** (TTL 10 min) and replies: "Open **goYouTube → Profile → Notifications**, enter code **482913** (expires 10 min)."
  3. User enters the code in the service's web UI. The service (holding its `X-Service-Token`) calls `POST /v1/link {"service":"goyoutube","user_ref":"17","code":"482913"}`.
  4. Gate: token valid → code valid+unexpired+unused → upserts `service_users(service, user_ref, user_id, status='active')`, consumes the code, and ensures a `subscriptions` row (all event types on).
  5. Gate replies in the DM: "Linked **goYouTube** (user 17). You now receive goyoutube events — tune them in `/menu`."
- **Alternative flows:**
  - 2a. **Direct auto-link for single-user services:** govpn has no per-user web flow — its events use `user_ref: "admin"`; the gate auto-links `service_users('govpn','admin', <first /start user>)` on first `/start` (documented; subsequent users cannot re-claim: link attempt → 409).
  - 2b. **Admin manual link:** `tgntfy admin link <service> <user_ref> <tg_user_id|@username>` — for services without a web UI hook (reserved until a kidsEdu adapter exists; D-4).
- **Error flow:** wrong/expired code → 400 `code_invalid`; service not registered or disabled → 400 `service_unknown`; user_ref already linked to a *different* TG user → 409 `already_linked` (service shows "already linked to another user — contact admin"); same user re-linking same identity → 200 idempotent refresh.

### UC-3 — Event flow: service event → Telegram topic message
- **Actor:** Service. **Goal:** one event lands as one (or one batched) message in the owner's service topic.
- **Main flow:**
  1. `POST /v1/events` with `X-Service-Token`; gate: token → service (sha256 compare) → 401 if bad.
  2. Schema validation (§3.2) → 400 on violation; body > 8 KB → 413.
  3. Rate limit (per-service 30/s burst + 100/min) → 429 + `Retry-After` on breach.
  4. Idempotency: `event_id` UQ — duplicate within 24 h → **409** (not re-processed).
  5. Route: `service_users` rows for `(service, user_ref)` → for each linked TG user: subscription filter (`subscriptions.service` row exists, not muted, `event_types` NULL or contains `type`).
  6. Coalesce key `(user_id, service, type)`, 5s window (§8).
  7. On flush: render message (§8.5), append to **per-chat delivery queue** (TG limit ~30 msg/min/chat, §9).
  8. Dispatcher sends `sendMessage(chat_id, message_thread_id=<topic>, text)`; stores `deliveries` row (`tg_msg_id`, `status='delivered'`).
- **Alternative flows:**
  - 5a. No linked users for the `user_ref` → event stored (`events` row), no `deliveries`, counted in `events_unrouted_total`; not an error (service-side identity simply unlinked).
  - 5b. User is in `delivery_mode='group'` but has no topic for that service yet (linked after `/connect`) → gate **lazily creates the topic** before first delivery (same rule as UC-1 step 5).
  - 5c. User not yet set up (`delivery_mode='dm'` / no group) → event delivered to the **DM** (smoke-test path only, §7.3); this is the *only* production-visible DM use.
  - 5d. `job_progress` type (or any catalog entry with `drop: true`) → **dropped at ingest**, counted `catalog_dropped_total`, 200. (D-3.)
- **Error flows:** see UC-7 matrix.

### UC-4 — Self-serve menu: per-service + per-event-type toggles and mute
- **Actor:** User. **Goal:** control what they receive, without admin help.
- **Main flow:**
  1. User: `/menu` → inline keyboard: one row per **linked service**: `✅ goYouTube` / `✅ Mail` (service-level toggle = mute/unmute all) + `➕ link more…`.
  2. Tap a service row → second-level keyboard: one toggle row per catalog event type for that service (`✅ job_completed`, `✅ job_failed`, `⬜ job_cancelled`) + `🔕 Mute all` + `⬅️ Back`.
  3. Every tap: `AnswerCallbackQuery` (no alert) + persist `subscriptions` (`muted` / `event_types` JSON) + re-render the keyboard with updated icons.
- **Alternative flow:** service not linked → its row shows `➕ link` → routes into UC-2.
- **Error flow:** stale keyboard (service unlinked meanwhile) → tap answered with "Service no longer linked — press /menu to refresh." All mutations require the callback sender's `tg_user_id` to exist in `users` and own the subscription (else "unknown user", nothing written).

### UC-5 — Status and delivery audit
- **Actor:** User. **Goal:** see what was delivered and what failed.
- **Main flow:**
  1. `/status` → text block: delivery mode + group title; per linked service: last event title+time; last 10 delivered events (service, type, title, time); `undelivered: N`.
  2. `/undelivered` → list of failed deliveries (up to 20): `#<id> <service> <type> <title> — <last_err> (attempt k/5)` + inline button `🔁 Retry all failed` → re-enqueues each (≤50 per click), answers with counts. (Semantics frozen: show + retry; no individual-delete in v1.)
- **Error flow:** no deliveries yet → "Nothing delivered yet."

### UC-6 — Admin: issue/rotate service tokens, manual link (A-3: CLI)
- **Actor:** Admin (Nikolay via `docker exec`). **Goal:** onboard services, rotate credentials, rescue links.
- **Commands** (§10.3, exact CLI contract):
  - `tgntfy admin service create <id> --name "Display"` → registers `services` row, prints the **raw token once** (32 B random, hex 64); only sha256 stored.
  - `tgntfy admin service list` / `service disable <id>` / `service enable <id>`.
  - `tgntfy admin service rotate <id>` → new raw token printed once; old token stops working immediately (A-7b: no grace period in v1 — documented; services must be updated before rotation).
  - `tgntfy admin link <service> <user_ref> <tg_user_id|@username>` / `admin unlink <service> <user_ref>`.
  - `tgntfy admin user list` (users + delivery_mode + group).
  - `tgntfy admin events recent [-n 20]` (ingest audit).
- **Main flow:** each command runs against the same SQLite file (WAL — safe while server runs); token output goes to stdout only, never to the DB or logs (logs print `token=***`).
- **Error flow:** unknown service id → exit 1 with clear message; rotate of disabled service → allowed but flagged.

### UC-7 — Error and edge flows (matrix — MUST all be covered by tests)

| # | Scenario | Gate behavior | HTTP/TG outcome |
|---|----------|---------------|-----------------|
| E-1 | Wrong/missing `X-Service-Token` | sha256 compare fails | **401** `{"error":"unauthorized"}` |
| E-2 | Malformed JSON / missing required field / bad `v` / bad `severity` | schema validation | **400** `{"error":"schema","detail":"…"}` |
| E-3 | Duplicate `event_id` within 24 h | UQ hit → no re-route | **409** `{"error":"duplicate"}` |
| E-4 | Body > 8192 bytes | size check before JSON parse | **413** `{"error":"too_large"}` |
| E-5 | > 30 events/s or > 100/min for one service | sliding-window counters | **429** + `Retry-After: 1` |
| E-6 | Bot not admin / lacks `can_manage_topics` in the group (UC-1 step 4b) | getChat check | guided error text, code preserved |
| E-7 | Telegram `sendMessage` 429 (global/chat flood limit) | honor `parameters.retry_after`, reschedule, no attempt-consume | message delayed, `deliveries.attempts` unchanged; metric `deliveries_429_total` |
| E-8 | Delivery retries exhausted (5 attempts: backoff 5s, 10s, 20s, 40s, 80s — A-10) | `deliveries.status='failed'`, `last_err` set | visible in `/status` (undelivered N) + `/undelivered` retry |
| E-9 | Group chat ID in DB no longer exists / bot kicked from group (403 on send) | fail fast after attempt 1, `last_err` records 403 | failed delivery + "Bot may have been removed from the group — run /setup" hint in `/status` |
| E-10 | Event for a `user_ref` with no linked users | stored, not routed | 200 accepted; `events_unrouted_total` |
| E-11 | `delivery_mode='group'` but topic row missing | lazy `createForumTopic` (UC-3 5b); on failure → normal retry path | at most one extra API call per (user, service) |
| E-12 | Same `event_id` re-posted after the 24 h retention was pruned | treated as NEW (retention expired) | 200 + re-delivered (documented, acceptable) |
| E-13 | `/link` code bound by a *different* service than requested | code row stores service → mismatch → 400 `code_invalid` | service shows invalid-code error |
| E-14 | Two users try to complete `/connect` with the same code | single-use: second gets "code already used" | no double-binding possible |
| E-15 | 60 same-type events in 10 s for one user+service | coalescing §8 | **≤ 2 messages** (AC) |

---

## 3. Event envelope contract (FROZEN — field names/types verbatim from research §3.1)

### 3.1 Endpoint
`POST /v1/events` — HTTP (LAN/VPN; TLS optional, see §14). Content-Type: `application/json`.
Auth: header `X-Service-Token: <raw-token>` (per-service, issued by admin CLI, §10.3).

### 3.2 Envelope schema (field names and types are the contract)

```json
{
  "v": 1,
  "event_id": "goyoutube:job:4821:completed:2026-09-01T11:22:33Z",
  "service": "goyoutube",
  "user_ref": "17",
  "type": "job_completed",
  "severity": "info",
  "title": "Видео скачано",
  "text": "«That Italy from Pinterest | Where Billionaires Vacation» (10:47)",
  "url": "https://goyt.spluft.ru/#/queue/4821",
  "metadata": {"job_id": 4821, "channel": "UyeF5cGujes"}
}
```

| Field | Type | Required | Rules |
|-------|------|----------|-------|
| `v` | int | yes | Must be `1` (other → 400). |
| `event_id` | string, 1..255 | yes | **Idempotency key.** Recommended shape `<service>:<kind>:<id>:<type>:<RFC3339>` (services follow; gate enforces only presence/length). UQ, 24 h retention window (§8.4). |
| `service` | string, 1..32 | yes | Registered service id (services table). Unregistered → 400 `service_unknown` (400 = identity/schema error; 401 is token-only). |
| `user_ref` | **string** | yes | Identity of the end-user *in that service* (string because identities differ: int-as-string, username, external). Service-level events use `"admin"` (e.g. govpn). |
| `type` | string, 1..64 | yes | Event type from the service's catalog entry (§3.3). Unknown type → **accepted** (A-4), stored as-is, `catalog_unknown_total`. |
| `severity` | string | yes | One of `info` \| `warn` \| `error` \| `success` (else 400). |
| `title` | string, 1..200 | yes | Short headline, rendered line 2. |
| `text` | string, 0..500 | no | Detail line (may be empty). |
| `url` | string, 0..500 | no | Optional link, rendered as last line (plain, no parse mode — A-9). |
| `metadata` | JSON object | no | Free-form; gate does not interpret it (routing uses only envelope fields, §3.4). |

**Body size cap: 8192 bytes → 413 `too_large`.** (Check before JSON decode.)

### 3.3 Status codes (frozen)
`200` accepted+queued · `400` schema/identity (`detail` names the field) ·
`401` bad or missing service token · `409` duplicate `event_id` within 24 h ·
`413` body > 8 KB · `429` per-service rate limit exceeded (+`Retry-After: 1`).

Error body shape: `{"error": "<code>", "detail": "<optional>"}`.

### 3.4 Routing (after validation) — frozen from research §3.4
1. `service + user_ref` → `service_users` rows (identity binding).
2. For each linked TG user: subscription filter (`subscriptions.service` + `event_types` (NULL/empty = all) + `muted`).
3. Coalesce window (default 5 s; §8) → 1 TG message per event **or** per batch.
4. Per-chat delivery queue → dispatcher (§7, §9).

---

## 4. events.yaml catalog (config, extensible WITHOUT re-release)

### 4.1 File format (YAML, v1) — v1 entries exactly as research §3.3

```yaml
version: 1
services:
  goyoutube:
    display_name: goYouTube
    events:
      job_completed:  {severity: success}
      job_failed:     {severity: error}
      job_cancelled:  {severity: info}
  gomail:
    display_name: Mail
    events:
      new-mail:       {severity: info}
      sync-status:    {severity: warn}
  gorecomendarr:
    display_name: Recomendarr
    events:
      download_completed: {severity: success}
      job_failed:         {severity: error}
  govpn:
    display_name: VPN
    events:
      vpn_connected:    {severity: success}
      vpn_disconnected: {severity: error}
```

Rules:
- **Loaded at startup and re-read on `SIGHUP`.** Bad file on SIGHUP → keep old catalog, log error, metric `catalog_reload_fail_total`. (File-watcher optional in v1; SIGHUP is sufficient.)
- A catalog entry's `severity` is a **default** used for `/menu` grouping and docs only; the event's own `severity` field always drives rendering.
- `drop: true` on an entry = D-3-style drop (accepted, counted, not delivered). v1 catalog has no `drop` entries, but the field is supported (future `job_progress`-style noise control).
- **v1 entries are exactly** research §3.3: goyoutube `job_completed`/`job_failed`/`job_cancelled`; gomail `new-mail`/`sync-status`; gorecomendarr `download_completed`/`job_failed`; govpn `vpn_connected`/`vpn_disconnected`. **No kidsedu** (D-4). **No `job_progress`** (D-3).
- Service ids in the catalog MUST match `services` table ids 1:1 (admin creates the service row + catalog entry together).
- New service / new event type = edit `events.yaml` + SIGHUP → zero re-release (frozen requirement). The repo ships `config/events.yaml` with exactly the v1 entries above (AC-8).

### 4.2 Template note (out of scope for v1 rendering)
Research §3.3's "text (шаблон)" column documents the *source* phrasing; v1 renders the **service-provided** `title`/`text` verbatim (§8.5). Per-service custom templates stay out of scope (research §8).

---

## 5. Go module layout (frozen)

Module: `github.com/spluft/tgNtfy` · **Go 1.25.0** (spluft baseline; goVpnWork/goMailClient pin 1.25.0).

```
cmd/tgntfy/main.go        — flags/env wiring, graceful shutdown (SIGTERM/SIGINT), SIGHUP catalog reload
internal/ingest           — HTTP server: POST /v1/events, POST /v1/link; token auth, schema, size, rate limit, idempotency
internal/catalog          — events.yaml load/validate/reload; lookups (service→types, display names)
internal/limit            — per-service sliding-window rate limiter (30/s burst, 100/min) [A-7]
internal/coalesce         — 5s tumbling windows per (user, service, type); batch cap 20 [A-8]
internal/store            — modernc.org/sqlite DDL + queries (WAL, single-writer pattern); all persistence
internal/tgbot            — go-telegram/bot v1.25.0 client wrapper: long-poll update pump, sendMessage w/ MessageThreadID, CreateForumTopic, getChat, AnswerCallbackQuery; TG 429 handling
internal/menu             — /start /link /connect /setup /menu /status /help /undelivered handlers + inline keyboards + setup state machine
internal/admin            — CLI subcommands (service create/list/enable/disable/rotate, link/unlink, user list, events recent) [A-3]
internal/healthz          — GET /api/health (200/401 convention), GET /metrics (Prometheus)
internal/transport        — Dispatcher interface + per-chat queue + pacing + retry:
                            type Dispatcher interface {
                                Send(ctx context.Context, d Delivery) error   // d: ChatID, MessageThreadID, Text
                                Enqueue(d Delivery)                           // bounded per-chat queue
                            }
                            future channels = one more implementation of this interface (research §8)
```

### 5.1 Pinned third-party dependencies (go.mod, min versions)

| Module | Version | Why |
|--------|---------|-----|
| `github.com/go-telegram/bot` | **v1.25.0** | A-1: forum-topic support verified in source (`CreateForumTopic(Params{ChatID, Name, IconColor})` → `models.ForumTopic{MessageThreadID int}`; `SendMessageParams.MessageThreadID int` `json:"message_thread_id,omitempty"`); same library already proven in goVpnWork v1.19.0 / goRecomendarr v1.22.0. |
| `modernc.org/sqlite` | **v1.57.0** | A-2: pure-Go (no cgo), latest verified via Go module proxy 2026-09-01. |
| `github.com/prometheus/client_golang` | latest stable at build (≥ v1.23.0) | `/metrics`. |
| `gopkg.in/yaml.v3` | v3.0.1 | catalog. |

No other runtime deps. **No NATS client (D-2). No cgo. `CGO_ENABLED=0`.**

### 5.2 Process model
Single binary, single goroutine tree: HTTP server (ingest+healthz) · TG long-poll pump (go-telegram/bot `OnUpdates`) · coalesce timer scheduler · per-chat dispatcher workers (1 per active chat, bounded) · SQLite via a single writer goroutine (in-memory channel) + multiple readers. SQLite pragmas: `journal_mode=WAL; busy_timeout=5000; synchronous=NORMAL; foreign_keys=ON`.

---

## 6. Data model (full DDL, frozen)

SQLite, file at `DB_PATH` (default `/data/tgntfy.db`). `PRAGMA foreign_keys=ON`.

```sql
CREATE TABLE users (
  tg_user_id     INTEGER PRIMARY KEY,          -- Telegram user id (from updates)
  username       TEXT,                         -- nullable, best-effort from updates
  first_name     TEXT,
  delivery_mode  TEXT NOT NULL DEFAULT 'dm'
                 CHECK (delivery_mode IN ('dm','group')),   -- v1: group = production, dm = pre-setup smoke only (D-1)
  group_chat_id  INTEGER,                      -- set by /connect (UC-1)
  first_seen     TEXT NOT NULL,                -- RFC3339
  last_event_at  TEXT                          -- last event routed for this user
);

CREATE TABLE services (
  service       TEXT PRIMARY KEY,              -- 'goyoutube' | 'gomail' | 'gorecomendarr' | 'govpn' | future
  display_name  TEXT NOT NULL,                 -- forum topic name source
  token_hash    TEXT NOT NULL,                 -- sha256 hex of the 32B random token
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL
);

CREATE TABLE service_users (
  service    TEXT NOT NULL REFERENCES services(service),
  user_ref   TEXT NOT NULL,                    -- service-side identity (string)
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','unlinked')),
  linked_at  TEXT NOT NULL,
  PRIMARY KEY (service, user_ref)
);
-- one service identity belongs to exactly one TG user (UC-2 409 case).

CREATE TABLE subscriptions (
  user_id      INTEGER NOT NULL REFERENCES users(tg_user_id),
  service      TEXT NOT NULL REFERENCES services(service),
  event_types  TEXT,                           -- JSON array of type strings; NULL/'' = ALL types
  muted        INTEGER NOT NULL DEFAULT 0,     -- 1 = service-level mute (UC-4)
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (user_id, service)
);

CREATE TABLE events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id     TEXT NOT NULL,
  service      TEXT NOT NULL,
  user_ref     TEXT NOT NULL,
  type         TEXT NOT NULL,
  severity     TEXT NOT NULL CHECK (severity IN ('info','warn','error','success')),
  title        TEXT NOT NULL,
  text         TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',
  metadata     TEXT NOT NULL DEFAULT '{}',     -- raw JSON
  received_at  TEXT NOT NULL,
  UNIQUE (event_id)                            -- idempotency UQ (24 h retention)
);
CREATE INDEX idx_events_service_time ON events(service, received_at);

CREATE TABLE deliveries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL REFERENCES users(tg_user_id),
  event_id     TEXT NOT NULL,
  service      TEXT NOT NULL,
  type         TEXT NOT NULL,
  batch_size   INTEGER NOT NULL DEFAULT 1,     -- >1 = coalesced batch message
  tg_msg_id    INTEGER,                        -- set on success
  thread_id    INTEGER,                        -- message_thread_id used (0 = DM/general)
  status       TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','sent','delivered','failed')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_retry_at TEXT,
  last_err     TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX idx_deliveries_user_status ON deliveries(user_id, status);
CREATE INDEX idx_deliveries_retry ON deliveries(status, next_retry_at);

-- A-5: one topic per (user, service) inside the user's personal forum group (D-1).
CREATE TABLE group_topics (
  user_id            INTEGER NOT NULL REFERENCES users(tg_user_id),
  service            TEXT NOT NULL REFERENCES services(service),
  message_thread_id  INTEGER NOT NULL,
  created_at         TEXT NOT NULL,
  PRIMARY KEY (user_id, service)
);

-- A-6: one-time codes. link = identity binding (service-side, UC-2); connect = group binding (UC-1).
CREATE TABLE link_codes (
  code       TEXT PRIMARY KEY,                 -- 6 digits
  service    TEXT NOT NULL REFERENCES services(service),
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  expires_at TEXT NOT NULL,                    -- +10 min
  used_at    TEXT                              -- NULL = unused (single use)
);
CREATE TABLE connect_codes (
  code       TEXT PRIMARY KEY,                 -- 6 digits
  user_id    INTEGER NOT NULL REFERENCES users(tg_user_id),
  expires_at TEXT NOT NULL,                    -- +10 min
  used_at    TEXT
);

-- 24 h retention sweep (goroutine, hourly):
--   DELETE FROM events WHERE received_at < now()-24h
--   AND id NOT IN (SELECT event row ids referenced by failed deliveries);
-- pruned events may re-arrive as new (E-12, documented).
```

Indexes/unique constraints summary: `events.event_id` **UQ** (AC idempotency); `service_users(service, user_ref)` PK; `subscriptions(user_id, service)` PK; `group_topics(user_id, service)` PK; codes PK `code`, single-use enforced by `used_at` under the same transaction as the binding write.

---

## 7. Delivery (D-1 topology, binding)

### 7.1 Topology
- **Production mode = `group`** (D-1): the user's personal **private forum group**; one **topic per linked service** (`group_topics.message_thread_id`); every `sendMessage` for a service event carries `message_thread_id` of that topic.
- **`dm` mode = pre-setup smoke testing only**: a user who has `/start`'d but not finished `/setup` receives events in the DM. The moment `/connect` completes, mode flips to `group` and DM delivery for that user stops. There is no user-facing choice to stay on DM (frozen).

### 7.2 Queue mechanics (per-chat)
- One bounded queue per active chat (cap 200 deliveries; overflow → oldest `pending` demoted to `failed` with `last_err='queue_overflow'`, counted).
- **TG chat limit ~30 msg/min** enforced by the pacing loop: max 30 sends per chat per rolling 60 s (token bucket, capacity 30, refill 0.5/s). Global TG cap (~20 msg/s) is not a practical constraint at spluft scale but the pacing loop also caps 15 msg/s globally.
- `queue_depth` = sum of pending lengths across chat queues (metric, §14.2).

### 7.3 Retry / failure semantics (A-10, frozen)
- Backoff: attempt n (1-based) waits `min(5s × 2^(n-1), 5min)` → sequence **5s, 10s, 20s, 40s, 80s**; **max 5 attempts** → `status='failed'`.
- TG **429**: use `parameters.retry_after` as the wait; 429 does **not** count as a consumed attempt (only transport/5xx/400-class errors do).
- TG **403** (bot kicked/no rights): fail fast after attempt 1, `last_err` records 403, `/status` shows the "run /setup" hint (E-9).
- `/undelivered` `Retry all failed` resets `attempts=0, status='pending', next_retry_at=now` for failed rows (bounded: max 50 per click, rest stays).

### 7.4 `delivery_mode` handling in code (binding rule)
```
func route(user, svc, msg):
    if user.delivery_mode == 'group' and user.group_chat_id != nil:
        tid = group_topics(user, svc) or lazy-create-topic   // UC-3 5b, E-11
        enqueue(chat=user.group_chat_id, thread_id=tid, msg)
    else:
        enqueue(chat=user.tg_user_id, thread_id=0, msg)       // DM smoke only
```

---

## 8. Coalescing (A-8, binding)

- **Window: 5 s (default; env `COALESCE_WINDOW_MS`, default 5000).**
- **Key: (user_id, service, type)** — per research §3.4.
- **Mechanism:** first event of a key arms a 5 s timer; further events of the same key before fire **join the batch**; on fire (or batch cap **20**) → flush ONE message to the chat queue. A new event after the flush arms a fresh window.
- **AC math (binding):** 60 same-type events in 10 s → flush #1 at t≈5 s (≤30 ev), flush #2 at t≈10 s (≤30 ev) ⇒ **≤ 2 TG messages**.
- **Batch rendering (§8.5):** header line `⚡ <N> × <severity icon> <service display_name>: <type>` then one line per event (`<title>` [+ ` — <text>` truncated to 120 chars]), then one unique `url` line only if all events share the same url (else no url in the batch).
- `deliveries.batch_size` records N (audit). Single-event flushes render the standard message.

### 8.4 Drop rule (D-3)
Events whose `(service, type)` catalog entry has `drop: true` — or `job_progress` for any service (hard-coded allowlist-free rule: type == `job_progress` is always dropped in v1) — are dropped after auth/schema/idempotency checks: 200, counted `catalog_dropped_total`, logged at debug. Not stored in `events`.

### 8.5 Message rendering (A-9, frozen format)

Single event:
```
<icon> <service display_name> — <type>
<title>
<text>            ← only if non-empty
<url>             ← only if non-empty
```
Icons by severity: `✅ success` · `❌ error` · `⚠️ warn` · `ℹ️ info`.
Plain text (no parse mode). Hard truncation: total ≤ 3500 chars (TG limit 4096; head-keep with `… [truncated]`).

Batch (N > 1):
```
⚡ N × <icon> <service display_name>: <type>
• <title 1>
• <title 2>
…
<url>   ← only if shared
```

---

## 9. Rate limiting (A-7, frozen numbers)

- **Ingest (per service):** two in-memory sliding windows — 1 s window cap **30** (burst) AND 60 s window cap **100** (sustained). Either breach → `429` + `Retry-After: 1` + `events_rejected_total{reason="rate_limited"}`. Counters are per service id (not per token), so rotation does not reset limits.
- **Egress (per chat):** token bucket capacity **30**, refill **0.5/s** (≈30 msg/min, TG chat limit); global egress cap **15 msg/s**. The dispatcher never sends outside these bounds (binding: "TG chat limit ~30 msg/min respected by the dispatcher").
- **TG 429 response:** honored via `retry_after` (§7.3).

---

## 10. API surface (complete; nothing else on the HTTP listener)

### 10.1 `POST /v1/events`
See §3 (envelope, 400/401/409/413/429, 8 KB cap). Response `200`: `{"status":"accepted","queued":<n deliveries created>}`.

### 10.2 `POST /v1/link`
```json
{"service": "goyoutube", "user_ref": "17", "code": "482913"}
```
Auth: `X-Service-Token` of `service`. Responses: `200` `{"status":"linked","user_id":12345}` · `400` `{"error":"schema|service_unknown|code_invalid|code_mismatch"}` · `401` bad token · `409` `{"error":"already_linked"}` (user_ref bound to another TG user). Code single-use; binding + code consumption + subscription upsert in ONE transaction (E-13, E-14).

### 10.3 Admin = CLI (A-3, binding — NO HTTP admin endpoints)
`tgntfy admin <subcommand>` (runs against `DB_PATH`; safe to run while the server runs — WAL):
```
admin service create <id> --name <display>   → prints RAW token once (32B random, hex 64)
admin service list | disable <id> | enable <id>
admin service rotate <id>                     → new raw token printed once; old token invalid immediately
admin link <service> <user_ref> <tg_user_id|@username>
admin unlink <service> <user_ref>
admin user list                               → users, mode, group, linked services
admin events recent [-n 20]                   → latest events (audit)
```
Raw tokens never logged, never persisted (only sha256). Rotate: services must be updated before rotation (no grace period, v1 — documented for the follow-up adapter epics).

### 10.4 `GET /api/health` — spluft convention
- `200` `{"status":"ok"}` when `ADMIN_TOKEN` is not set OR the `X-Admin-Token` header matches.
- `401` when `ADMIN_TOKEN` is set and header missing/wrong.
- Health also checks: DB reachable (`SELECT 1`) and catalog loaded; on failure `503` (only reachable without admin token).

### 10.5 `GET /metrics` — Prometheus text format (§14.2). LAN-only assumption; no auth in v1 (documented; behind the same Portainer network).

---

## 11. TG self-serve commands (binding behavior; UI/UX detail §12)

| Command | Who | Behavior (summary) |
|---------|-----|--------------------|
| `/start` | any TG user | Upserts `users` row; auto-links govpn `user_ref='admin'` for the FIRST starter only (UC-2 2a); welcome + command list; if no links → push `/link` CTA; if `delivery_mode='dm'` → note "events go to this DM until you finish /setup". |
| `/link` | any TG user | UC-2: pick service (inline KB) → 6-digit `link_code` (10 min) → "enter it in the service's UI". |
| `/connect <code>` | in the personal group (UC-1) | Consumes a `connect_code`, verifies forum+admin+can_manage_topics+sender-admin, sets `delivery_mode='group'`, creates topics for all linked services, confirms. In DM (wrong place) → "Send /connect in your group." |
| `/setup` | in DM **or** in the group | Starts/resumes the step-by-step script (§11.3). In DM: steps 1–2 (instructions) + issue connect code. In the group: completes (needs a valid pending code for this user). |
| `/menu` | user | UC-4: two-level inline keyboards (service toggle → per-event-type toggles + mute all + back). |
| `/status` | user | UC-5: mode, group title, per-service last event, last 10 delivered, undelivered count (+ 403 hint if any). |
| `/undelivered` | user | UC-5: up to 20 failed rows + `🔁 Retry all failed` inline button (≤50 requeued). |
| `/help` | any | Static command list + setup summary + owner contact (Nikolay). |

Command recognition: `go-telegram/bot` `RegisterHandler(HandlerTypeMessageText, …, MatchTypePrefix)` **and** `MatchTypeRegexp` fallback for `/<cmd>@<bot_username>` (bot can be addressed by username in the group). Callback data format: `menu:<action>:<service>:<type>` (e.g. `svc_toggle`, `type_toggle`, `mute_all`, `back`, `retry_failed`, `setup_done`) — sender identity ALWAYS from the update's `From.ID`, never from callback data (anti-forgery).

### 11.1 `/link` state (no persistent state machine; one-shot per message)
Each `/link` + service-tap is stateless: the code row is the state. User abandons → code expires at TTL.

### 11.2 Code generation (A-6)
6 digits, `crypto/rand`, drawn from `[0-9]^6` via rejection sampling (no modulo bias); uniqueness retry (max 5) against existing unused codes; TTL exactly 10 min; expired rows pruned on read (lazy delete).

### 11.3 `/setup` state machine (D-1 ritual, binding step script)
States kept in memory per user (lost on restart → `/setup` restarts; harmless, codes are durable):
```
S0 idle
 └ /setup (DM) → S1: send STEP-1 text (create PRIVATE group, only you as member; add bot as admin WITH "Manage topics") + inline button [✅ I did it]
S1 → (user taps ✅ I did it, OR /setup//connect arrives from a forum chat of this user) → S2 verify:
      getChat → is_forum? bot admin with can_manage_topics? sender admin of chat?
      verify-ok  → if connect_code pending for this user: complete UC-1 step 4–5; else issue connect code → S3 text: "Send in the group: /connect 123456" (10 min)
      verify-fail → send the specific E-6/E-7/sender-not-admin text, stay S1 ("Fix it and tap ✅ I did it again."), nothing consumed
S3 → /connect <code> in the group → code check (valid, this user, unused) → bind group + create topics → S0 + confirmation in group
      code bad/expired → "code invalid — run /setup in the DM again" (stay S3)
```
The S2 "which group" resolution: the most recent chat update seen from this user where `is_forum=true` (bot saw them type in it); if none, ask the user to send any message (e.g. `/setup`) in the group first.

---

## 12. UI/UX — bot conversation flows (no web UI; TG menu UX frozen for QA verification)

### 12.1 Message templates (English)
- **Welcome (`/start`):**
  "Hi! I'm the tgNtfy gate. Your events from goYouTube, Mail, Recomendarr and VPN will arrive in YOUR personal Telegram forum group — one topic per service.\n1) Link a service: /link\n2) Create your group: /setup\nManage anything in /menu."
- **`/link` after service pick:** "Enter this code in <display_name> (Profile → Notifications):\n**482913**\n(expires in 10 min; single use)"
- **Link success:** "✅ Linked <display_name> (user <user_ref>). You'll receive its events — tune them in /menu."
- **Setup steps** (verbatim, the step-by-step script the bot prints):
  - Step 1: "📋 STEP 1/2 — Create a new **private** group in Telegram (any name, e.g. 'my tgntfy'). Members: only you. Then add **me** as **Administrator** with the permission **Manage topics** (group → Administrators → Edit → Manage topics ✓)."
  - Step 2: "📋 STEP 2/2 — Open your new group and send: /connect <code>\nYour code: **123456** (10 min). I'll create one topic per linked service."
  - Verify-fail texts: E-6 "I can see the group, but I'm missing the **Manage topics** admin right. Grant it (group → Administrators → Edit), then tap ✅ I did it again." · E-7 "This group doesn't have **Topics** enabled — create a forum-style group (group settings → Topics → on) or a new one, then tap ✅ I did it." · sender-not-admin "Only an **admin of the group** can finish setup."
  - Success: "✅ Setup complete in <group title>.\nTopics: goYouTube · Mail · Recomendarr · VPN\nEvents will appear in their topics. Per-service mute: /menu."
- **`/menu` level 1:** "📡 Your services — tap to manage:" + keyboard rows `✅ goYouTube` / `✅ Mail` / `➕ link more…` (rows for linked services; `➕` opens UC-2).
- **`/menu` level 2 (per service):** "goYouTube — choose event types:" rows `✅ job_completed` `✅ job_failed` `⬜ job_cancelled` + `🔕 Mute all` + `⬅️ Back`.
- **`/status`:** "📊 Mode: **group** (<title>)\n\ngoYouTube: last <type> — <title> · <time>\nMail: last new-mail — <subject> · <time>\n\nLast delivered:\n1. <time> goYouTube job_completed <title>\n…\nUndelivered: N" (+ when N>0: "/undelivered to see and retry." + 403-hint variant E-9).
- **`/undelivered`:** "❌ Failed deliveries:\n#12 VPN vpn_disconnected — <last_err> (5/5)\n…" + `🔁 Retry all failed` → "🔁 Requeued 2."
- **Errors:** API errors per §3.3 (JSON). Bot-side errors always actionable + next step (patterns above).
- **All keyboards** use `InlineKeyboardMarkup`; callbacks answered silently (`AnswerCallbackQuery` no alert), except unexpected/failure ones → `ShowAlert` with one-line reason.

### 12.2 UX invariants (QA checklist source)
1. No state is lost that a user cannot re-trigger: every flow re-enters via a command.
2. Codes are always shown with TTL and single-use note.
3. Every error names the fix (no dead ends).
4. The bot NEVER sends a user's event text into a chat other than their own (isolation, §13).
5. Bot commands list registered on startup via `SetMyCommands` (start, link, setup, connect, menu, status, undelivered, help).

---

## 13. Security (binding)

1. **Service tokens:** 32 B from `crypto/rand`, hex(64), stored **sha256-hashed only** (`services.token_hash`); constant-time compare; raw token printed exactly once at `admin service create/rotate`; never in logs/DB/repo; rotation = instant invalidation (A-3).
2. **TG bot token:** **env-only** (`TG_BOT_TOKEN`); repo ships `.env.example` placeholders; server refuses to boot without it (admin CLI subcommands do not need it).
3. **Isolation guarantee (first-class):** a delivery for `user_id U` is constructed ONLY from U's rows (`users`, `subscriptions`, `group_topics`) and the events routed to U in §3.4; the dispatcher's queue is per-chat keyed by the destination chat id resolved at route time; **there is no code path that writes event content to a chat not owned by the routed user.** Enforced by: (a) route→enqueue always pairs `(user, destination)` in one function, (b) integration test ISO-1 (§15.2), (c) `deliveries.thread_id` recorded per row for audit. This is THE differentiator vs research option A and must never be relaxed.
4. **Inputs:** schema-validated envelope (length caps on all strings); metadata size bounded by the 8 KB body cap; no HTML/Markdown parsing of service-provided text (A-9); callback sender identity from the update, never from callback payload.
5. **Codes:** 6 digits is acceptable given 10-min TTL + single-use + service-token-gated use (link codes are only redeemable by the owning service); connect codes additionally require group-admin sender.
6. **Repo hygiene:** `.env.example` with placeholders only; `.gitignore` covers `.env`, `*.db`; no secrets in images (no build args carrying secrets).

---

## 14. Observability + deployment

### 14.1 Logging — `log/slog` (JSON in prod, text in dev via `LOG_FORMAT`)
Structured fields on every pipeline stage: `event_id`, `service`, `user_ref`, `user_id`, `chat_id`, `thread_id`, `attempt`, `err`. Token values redacted (`token=***`). Levels: debug (coalesce internals, dropped progress), info (accepted, linked, setup steps), warn (unknown type, 4xx), error (5xx, delivery failure).

### 14.2 Metrics — `GET /metrics` (Prometheus)
| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `events_in_total` | counter | `service` | accepted events (post-auth/schema) |
| `events_rejected_total` | counter | `service`, `reason` | 400/401/409/413/429 counts |
| `events_unrouted_total` | counter | `service` | accepted, no linked users (E-10) |
| `catalog_unknown_total` | counter | `service`, `type` | A-4 unknown types |
| `catalog_dropped_total` | counter | `service`, `type` | D-3 drops |
| `deliveries_ok_total` | counter | `service` | successful TG sends |
| `deliveries_fail_total` | counter | `service` | exhausted/final failures |
| `deliveries_429_total` | counter | — | TG flood-limit deferrals |
| `queue_depth` | gauge | — | total pending deliveries across chat queues (AC metric) |
| `coalesce_batches_total` | counter | — | flushes (histogram `coalesce_batch_size` for N) |
| `catalog_reload_fail_total` | counter | — | SIGHUP failures |

### 14.3 Dockerfile (frozen pattern; classic builder — NO `# syntax`/`#include`)
```dockerfile
FROM golang:1.25.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tgntfy ./cmd/tgntfy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tgntfy /usr/local/bin/tgntfy
COPY config/events.yaml /etc/tgntfy/events.yaml
ENV LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/usr/local/bin/tgntfy"]
```
Target image size **~15 MB** (AC; distroless static). No cgo (A-2). (Deploy-stage note: Portainer classic-builder host needs this Dockerfile inlined verbatim, `networkmode=host` + `extrahosts` for proxy.golang.org/sum.golang.org/storage.googleapis.com per dev-pipeline deploy rules.)

### 14.4 `.env` surface (env vars; all optional except TG_BOT_TOKEN; `.env.example` ships placeholders)
| Var | Default | Meaning |
|-----|---------|---------|
| `TG_BOT_TOKEN` | — (**required** for server) | BotFather token, env-only (§13). |
| `LISTEN_ADDR` | `:8080` | HTTP listener (ingest+healthz+metrics). |
| `DB_PATH` | `/data/tgntfy.db` | SQLite file (volume `/data`). |
| `CATALOG_PATH` | `/etc/tgntfy/events.yaml` | catalog file; SIGHUP reload. |
| `COALESCE_WINDOW_MS` | `5000` | §8 window. |
| `ADMIN_TOKEN` | (empty) | if set, `/api/health` requires `X-Admin-Token` (200/401 convention). |
| `LOG_FORMAT` | `json` | `json` \| `text`. |
| `LOG_LEVEL` | `info` | slog level. |

**Portainer expectations (ep3, NEW stack):** 1 service `tgntfy`; ports `8080:8080`; volume `tgntfy-data:/data`; env from the table above; image `ghcr.io/spluft/tgntfy:<tag>` (~15 MB). New stack, new volume — never reuse another service's volume.

---

## 15. Acceptance criteria (verbatim from epic t_352cddfe) + QA test plan

### 15.1 AC (verbatim)
1. Full test suite green at acceptance (unit + integration; Go, `go test`).
2. Isolation: event of user-A is never delivered to user-B group (two-test-user integration test).
3. Idempotency: duplicate `event_id` within 24 h not re-delivered (test).
4. Coalesce: 60 same-type events in 10 s produce ≤ 2 messages (test).
5. Forum delivery: topic per service created via `createForumTopic`, events carry `message_thread_id` (integration test with mocked/recorded Bot API; live E2E below).
6. E2E live: gate deployed to Portainer ep3 (192.168.1.200, NEW stack) from the DEFAULT branch head; curl POST /v1/events with valid service token produces a real TG message in Nikolay's forum group in the correct service topic.
7. `/api/health` returns 200 from the host after deploy; image ~15 MB; TG bot token + service tokens only in env, `.env.example` placeholders in repo.
8. Docs: README (setup walkthrough incl. /setup ritual), `.env.example`, `DEPLOYMENT.md`.

### 15.2 Test plan outline (every AC mapped; qa writes, integrate gates on)
Test infra: `httptest` for ingest; **Bot-API stub** — an `httptest` server impersonating `api.telegram.org/bot<token>` that records `sendMessage`/`createForumTopic`/`getChat` calls and returns scripted results (AC5: "mocked/recorded Bot API"); temp SQLite per test; `COALESCE_WINDOW_MS` overridable + fake-clock/manual flush for determinism.

| ID | AC | Test | Key assertions |
|----|----|------|----------------|
| ISO-1 | 2 | **Isolation:** two users U_A (group G_A, topic T_A/goyoutube), U_B (G_B, T_B/goyoutube); link `service_users('goyoutube','1'→U_A)` and `('goyoutube','2'→U_B)`; ingest event `user_ref=1`; flush | Bot-API stub recorded **exactly one** `sendMessage` with `chat_id=G_A`, `message_thread_id=T_A`; **zero** calls touching G_B/T_B; `deliveries` rows: 1 (user A). Repeat mirrored (user_ref=2 → only G_B). |
| IDP-1 | 3 | **Idempotency:** valid event `event_id=X` accepted (200, queued, 1 delivery); re-POST same body | second response **409** `duplicate`; delivery count unchanged; no new `deliveries` row; no TG send recorded. Variant: after simulated retention prune, same `event_id` → 200 re-delivered (E-12 documented). |
| COA-1 | 4 | **Coalesce 60/10s:** window 5 s, inject 60 same `(user,service,type)` events spread over 10 s | flush count == 2; `sendMessage` count for that chat/topic == **2**; `deliveries.batch_size` values sum to 60; each batch ≤ 20 (cap). |
| FOR-1 | 5 | **Forum topic creation:** user completes UC-1 against the Bot-API stub (getChat → forum+admin OK; createForumTopic → scripted `ForumTopic{MessageThreadID: 42, 43}`); then ingest 1 event per linked service | `createForumTopic` called once per linked service with `Name=display_name`; `group_topics` rows persisted; subsequent `sendMessage` calls ALL carry the matching `message_thread_id`; lazy path: delete one `group_topics` row, ingest → topic recreated (E-11). |
| FOR-2 | 5 | **Setup failure modes:** stub getChat returns (a) bot not admin, (b) no can_manage_topics, (c) non-forum chat, (d) sender not admin | each yields the §12.1 verify-fail text, `delivery_mode` stays `dm`, code NOT consumed (reusable while valid); E-6/E-7/E-14 covered. |
| RATE-1 | 1 (suite) | **Rate limit:** 31st event within 1 s for a service; 101st within 60 s | 31st → **429** + `Retry-After: 1`; 101st → 429; after window slides → 200. Per-service independence: service B unaffected. |
| ERR-1 | 1 (suite) | **Error matrix:** E-1 (401), E-2 (400 per field), E-3 (409), E-4 (413 with 8193-byte body), E-13 (code mismatch), E-14 (double connect) | exact status + `error` code per §3.3 / §10 / §11. |
| RET-1 | 1 (suite) | **Retry/backoff:** stub `sendMessage` fails with 500 for first 2 calls, then 429 `retry_after=2`, then 200 | attempts sequence honors 5s/10s/20s… (injected clock); 429 does not consume an attempt; final `status='delivered'`; `deliveries_429_total`=1. Exhaustion variant: always-500 → after 5 attempts `status='failed'`, visible via `/undelivered` → retry button resets and redelivers. |
| LINK-1 | 1 (suite) | **Link flow:** `/link` (fake user update) → code row; `POST /v1/link` with service token | binding + subscription created in one transaction; code consumed (second POST → 400 `code_invalid`); wrong service → 400 `code_mismatch`; already-linked user_ref → 409. |
| SEC-1 | 1 (suite) | **Token hygiene:** `admin service create` prints raw token once; DB holds only sha256; ingest with wrong token → 401; logs contain no raw token | byte-compare sha256(raw)==token_hash; regex-scan captured logs for the raw token (absent). |
| HZ-1 | 7 | **Health:** no ADMIN_TOKEN → 200; with ADMIN_TOKEN set: no header → 401, correct header → 200; DB unreachable (simulated) → 503 | exact codes + body. |
| DOC-1 | 8 | **Docs presence:** repo root has README.md (contains "/setup" walkthrough section), .env.example (placeholders only, no real token), DEPLOYMENT.md | static file + keyword assertions in the QA suite. |
| E2E-1 | 6 | **Live E2E (deploy stage, NOT unit):** deploy NEW Portainer stack ep3 from default-branch head; `curl -H "X-Service-Token: …" -d @sample.json :8080/v1/events` | real message appears in Nikolay's forum group in the correct service topic (verified with owner); `/api/health` 200 from host. **Blocks on owner bot token** (epic note: pipeline must kanban_block needs_input if missing — never ship an unverified E2E). |
| IMG-1 | 7 | **Image size:** `docker image inspect` size | ≤ ~15 MB total (report actual). |

---

## 16. Out of scope (v1, frozen — research §8 + epic)

kidsEdu integration (D-4, contract reserved in research §3.3) · NATS ingest (D-2) · Web UI/PWA · ntfy/Gotify transports (Dispatcher interface is the reserved extension point) · per-service custom message templates · multi-instance/sharding · DM as a user-selectable production mode (D-1) · token rotation grace periods (A-3) · `job_progress`/heartbeat events (D-3) · TLS termination (reverse-proxy concern, not the gate).

## 17. Edge cases register (design-level; QA picks what fits §15.2)
E-1…E-15 (§2 matrix) + retention/pruning interplay (E-12), queue overflow (§7.2), SIGHUP bad catalog (keep-old), bot restart mid-coalesce (in-memory windows lost → at worst one extra message; events are durable in `events`, unflushed `deliveries status='pending'` rows are re-driven at startup), clock skew (monotonic timers, RFC3339 storage).

## 18. Definition of done for the impl epic
`go build` clean · `go vet` clean · `go test ./...` green (§15.2 all) · Dockerfile builds to ~15 MB · docs per AC-8 · E2E per AC-6 with the owner's bot token · frozen D-1..D-4 and A-1..A-10 honored (acceptance re-checks this spec section by section).
