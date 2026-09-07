# tgNtfy v1.1 — Binding Implementation SPEC
## Service-agnostic lazy topic creation + optional catalog + 2 acceptance-FAIL fixes

> **Doc status (labeled 2026-09-05, epic t_2d992300 docs reorg): BINDING (current).**
> Last verified 2026-09-02; shipped to main (tip `48d6964`). Amends the v1 spec
> (`docs/epics/tgnfyt-t_352cddfe/SPEC.md`) where stated. See `docs/README.md` for the index.

**Epic:** t_a86c33cd — tgNtfy v1.1: service-agnostic topic creation (service supplies name; topic created at registration)
**Builds on:** v1 feature branch `feature/tgnfyt-t_352cddfe-gate-v1` (tip `056b774`) — this spec amends v1 SPEC `docs/epics/tgnfyt-t_352cddfe/SPEC.md` where stated; all v1 bindings not amended here remain in force.
**Project:** tgNtfy (`github.com/spluft/tgNtfy`) · board `spluft` · default branch `main`
**Status:** BINDING for backend / qa / integrate / acceptance. Any deviation requires a spec amendment, not a worker decision.
**Language:** English (user convention).

---

## 0. Architect's binding decisions (made in this spec; workers must follow)

| # | Decision | Consequence |
|---|----------|-------------|
| **V-1** | The **service** is the single source of a topic's display name, supplied in `POST /v1/link` via the **`display_name`** field (chosen over `service_name`: the store column is already `services.display_name` — naming consistency). The service **id** remains the ingest token's identity (`services.service`). | §3 link schema. |
| **V-2** | `display_name` in the link body is **OPTIONAL** (back-compat: v1 bodies carried only `service`/`user_ref`/`code`). When present and non-blank after sanitization it **always wins** — for both new and already-known services (updates `services.display_name` in the same transaction as the binding). When absent, the existing row value (set by `admin service create <id> --name …`) is kept. Rationale: a link must never fail because a client forgot a cosmetic field; the admin-created default always exists (`--name` is required by the CLI). | §3.2, §5. |
| **V-3** | Topic creation is **registration-triggered + first-event-fallback only**. **No code path reads the catalog to decide whether/what to create.** At `POST /v1/link` success: if the TG user is already in `delivery_mode='group'` with `group_chat_id` set, the (user, service) topic is created immediately (if no `group_topics` row exists) with the stored display name. If the user is still in `dm` mode, **no topic is created at link time** — the user has no forum group yet; the first event after `/connect` creates it (first-event fallback, §4). A link that creates no topic is CORRECT, not a failure. | §4. |
| **V-4** | **Idempotency:** re-linking the same `(user, service)` reuses the existing `group_topics` row / existing topic — **never a duplicate topic**. Topic creation goes through `store.GetOrCreateTopic` (SELECT row → return if present; else create + upsert `ON CONFLICT DO NOTHING`). Re-link by the SAME user → `200` (idempotent refresh, v1 behavior kept); link where `user_ref` is bound to a DIFFERENT TG user → `409 already_linked` (kept). | §4.2, §3.3. |
| **V-5** | The event-path topic resolver is fixed to: **`group_topics` row → (else) create using `services.display_name`** (sanitized; fallback: the service id). The v1 latent bug — `main.go`'s `topicResolver` called `CreateForumTopic` on **every** event without checking `group_topics` — is eliminated by routing the event path through `store.GetOrCreateTopic`. The resolver NEVER consults the catalog. | §4.3. |
| **V-6** | **Catalog (`events.yaml`) is demoted to OPTIONAL severity hints only.** It is never required for boot, auth, ingest, routing, rendering, or topic creation. The gate boots and fully operates with an empty or absent catalog (startup warn-and-continue becomes the norm; the `EnsureRegistered`-from-catalog loop in `main.go` is REMOVED). The catalog still provides: per-`(service, type)` default severity (menu/docs hint only) and the `drop: true` flag (ingest drop rule). SIGHUP reload stays. | §6. |
| **V-7** | **Sanitization (exact rule):** `SanitizeTopicName(s)`: (1) replace every Unicode whitespace run with a single ASCII space; (2) trim; (3) strip control/format chars (Unicode categories Cc/Cf — tab/newline already collapsed in step 1); (4) clamp to **128 code points** (Telegram `createForumTopic.name` limit is 1–128 chars); (5) if the result is empty → the caller falls back to the **service id** (1..32, always valid). Emojis and CJK pass through (Telegram accepts arbitrary Unicode in topic names). Clamping does NOT append an ellipsis. | §5. |
| **V-8** | **Coalesce AC corrected (owner-approved):** v1 AC "60 same-type events in 10 s → ≤ 2 messages" is unsatisfiable because v1 SPEC A-8 mandates batch cap 20 → floor for 60 events is `ceil(60/20) = 3`. New AC: **≤ 3 messages for 60 same-type events in 10 s** under the production config (5 s window, cap 20). QA measured exactly **3** in v1 acceptance (card t_4b15682c); the v1.1 test asserts `n ≤ 3` plus `Σ batch_size == 60` and `each batch ≤ 20`. | §7, §11 (COA-1). |
| **V-9** | `/connect` (finishSetup) **no longer pre-creates topics** (v1 `createTopicsFor` removed). On group binding it **DELETES the user's stale `group_topics` rows** (a new group means old `message_thread_id`s point at the old chat — delivering there would 403); topics then appear lazily per linked service on the first event (or at link, per V-3). | §4.1, edge E-16. |
| **V-10** | `/link` keyboard and `/menu` are **store-driven, not catalog-driven**: `/link` enumerates **enabled services from the `services` table** (display name from the same row); `/menu` level-1 lists linked services (display name from the store) + one "link another service" entry; level-2 shows per-type toggles **only for types the optional catalog declares** for that service (else: no type rows, note "all types on"). Rendering (`RenderMessage`/`RenderBatch`) uses the **store display name**, never the catalog. | §8. |
| **V-11** | v1's `welcomeText` hard-coded the four service names ("goYouTube, Mail, Recomendarr and VPN") — replaced with a service-agnostic text (the gate must not know services up front). `/setup` step-2 text drops "I'll create one topic per linked service" → "Your linked services will get a topic each." | §8.2. |
| **V-12** | **Renaming an existing TG topic is out of scope** (Bot API `renameForumTopic` exists but is not used). If a re-link updates `display_name` while a topic already exists, the stored name changes but the existing topic keeps its Telegram name; only NEW topics use the new name. | Edge register. |

---

## 1. Business description

tgNtfy v1 shipped with a hidden premise: the gate must be told, in advance, which services exist and what to call them — `config/events.yaml` (the catalog) lists `goyoutube`, `gomail`, `gorecomendarr`, `govpn` with display names, and the bot **pre-creates one forum topic per catalog service** when a user finishes `/connect`. That breaks the core promise of the gate — "adding a service is a `POST`, zero gate changes": today, onboarding a new service means (a) an admin creates the service row, (b) the repo's `events.yaml` is edited, (c) the image/config is redeployed, and (d) existing users' topics are only correct if the catalog matched the service's own identity.

**v1.1 makes the gate service-agnostic:**

1. **The service names itself.** When a service links a user (`POST /v1/link`), it supplies its own operator-visible display name. That name is the single source for the forum topic name.
2. **Topics are created lazily at registration** — at the moment of linking (if the user already has a forum group) or at first event delivery (if not) — never in bulk from a static registry.
3. **The catalog becomes optional.** `events.yaml` survives only as a hint file (per-type default severities, `drop` flags). The gate boots, ingests, renders, and delivers with **zero** catalog content.
4. **Two v1 acceptance FAILs are closed:** the mathematically impossible coalesce AC (≤2 for 60 events under a cap-20 batch) is corrected to ≤3 (owner-approved), and the forum happy path gets a **positive** test (topic IS created at registration with the service-supplied name — v1 only asserted topic-creation failure paths).

**User value:** onboarding a brand-new notification source takes one `admin service create` + one link — no repo edit, no redeploy, no knowledge of the service inside the gate. **Operational value:** the gate's behavior is identical with or without `events.yaml`, and topic creation is idempotent (re-links never duplicate topics).

---

## 2. Use cases (v1.1 deltas; unchanged v1 UCs inherit v1 SPEC §2)

Actors: **User** (Telegram human), **Service** (HTTP client holding `X-Service-Token`), **Admin** (Nikolay, `tgntfy admin` CLI).

### UC-V2 — Identity linking with self-supplied name (amends v1 UC-2)
- **Actor:** User + Service. **Goal:** gate binds service identity ↔ TG user AND learns the service's display name; the user's service topic exists (or is guaranteed to appear on first event).
- **Main flow:**
  1. User in DM: `/link` → inline keyboard lists **enabled services from the `services` table** (store-driven, §8); user taps one.
  2. Gate issues a 6-digit `link_code` (10 min, single use — unchanged) and replies with the code + the service's **store** display name.
  3. User enters the code in the service's UI. The service calls `POST /v1/link` with its token: `{"service":"goyoutube","user_ref":"17","code":"482913","display_name":"goYouTube"}` (field per §3).
  4. Gate: token valid → code valid → **one transaction**: consume code, upsert `service_users`, upsert `subscriptions`, and (if `display_name` present non-blank) `services.display_name := SanitizeTopicName(display_name)` (V-2).
  5. **Topic step (V-3):** if the linked user has `delivery_mode='group'` + `group_chat_id`: `GetOrCreateTopic(user, group, service)` — creates `createForumTopic(chat, storeDisplay)` exactly once if no row exists, persists `group_topics(user, service, message_thread_id)`. If the user is in `dm` mode: no topic call (correct — no group exists yet).
  6. Response `200 {"status":"linked","user_id":<id>,"topic_created":<bool>}` (`topic_created=true` iff a `group_topics` row exists for (user,service) after step 5).
- **Alternative flows:**
  - 5a. TG `createForumTopic` fails at link (flood/transport) → link still `200`, `topic_created=false`, warn log. The first-event fallback (§4.3) creates the topic on the first event. Rationale: identity binding must not be hostage to transient TG failures; events are retried by the delivery pipeline, links are not retried by services.
  - 5b. User not yet in a group → link succeeds, `topic_created=false`; after `/connect` the first event creates the topic (UC-V3).
- **Error flows:** v1 matrix kept (401 / 400 `code_invalid|code_mismatch|service_unknown` / 409 `already_linked`), plus `400 schema` when raw `display_name` > 256 chars (§3.2).

### UC-V3 — First-event fallback: topic created on demand (amends v1 UC-3 5b)
- **Actor:** Service. **Goal:** no event is dropped or mis-routed for a (user, service) that has no topic row yet.
- **Main flow:** event ingested → routed to a `group`-mode user → `store.GetOrCreateTopic`: row present → use it; absent → create topic named from `services.display_name` (sanitized; V-5) → persist row → deliver with `message_thread_id`.
- **Alternative flow:** creation fails → delivery enqueued with `thread_id=0` + normal retry/backoff (v1 behavior, E-11); each retry re-attempts creation via the same idempotent path.
- **Invariants:** at most one extra `createForumTopic` call per (user, service) per creation attempt; **never** one per event (v1 bug fixed, V-5); ordering/liveness preserved (creation happens BEFORE delivery of the same event).

### UC-V1 — Setup ritual (amends v1 UC-1 step 5)
- v1.1: after `/connect` code validation (forum + admin rights + sender admin — unchanged), the gate sets `delivery_mode='group'` + `group_chat_id`, **deletes the user's existing `group_topics` rows** (V-9, E-16), and confirms. **No topics are created at `/connect`.** Topics appear per linked service at link time (already-grouped users) or first event (V-3).
- All v1 verify-fail flows (E-6/E-7/sender-not-admin, code preserved) unchanged.

### UC-V4 — Operating with no catalog (new)
- **Actor:** Admin. **Goal:** run the gate with `events.yaml` absent or empty and everything works.
- **Main flow:** boot → catalog load fails/empty → warn log, empty catalog, **no** `EnsureRegistered` side effects (loop removed, V-6) → ingest/link/menu/delivery all operate from the store only.
- **Error flow:** SIGHUP with a bad file → keep previous snapshot (v1 behavior kept).

---

## 3. `POST /v1/link` — v1.1 schema (FROZEN)

### 3.1 Request
```json
{"service":"goyoutube","user_ref":"17","code":"482913","display_name":"goYouTube"}
```
Auth: `X-Service-Token` of `service`. Body cap 8 KB (unchanged).

| Field | Type | Required | Rules |
|-------|------|----------|-------|
| `service` | string, 1..32 | yes | Must equal the token's service id (else `400 code_mismatch`). Must be a registered+enabled service (else `400 service_unknown`). |
| `user_ref` | string | yes | Service-side identity (unchanged). |
| `code` | string | yes | 6-digit link code issued for (service, this TG user) (unchanged). |
| `display_name` | string | **no** (V-2) | Operator-visible label the service supplies for itself. Raw length ≤ **256** chars (else `400 schema`, detail `display_name must be ≤256 chars`). After `SanitizeTopicName` (§5): if empty → **treated as absent** (existing stored name kept, no error). If non-empty → replaces `services.display_name` (clamped to 128 code points by sanitization). |

Back-compat: a v1-shaped body (no `display_name`) is fully valid; nothing is required of already-known services.

### 3.2 Processing order (binding)
1. Method/token/schema checks (v1 order kept) — including the new `display_name` raw-length check at schema time.
2. `store.LinkIdentity(service, userRef, code, codeUserID, tgUserID, sanitizedDisplayName)` — ONE transaction: consume code · `service_users` upsert · `subscriptions` upsert · (if `sanitizedDisplayName != ""`) `UPDATE services SET display_name=? WHERE service=?` (V-2).
3. Topic step (V-3): `GetUser(tgUserID)`; if `delivery_mode='group'` and `group_chat_id != nil` → `store.GetOrCreateTopic(tgUserID, groupChatID, service, createFn)` where `createFn` = the wired topic resolver (§4.3). Errors here: warn log, never fail the request.
4. Respond.

### 3.3 Responses (frozen)
| Status | Body | When |
|--------|------|------|
| `200` | `{"status":"linked","user_id":<id>,"topic_created":<bool>}` | binding succeeded (incl. same-user re-link idempotent refresh — V-4). `topic_created=true` iff a `group_topics` row exists for (user,service) afterwards (created now OR pre-existing); `false` in dm mode or on creation failure. |
| `400` | `{"error":"schema","detail":"display_name must be ≤256 chars"}` | raw `display_name` > 256. |
| `400` | `{"error":"code_invalid"}` / `{"error":"code_mismatch"}` / `{"error":"service_unknown"}` | v1 conditions (kept). |
| `401` | `{"error":"unauthorized"}` | bad/missing token (kept). |
| `409` | `{"error":"already_linked"}` | `user_ref` already bound to a DIFFERENT TG user (kept; same-user re-link is 200, not 409). |

---

## 4. Topic lifecycle (binding)

### 4.1 Creation points — exactly two
1. **At registration (link):** UC-V2 step 5 — only when the user is already in group mode.
2. **At first event (fallback):** UC-V3 — any group-mode (user, service) without a `group_topics` row.
No third creation point exists. `finishSetup`/`/connect` creates **zero** topics (V-9) and deletes stale rows.

### 4.2 Idempotency (V-4)
- ALL topic resolution goes through `store.GetOrCreateTopic(userID, chatID, service, resolver)`:
  `SELECT message_thread_id FROM group_topics WHERE user_id=? AND service=?` → hit: return it (no Bot API call);
  miss: `resolver(chatID, service)` → `INSERT … ON CONFLICT(user_id, service) DO NOTHING` (existing helper, currently unused — now wired).
- Consequence: re-link, re-event, concurrent link+event → at most the pre-existing E-11 race (two creators, one row wins, the loser's topic is an orphan in TG — acceptable, same as v1). No duplicate ROWS ever; duplicate TG topics only via the documented race or V-12 re-link rename.

### 4.3 Event-path resolver (V-5; fixes v1 per-event-create bug)
- `main.go` wires `ingest.WithTopicResolver(r)` where `r(ctx, userID, chatID, svc)` does: `name := store.DisplayName(svc)` (i.e. `SELECT display_name FROM services`), `name = SanitizeTopicName(name)` (fallback: `svc` if empty), then `client.CreateTopic(chatID, name)`. **No catalog access.**
- `store.ResolveRoutes` routes the group-mode branch through `GetOrCreateTopic` (adapting the `TopicResolver` func to the `resolver(chatID, service)` shape), so the row lookup happens in the store and the Bot API is only called on a miss.
- Failure semantics unchanged: on resolver error the delivery is enqueued with `thread_id=0` for the retry path (E-11).

### 4.4 Name source (single, binding)
`group_topics` row → else create with `SanitizeTopicName(services.display_name)` → else (name empty after sanitization — cannot happen for admin-created rows) the service id. **The catalog is never consulted for names, anywhere.**

---

## 5. Name sanitization (binding, V-7)

`internal/topic/topic.go` (NEW package): `SanitizeTopicName(s string) string`

1. **Whitespace:** every Unicode whitespace (`unicode.IsSpace`) run → one ASCII space; trim leading/trailing.
2. **Control/format chars:** drop runes in Unicode categories `Cc`/`Cf` (BOM, zero-width, ESC…).
3. **Clamp:** truncate to **128 code points** (rune-based, NOT bytes — Telegram counts characters). No ellipsis suffix.
4. **Empty result:** return `""`; the caller falls back to the service id (1..32, always TG-legal).

Test vectors (QA, SAN-1): `"  goYouTube "` → `goYouTube`; `"a\n\n  b"` → `a b`; 200 chars → first 128; 128 emojis → unchanged; `"\x00\x07x"` → `x`; `"   "` → `""` (caller falls back to service id); `"\u200B\uFEFFMail"` → `Mail`; 130 CJK chars → first 128.

---

## 6. Catalog demotion (binding, V-6)

### 6.1 What changes
- `cmd/tgntfy/main.go`: **REMOVE** the startup loop `for svc, s := range cat.Services { st.EnsureRegistered(...) }`. Keep: load attempt → on error `log.Warn` + empty catalog (now the norm, not the exception). `EnsureRegistered` stays in the store (admin/CLI paths may still use it) but nothing calls it from the catalog.
- Service rows are created **only** by `admin service create <id> --name …` (CLI unchanged; `--name` stays required — it is the initial default that links may later overwrite).
- `internal/ingest`: unchanged behavior on an empty catalog (already tolerates it: unknown type → warn + accept, A-4; `job_progress` hard-coded drop, D-3; catalog `drop:true` rule active only when the entry exists). No code may `return error` because the catalog lacks a service.
- SIGHUP reload kept (optional hints only on success; failure keeps previous).

### 6.2 What the catalog still provides (and only this)
- Per-`(service, type)` **default severity** — hint for `/menu` grouping/docs; the event's own `severity` always drives rendering (v1 rule kept).
- **`drop: true`** flag — ingest drop rule (accepted, counted `catalog_dropped_total`, not delivered).
- Nothing for auth, routing, topic creation, or display names.

### 6.3 Empty/absent-catalog contract (AC V11-4)
With an empty catalog the gate MUST: boot (warn log, exit 0) · accept events for any registered service (200, unknown-type warn) · render with the STORE display name · route/deliver to the correct group topic · serve `/link` (keyboard from the store) and `/menu` (linked services from the store, no type toggles). Covered by CAT-1 (§11).

---

## 7. Coalesce AC correction (binding, V-8)

- v1 SPEC §8 "AC math" (≤ 2 messages for 60 events in 10 s) is **superseded**: with tumbling 5 s windows AND batch cap 20, 60 events have a hard floor of `ceil(60/20) = 3` messages. The old AC was unsatisfiable — confirmed by v1 acceptance (card t_4b15682c), where QA measured **exactly 3**.
- **Corrected AC (verbatim for acceptance):** *"60 same-type events for one (user, service) over 10 s, production config (window 5 s, batch cap 20), produce **≤ 3** Telegram messages; `Σ deliveries.batch_size = 60`; every batch ≤ 20."*
- **Test (COA-1, rewritten):** harness with `coalesceWindow = 5*time.Second`, `coalesceCap = 20`; post 60 same-`(user,service,type)` events paced ≤ 30/s spread over ~10 s; assertions: `sendMessage` count for that chat ≤ 3 (AC) and ≥ 1; `Σ batch_size == 60`; each recorded batch ≤ 20. (v1 acceptance measured exactly 3 under this config — cite it in the test comment.) The v1 `TestCOA1CoalesceBurst` (900 ms window, `≤ 12`) is REPLACED, not kept alongside.
- §8 mechanics (5 s window, key `(user, service, type)`, cap 20) are unchanged — only the AC wording and its test tighten.

---

## 8. Menu & UX changes (store-driven, V-10/V-11)

### 8.1 `/link` (cmdLink / issueLinkCode)
- Keyboard = **enabled rows from `store.ServiceList`** (filter `enabled=1`), label = store `display_name`, callback `link:svc:<id>` (format kept). No catalog enumeration.
- Code message uses the store display name (v1 used catalog; fallback chain: store name → service id).

### 8.2 Text updates
- `welcomeText`: drop the four hard-coded names → "Hi! I'm the tgNtfy gate. Your service events arrive in YOUR personal Telegram forum group — one topic per linked service.\n1) Link a service: /link\n2) Create your group: /setup\nManage anything in /menu."
- `/setup` step-2 text: replace "I'll create one topic per linked service." with "Your linked services will get a topic each." (topics are lazy now).
- `finishSetup` confirmation: list linked services' store display names (v1 catalog-driven `linkedServiceNames` → store-driven).

### 8.3 `/menu`
- Level 1: linked services (store display names, mute icons as v1) + one row `➕ link another service…` → re-opens the UC-V2 keyboard (store-driven). No per-catalog-service `➕ link` rows.
- Level 2: per-type toggle rows **only for types declared by the catalog** for that service (optional hint); if the service has no catalog entry (or none of its types) → no type rows, header note "(event types unknown to the gate — all types on)" + `🔕 Mute all` + `⬅️ Back` (kept). Mute/subscription mechanics unchanged.

### 8.4 `/connect` (finishSetup, V-9)
After code consumption + `SetDeliveryMode('group', chatID)`: `store.ClearUserTopics(userID)` (NEW: `DELETE FROM group_topics WHERE user_id=?`), then the confirmation (no topic list, no `createTopicsFor`).

---

## 9. Data model & store changes (binding)

- **No schema migration.** `services.display_name` (NOT NULL) becomes the **authoritative** topic-name source; `group_topics` unchanged. No new columns.
- `store.LinkIdentity` gains a trailing parameter `displayName string` (empty = "do not update the name"); the `UPDATE services SET display_name=…` executes INSIDE the existing transaction when non-empty. (All call sites — ingest + tests — updated.)
- NEW `store.SetServiceDisplayName(ctx, service, name) error` (simple UPDATE; admin/CLI/tests).
- NEW `store.DisplayName(ctx, service) (string, error)` → `(serviceID, nil)` on `sql.ErrNoRows` (safe fallback for the resolver/renderer).
- NEW `store.ClearUserTopics(ctx, userID) error` (V-9).
- `store.GetOrCreateTopic` (existing, previously unused) — wired into `ResolveRoutes` and the link path (§4); behavior unchanged (row-first, upsert `DO NOTHING`).
- `ResolveRoutes` group branch: replace direct `resolveTopic(ctx, uid, gci, svc)` with `GetOrCreateTopic(uid, gci, svc, func(chatID int64, service string) (int, error) { return resolveTopic(ctx, uid, chatID, service) })`.

---

## 10. File map by role (binding scope)

**Backend** (`backend: ` commits):
| File | Change |
|------|--------|
| `internal/topic/topic.go` | NEW: `SanitizeTopicName` (V-7). |
| `internal/store/store.go` | `LinkIdentity(+displayName)`; NEW `SetServiceDisplayName`, `DisplayName`, `ClearUserTopics`; `ResolveRoutes` → `GetOrCreateTopic` (§9). |
| `internal/ingest/ingest.go` | `handleLink`: parse/validate `display_name`, pass sanitized name to `LinkIdentity`, post-link topic step via the existing `topicResolver` opt + `GetOrCreateTopic`, response `topic_created` (§3). |
| `cmd/tgntfy/main.go` | REMOVE catalog `EnsureRegistered` loop; `topicResolver` name from `store.DisplayName` + sanitize (no catalog); `batchFlusher.Flush` render display from `store.DisplayName` (fallback service id). |
| `internal/menu/menu.go` | `cmdLink` keyboard from `ServiceList` (enabled); `issueLinkCode` store name; `finishSetup`: remove `createTopicsFor`, add `ClearUserTopics`, new texts; `linkedServiceNames` store-driven; `menuLevel1/2` store/catalog-optional per §8; `welcomeText` generic. |

**QA** (`qa: ` commits):
| File | Change |
|------|--------|
| `internal/topic/topic_test.go` | NEW: SAN-1 table (V-7 vectors). |
| `internal/itest/harness_test.go` | mirror v1.1 wiring: resolver via `store.GetOrCreateTopic` + `store.DisplayName`; flusher render from store name; harness option for **empty catalog**. |
| `internal/itest/integration_test.go` | REWRITE `TestCOA1CoalesceBurst` → COA-1 per §7 (5 s window, cap 20, ≤ 3, Σ60, ≤ 20/batch); FOR-2 keeps gates + asserts **zero** `createForumTopic` on successful `/connect` (V-9); LINK-1 extended per §11. |
| `internal/itest/v11_test.go` | NEW: FOR-3, FOR-4, IDL-1, DM-1, CAT-1, CAT-2 (see §11). |

**Frontend:** none (no web UI). **Docs-update card:** README (link flow with `display_name`, lazy topics, optional catalog), DEPLOYMENT.md (catalog optional); this SPEC is already on the branch.

---

## 11. Acceptance criteria (v1.1 — restated, verifiable) + test plan

| ID | AC (binding) | Test | Key assertions |
|----|--------------|------|----------------|
| **V11-1** | No code path creates topics from a static catalog; creation is registration-triggered (+ first-event fallback). | CAT-1 + code-review gate (integrate) | empty-catalog harness: link + event produce a topic with the store-supplied name; no `catalog` reference on any topic-creation call path (checked in review). |
| **V11-2** | `POST /v1/link` carries `display_name`; topic created in the user's group with the sanitized name; `group_topics` row stores `message_thread_id`. | **FOR-3** (positive — closes v1 acceptance FAIL #2) | user U in group G; service S linked via code with `display_name "  Go YouTube  x!"` → expected topic name `"Go YouTube x!"`; → `200`, `topic_created=true`; mock recorded **exactly 1** `createForumTopic` with that exact `name`; `group_topics(U,S)` row holds the mock thread id; a following event → `sendMessage` carries that `message_thread_id`. |
| **V11-3** | Re-link idempotent: no duplicate topic. | **IDL-1** | link S→U (code 1) → 1 topic; fresh code, same user+service, different `display_name` → `200` (not 409); `createForumTopic` total still **1**; `group_topics` row same thread id; `services.display_name` updated; variant: same `user_ref` → different TG user → `409 already_linked`. |
| **V11-4** | Empty/absent catalog: gate boots, ingests, renders, delivers. | **CAT-1** (empty catalog end-to-end) + **CAT-2** (absent-file boot path) | CAT-1: harness with `catalog.Empty()`: event 200 → rendered message contains the **store** display name; delivered to the right chat/thread; `/link` keyboard lists DB services; `/menu` level-2 shows no type rows. CAT-2: `catalog.Load` on a missing path returns error → main-style construction (empty catalog + warn) yields a working ingest handler (unit-level). |
| **V11-5** | Coalesce AC = **≤ 3** messages for 60 same-type events in 10 s (spec §7 + test aligned). | **COA-1** (rewritten) | `n ≤ 3` (v1 acceptance measured exactly 3); `Σ batch_size == 60`; each batch ≤ 20; production window 5 s + cap 20 in the harness. |
| **V11-6** | Full suite green (`go build`, `go vet`, `go test ./...`); docs updated. | suite + docs card | all v1 tests (ISO/IDP/RATE/AUTH/SEC/HZ/ERR/FOR-2/RET) stay green with the new wiring. |

**Additional tests (supporting, in `internal/itest/v11_test.go`):**
- **FOR-4 (first-event fallback):** U in **dm** mode; link S (code) with `display_name` → `200`, `topic_created=false`, **0** `createForumTopic`; then `/connect` (code path) → `ClearUserTopics` no-op; then 1 event → **exactly 1** `createForumTopic` (name = store display name) before delivery; `sendMessage` carries the new `message_thread_id`; 2nd event → **no** further `createForumTopic` (row hit — V-5 fix asserted).
- **DM-1 (deferred creation contract):** link while dm → `topic_created=false`; event while dm → delivered to DM (`chat_id == U`, `message_thread_id` absent/0); no topic call. (Locks in V-3: "a link may not create a topic, and that is correct.")
- **LINK-1 (extended, v1.1 schema):** `display_name` absent → name unchanged (pre-seeded store name kept, 200); present → updated (IDL-1); raw > 256 chars → `400 schema` with detail; whitespace-only → treated as absent (200, name unchanged).

---

## 12. Edge cases register (v1.1 deltas; v1 E-1..E-15 kept)

| # | Scenario | Behavior (binding) |
|---|----------|--------------------|
| E-16 | User re-binds to a NEW group via `/connect` while old `group_topics` rows exist | `ClearUserTopics` at bind (V-9); old TG topics orphaned in the old group (acceptable, documented); first event creates fresh topics in the new group. |
| E-17 | Re-link updates `display_name` but the topic already exists | Stored name updated; existing TG topic **not** renamed (V-12); new name applies to future (re)creations only. |
| E-18 | `createForumTopic` fails at link (429/transport) | Link still 200, `topic_created=false`, warn log; first-event fallback retries (V-3 / UC-V2 5a). |
| E-19 | Concurrent link + first event for the same (user, service) | `GetOrCreateTopic` upsert `DO NOTHING`: one row; worst case one orphan TG topic (pre-existing E-11 tolerance). |
| E-20 | `display_name` = 200 CJK chars | sanitized to 128 code points (rune-safe), topic created with the clamp; stored value is the sanitized string. |
| E-21 | Catalog present but service missing from it (the normal v1.1 state) | No degradation: warn `catalog_unknown_total` on unknown types (A-4), no type toggles in menu, store name everywhere. |
| E-22 | `govpn` auto-link (UC-2 2a, `ClaimGovpnAdmin`) | Unchanged — it bypasses `/v1/link`; the govpn topic appears on first event with the store display name (admin's `--name`), provided the govpn service row exists. |

---

## 13. Out of scope (v1.1, frozen)

Service-side adapters (no service is modified to actually send `display_name` in this epic — v1-shaped bodies remain valid and fully functional) · `renameForumTopic` (V-12) · topic deletion on unlink · per-topic icons/colors · kidsEdu · NATS · web UI · new deploy (reuses t_7cadfbc9 after main merge) · token rotation grace periods.

---

## 14. Definition of done for the v1.1 epic
`go build` + `go vet` clean · `go test ./...` green (v1 suite + §11 table) · AC V11-1..V11-6 verified by the acceptance run on `feature/tgnfyt-t_352cddfe-gate-v1` · README/DEPLOYMENT updated by the docs card · this spec section-by-section re-checked at acceptance.
