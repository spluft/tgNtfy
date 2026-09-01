# tgNtfy

The unified **notification gate** for the spluft services. Every service posts its events
to one HTTP endpoint with one JSON envelope and a per-service token; tgNtfy routes
each event to the right Telegram user and delivers it as a message in a **personal
private forum group** — one **topic per service**.

- HTTP ingest: `POST /v1/events` (frozen contract, zero gate changes when you add a
  service) and `POST /v1/link`.
- **Multi-user routing**: each Telegram user self-serves which services and which event
  types they want, per-service mute, and their own delivery destination.
- **Delivery**: a private Telegram *forum* group per user, one topic per service. Full
  privacy (no shared chat), native per-service mute/silence via Telegram topics.
- **Accountability**: idempotent ingest, per-service rate limiting, 5-second coalescing
  (bursts become one message), retried delivery with exponential backoff, and audit via
  `/status`, `/undelivered` and Prometheus metrics.

Languages: Go 1.25, SQLite via `modernc.org/sqlite` (pure Go, no cgo), and the
`go-telegram/bot` v1.25.0 Telegram client. Delivery topology, DDL and API contract are
frozen in `docs/epics/tgnfyt-t_352cddfe/SPEC.md` (binding).

---

## v1.1 — service-agnostic lazy topic creation

Where this README describes v1 behaviour, **v1.1 changes three things** (full spec:
`docs/epics/tgnfyt-t_a86c33cd/SPEC.md`):

- **The service supplies the topic name.** `POST /v1/link` now carries an **optional**
  `display_name` field. When present and non-blank it is the single source of the topic's
  display name (sanitised: whitespace-collapsed, control/format chars stripped, clamped to
  128 code points; empty → service-id fallback) and it always wins, even for an already-known
  service. `services.display_name` becomes authoritative — no new columns, no migration.
- **Topics are created lazily, not up front.** Exactly **two** creation points: at link time
  (only if the user is already in `delivery_mode=group`, via the idempotent
  `store.EnsureTopic`) and as a first-event fallback. There is **no static-catalog-driven
  creation**. `/connect` creates **zero** topics (the v1 `createTopicsFor` was removed) and
  instead clears stale `group_topics` rows.
- **The catalog is now optional.** `config/events.yaml` (service→event-type map + severity /
  drop hints) is no longer required to boot: the gate starts, ingests, renders and delivers
  with an empty or **absent** `events.yaml` (a missing file is a normal, debug-logged state).
  `/link`, `/menu` and rendering are store-driven, not catalog-driven.

---

## Architecture overview

```
                spluft services
          (goyoutube, gomail, gorecomendarr, govpn)
                        |
             POST /v1/events        X-Service-Token: <raw>
                        |
                        v
                ┌─────────────────┐
                │  ingest         │  auth (token → service), schema+size checks,
                │  internal/ingest│  rate limit (30/s burst + 100/min per service),
                │                 │  idempotency (event_id UQ, 24h), routing,
                │                 │  drops (job_progress), coalesce trigger
                └─────────────────┘
                        |
                coalesce 5s (per user+service+type), cap 20/batch
                        |
                        v
              per-chat delivery queue  (TokenBucket pacing ≈30 msg/min)
                        |
                        v
              Telegram transport  (sendMessage with message_thread_id, backoff retries)
                        |
              YOUR private forum group ── topic per service
```

Main packages (all under `internal/`):

- `ingest` — HTTP surface: `POST /v1/events` and `POST /v1/link`; token auth,
  schema/size validation, rate limiting, idempotency, routing, coalescing, rendering.
- `catalog` — loads/validates/reloads `config/events.yaml` (service→event-type map
  and display names). Re-read on `SIGHUP` — extend services without a re-release.
- `store` — modernc.org/sqlite: DDL + queries (WAL). Users, services, service_users,
  subscriptions, events, deliveries, group_topics, link_codes, connect_codes.
- `coalesce` — 5 s tumbling windows per `(user, service, type)`, batch cap 20.
- `limit` — per-service sliding-window rate limiter.
- `transport` — Dispatcher: bounded per-chat queue, token-bucket pacing, and retry
  with exponential backoff (5s × 2^n, capped 5 min, max 5 attempts).
- `tgbot` — go-telegram/bot wrapper: long-poll update pump, sendMessage (with
  `message_thread_id`), CreateForumTopic, getChat/getChatMember, SetMyCommands.
- `menu` — `/start /link /connect /setup /menu /status /undelivered /help` + inline
  keyboards + the `/setup` state machine.
- `admin` — admin CLI (`tgntfy admin …`): issue/rotate service tokens, manual link.
- `healthz` — `GET /api/health` (200/401/503) and `GET /metrics` (Prometheus).

---

## Quick start (single binary)

```sh
# build
go build -o bin/tgntfy ./cmd/tgntfy

# configure
cp .env.example .env
# set TG_BOT_TOKEN to a real BotFather token in that file (or export it)

# run the server (loads the env, or export the values)
export $(cat .env | xargs)
./bin/tgntfy
```

The binary has two modes:

- default — the **server** (requires `TG_BOT_TOKEN`).
- `tgntfy admin <db-path> <subcommand> …` — the **admin CLI** (does **not** need a
  bot token; runs against the same SQLite file, safe while the server runs — WAL).

---

## Full setup walkthrough (the `/setup` forum ritual)

This is the frozen delivery model (SPEC decision D-1): a **personal private forum group**
per user, one **topic per service**. The gate walks you through it.

### 1. Link a service to your Telegram account

```
/start
```

The bot upserts you as a user and shows the command list. Link a service:

```
/link
```

The bot shows an inline keyboard of registered services. Pick one (e.g. **goYouTube**).
The bot creates a 6-digit one-time **link code** (10-minute TTL) and tells you:

> Enter this code in goYouTube (Profile → Notifications): **482913** (expires in 10 min; single use)

You enter that code in the service's own UI. The service (holding its `X-Service-Token`)
calls `POST /v1/link` with the code to bind your identity. The gate replies:

> ✅ Linked goYouTube (user 17). You'll receive its events — tune them in /menu.

Notes:
- **govpn** has no per-user web flow; on first `/start` the gate auto-links
  `service_users('govpn','admin', <first starter>)`. Only the first starter can claim it
  (a second attempt → `409 already_linked`). ADMIN can also link/unlink manually
  (see "Admin" below).
- Every linked service also gets a `subscriptions` row with **all** event types enabled by
  default — tune in `/menu`.

### 2. Create your personal forum group (`/setup`)

In the DM, run:

```
/setup
```

The bot starts a **step-by-step script**:

- **STEP 1/2** — *Create a new **private** group in Telegram (any name, e.g. "my
  tgntfy"). Members: only you. Then add **me** as **Administrator** with the permission
  **Manage topics** (group → Administrators → Edit → Manage topics ✓).* Have it and press
  the inline **✅ I did it** button.
- **STEP 2/2** — the bot verifies the group and issues a fresh 6-digit **connect code**
  (10 min), then prints:

  > Open your new group and send:
  > `/connect 123456`
  > Your code: **123456** (10 min). Topics appear when you link services or when the first event arrives.

### 3. Complete the binding (`/connect <code>`)

Open your **private group** (not the DM) and send:

```
/connect 123456
```

(or `/connect@tgntfy_bot 123456`). The gate checks:

1. the code is valid, unexpired, unused, and issued to you;
2. the chat is a **forum** (topics enabled);
3. the bot is an **admin** with **Manage topics** allowed;
4. you are an **admin** of that group.

On success it sets your delivery mode to `group`, clears any stale `group_topics`
rows, and confirms (topics are created lazily — at link time or first event, not here):

> ✅ Setup complete in <group title>.
> Topics will appear as you link services and events arrive. Per-service mute: /menu.

If something is wrong, the bot names the fix (grant **Manage topics**; enable Topics;
only a group **admin** can finish) and lets you retry without burning the code.

> The DM receives events **only** before you finish `/setup` (smoke-test path). Once
> `/connect` completes, delivery moves to your group permanently.

---

## Self-serve command reference

| Command | Who | What it does |
|---------|-----|--------------|
| `/start` | anyone | Upserts you; auto-links govpn `admin` for the first starter; welcome + commands. |
| `/link` | anyone | Link a service to your account (service pick → 6-digit code → enter it in the service's UI). |
| `/setup` | DM **or** group | Start/resume the step-by-step forum-group ritual (see above). |
| `/connect <code>` | in your forum group | Bind this group (verifies forum + bot admin + group admin); sets `delivery_mode=group`; clears stale `group_topics` rows. v1.1: creates zero topics (topics are created lazily at link or first-event). |
| `/menu` | user | Two-level keyboard: service toggle (mute all) → per-event-type toggles. |
| `/status` | user | Delivery mode + group; per-service last event; last 10 delivered; undelivered count. |
| `/undelivered` | user | Failed deliveries (up to 20) + **🔁 Retry all failed** button. |
| `/help` | anyone | Static command list + setup summary. |

---

## How a service posts an event

`POST /v1/events` — `Content-Type: application/json`, header `X-Service-Token: <raw-token>`.

Envelope schema (frozen in SPEC §3.2):

| Field | Type | Required | Rules |
|-------|------|----------|-------|
| `v` | int | yes | must be `1`. |
| `event_id` | string, 1..255 | yes | **Idempotency key**; recommended `<service>:<kind>:<id>:<type>:<RFC3339>`. Unique within a 24 h window. |
| `service` | string, 1..32 | yes | Registered service id. |
| `user_ref` | string | yes | Identity of the end-user *in that service* (string); service-level events use `"admin"` (e.g. govpn). |
| `type` | string, 1..64 | yes | Event type from the service's catalog entry. Unknown types are **accepted** and counted (`catalog_unknown_total`). |
| `severity` | string | yes | `info` \| `warn` \| `error` \| `success`. |
| `title` | string, 1..200 | yes | Short headline. |
| `text` | string, 0..500 | no | Detail line. |
| `url` | string, 0..500 | no | Optional link (last line, plain). |
| `metadata` | object | no | Free-form; the gate does not interpret it. |

**Body size cap: 8192 bytes** (larger → 413).

Example:

```sh
curl -i -X POST http://localhost:8080/v1/events \
  -H 'Content-Type: application/json' \
  -H 'X-Service-Token: <raw-token>' \
  -d '{
    "v": 1,
    "event_id": "goyoutube:job:4821:completed:2026-09-01T11:22:33Z",
    "service": "goyoutube",
    "user_ref": "17",
    "type": "job_completed",
    "severity": "success",
    "title": "Video downloaded",
    "text": "That Italy from Pinterest — downloaded",
    "url": "https://goyt.spluft.ru/#/queue/4821",
    "metadata": {"job_id": 4821, "channel": "UyeF5cGujes"}
  }'
```

Response `200`: `{"status":"accepted","queued":<n deliveries created>}`.

### Status codes (frozen)

| Code | Body `error` | Meaning |
|------|--------------|---------|
| 200 | — | accepted (+queued); or linked |
| 400 | `schema` / `service_unknown` / `code_invalid` / `code_mismatch` | schema/identity error (`detail` names the field) |
| 401 | `unauthorized` | bad or missing `X-Service-Token` |
| 409 | `duplicate` / `already_linked` | duplicate `event_id` within 24 h; or user_ref already linked to another user |
| 413 | `too_large` | body > 8 KB |
| 429 | `rate_limited` (+ `Retry-After: 1`) | per-service rate limit exceeded |

### `POST /v1/link`

Called *by a service* to redeem the user's link code:

```json
{"service": "goyoutube", "user_ref": "17", "code": "482913", "display_name": "goYouTube"}
```

`display_name` is **optional** (v1.1). When present and non-blank it becomes the topic
display name (sanitised, 128-code-point clamp) and always wins over any previously stored
name. Omit it for v1-compatible calls. Response `200`
`{"status":"linked","user_id":12345,"topic_created":true}` — `topic_created` is `true` if a
forum topic row now exists for this user+service (created just now, or pre-existing on a
re-link); `false` in DM mode or if TG creation failed (first-event fallback retries).
Binding + code consumption + subscription upsert happen in one transaction.

---

## Issue / rotate service tokens (admin)

Admin surface is the **CLI** (SPEC A-3) — there are **no** HTTP admin endpoints.
Run it against the same SQLite file while the server runs (WAL is safe):

```sh
# register a service; prints the RAW token exactly once
docker exec <container> tgntfy admin /data/tgntfy.db service create goyoutube --name "goYouTube"
# 64-hex-char token printed to stdout only; only its sha256 is stored.

docker exec <container> tgntfy admin /data/tgntfy.db service list
docker exec <container> tgntfy admin /data/tgntfy.db service enable  <service>
docker exec <container> tgntfy admin /data/tgntfy.db service disable <service>

# rotate -> new raw token printed once; the old token stops working IMMEDIATELY (no grace
# period in v1). Update the service to the new token before/right after rotating.
docker exec <container> tgntfy admin /data/tgntfy.db service rotate <service>

# manual identity link/unlink (services without a web-UI hook)
docker exec <container> tgntfy admin /data/tgntfy.db link   <service> <user_ref> <tg_user_id>
docker exec <container> tgntfy admin /data/tgntfy.db unlink <service> <user_ref>

# audit
docker exec <container> tgntfy admin /data/tgntfy.db user list
docker exec <container> tgntfy admin /data/tgntfy.db events recent

# locally (no container): tgntfy admin <db-path> service list
```

Raw tokens are **never** persisted or logged — only sha256 hashes are kept, and logs print
`token=***`. Keep each raw token out of the repo.

---

## Observability

- `GET /api/health` → `200 {"status":"ok"}` when healthy (DB reachable via `SELECT 1`).
  If `ADMIN_TOKEN` is set it requires a matching `X-Admin-Token` header, otherwise `401`;
  DB failure → `503`.
- `GET /metrics` → Prometheus text format (counters: `events_in_total`,
  `events_rejected_total`, `events_unrouted_total`, `catalog_unknown_total`,
  `catalog_dropped_total`, `deliveries_ok_total`, `deliveries_fail_total`,
  `deliveries_429_total`, `queue_depth`, `coalesce_batches_total`, …).
- Structured logs (`log/slog`): JSON in prod, text via `LOG_FORMAT=text`; levels via
  `LOG_LEVEL`.

---

## Deploying

See **[DEPLOYMENT.md](DEPLOYMENT.md)** — Docker build, GHCR push, and the Portainer
stack (endpoint 3 / 192.168.1.200), including the host build constraints.

## Repository layout

- `cmd/tgntfy/main.go` — env wiring, mode dispatch (`admin` vs server), graceful shutdown,
  SIGHUP catalog reload.
- `internal/…` — see *Architecture overview*.
- `config/events.yaml` — v1 catalog entries (goyoutube, gomail, gorecomendarr, govpn).
- `.env.example` — sample environment (placeholders only).
- `docs/epics/` — research + binding SPEC for this epic.
- `Dockerfile` — multi-stage static build (see DEPLOYMENT.md).

> **Env-var note:** this service listens on **`LISTEN_ADDR`** (default `:8080`). (The
> card that spawned this doc mentioned `HTTP_ADDR`; the shipped code and SPEC use
> `LISTEN_ADDR`, and `.env.example` documents it.)
