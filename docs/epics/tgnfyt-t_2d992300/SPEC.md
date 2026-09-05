# tgNtfy — Deep Analysis & Behavior-Preserving Refactoring SPEC

**Epic:** t_2d992300 — deep code refactoring & optimization (behavior-preserving)
**Project:** tgNtfy (`github.com/spluft/tgNtfy`) · board `spluft` · default branch `main` (analysis base: `48d6964`)
**Image:** `ghcr.io/spluft/tgntfy` · **Feature branch (integrate):** `feature/tgnfyt-t_2d992300-deep-refactoring-optimization`
**Status:** BINDING for backend / qa / integrate / acceptance. Language: English (user convention).
**Author:** architect, 2026-09-05. Every claim below was re-verified against the live tree at `48d6964` (file paths, line numbers, `go list`, `go test`, `go vet` clean).

---

## 1. Business description

tgNtfy is the production notification gate for the spluft services. It works and ships;
this epic changes **what a user gets: nothing** — and that is the point. The value is
entirely operational:

- **Maintainability.** Two god-files (`store.go` 1105 LOC, `menu.go` 551 LOC) mix 8+
  unrelated domains each; every future feature edit pays a comprehension tax and a
  merge-conflict tax on those two files.
- **Safety of future changes.** The integration harness (`internal/itest`) *re-implements*
  the production coalesce-flush logic instead of importing it, so tests can stay green
  while production drifts. Closing this is the single highest-value fix in the epic.
- **Dead weight.** ~25 verified-dead functions/fields (section 6) invite accidental use
  of the wrong variant of near-duplicate logic (e.g. `SaveConnectCode` vs
  `CreateConnectCode`, `EnqueueDelivery` vs `CreateDelivery`).
- **Coverage.** Overall statement coverage is 42.8% cross-package (26.2% same-package);
  `admin` and `cmd` are at 0%. The refactor plan sets per-package floors so regressions
  are mechanically detectable.

**Hard boundary:** behavior-preserving only. No new features, no API/CLI/auth/env
contract changes, no schema changes, no deliberate user-visible behavior changes.
Suspected defects found during analysis (section 8) are **reported, not fixed** here.

## 2. Use cases (for this epic)

1. **Backend implementer** — *Goal:* execute the ranked refactor phases without breaking
   behavior. *Main flow:* read SPEC → implement phase P1–P5 in order → after each phase
   run `go vet ./... && go test ./...` (all 43 tests green, zero test-file edits except
   the harness wiring change explicitly sanctioned in R1) → commit. *Error flow:* any
   test failure he cannot explain as harness-wiring → `kanban_block` (needs_input).
2. **QA implementer** — *Goal:* raise coverage to the section 7 targets and prove
   behavior preservation. *Main flow:* run baseline method (section 7.1) on the
   pre-refactor tree → add tests for uncovered surfaces (admin CLI, menu, ingest error
   paths) without changing existing test semantics → re-run before/after report.
   *Error flow:* a test that cannot pass post-refactor without changing its assertions →
   the refactor broke behavior → FAIL evidence in card.
3. **Acceptance reviewer** — *Goal:* verify MUST NOT BREAK (section 9) verbatim. *Main
   flow:* diff contract surfaces (envelope, codes, CLI, env), run full suite on the
   feature branch, confirm no `.go` production API changed in a way visible outside
   `internal/` (it cannot — everything below `cmd/` is `internal/`).

## 3. Analysis summary (fact-checked)

### 3.1 LOC map (recomputed with `wc -l` at 48d6964 — PM baseline confirmed exact)

| Path | Prod LOC | Test LOC |
|---|---|---|
| `cmd/tgntfy/main.go` | 231 | — |
| `internal/store/store.go` | 1105 | 150 |
| `internal/menu/menu.go` | 551 | 0 |
| `internal/ingest/ingest.go` | 466 | 191 |
| `internal/transport/transport.go` | 245 | 116 |
| `internal/admin/admin.go` | 202 | 0 |
| `internal/catalog/catalog.go` | 161 | 50 |
| `internal/tgbot/tgbot.go` | 145 | 0 |
| `internal/coalesce/coalesce.go` | 111 | 80 |
| `internal/limit/limit.go` | 89 | 52 |
| `internal/healthz/healthz.go` | 70 | 53 |
| `internal/topic/topic.go` | 45 | 61 |
| **Prod total** | **3421** | |
| Unit-test total (`internal/*/*_test.go` minus itest) | | **753** |
| `internal/itest/` (test-only): integration 314, v11 268, harness 235, mockbot 184, health_link 50 | | **1051** |
| **Grand total** | | **5225** |

Test functions: 43 total (itest 14; unit 29 across catalog 3, coalesce 3, healthz 3,
ingest 6, limit 3, store 5, topic 2, transport 4). `go vet ./...` clean.

### 3.2 Package dependency map (`go list -f '{{.ImportPath}}: {{join .Imports " "}}'`)

```
topic, limit, catalog, coalesce, store, tgbot, healthz   →  NO internal imports (leaves)
ingest    → catalog, coalesce, limit, store, topic
menu      → catalog, store, tgbot
admin     → store
transport → (no internal imports; uses go-telegram/bot + prometheus directly)
itest     → catalog, coalesce, healthz, ingest, menu, store, tgbot, topic, transport  (test-only)
cmd/tgntfy→ admin, catalog, coalesce, healthz, ingest, menu, store, tgbot, topic, transport
```

**No import cycles.** The DAG is shallow and clean: 7 leaf packages + 3 mid packages +
composition root. The store↔tgbot decoupling is already correct (topic creation injected
as `store.TopicResolver` func, `store.EnsureTopic(creator)` callback). The only
dependency-design smell is `cmd/tgntfy` importing **all 10** packages and owning real
logic (the `batchFlusher`, `runServer`) — see R1.

### 3.3 Event flow map (verified against code)

**POST /v1/events** (`ingest.go:256`): method check (405) → body read with 8192+1 cap
before auth (413 `too_large`) → `authService` X-Service-Token → `store.VerifyToken`
(sha256 match, enabled=1) + `ServiceEnabled` (401/400) → `json.Unmarshal` +
`validateEnvelope` (400 `schema`) → envelope `service` must equal token service (400) →
`limit.Registry.AllowForService` sliding 30/s + 100/min (429 + `Retry-After: 1`) →
drop rule: catalog `drop:true` OR unknown-type `job_progress` → 200
`{"status":"accepted","queued":0}` + counters → unknown (svc,type) accepted with warn
+ `catalog_unknown_total` → `store.InsertEvent` (UNIQUE event_id → 409 `duplicate`) →
`store.ResolveRoutes` (active service_users ∩ subscription muted/event_types filters;
group mode → `EnsureTopic` lazy create, fallback thread 0 on create failure, E-11) →
per target `coalescer.Add(key{user,service,type})` (5 s tumbling window, cap 20) →
200 `{"status":"accepted","queued":len(targets)}`.

**Coalesce flush** (`main.go:197` `batchFlusher.Flush`): resolve chat/thread (group mode →
`group_chat_id` + raw SQL `SELECT message_thread_id FROM group_topics` via
`store.QueryRow`) → render `RenderMessage` / `RenderBatch` (>1 item, §8.5 format,
truncate 3500/120/200) → `store.CreateDelivery` row → `transport.Queue` (bounded 5000,
drop-on-full) → `TelegramTransport.Run` loop → token bucket (cap 30, 0.5/s — single
shared bucket, not per-chat despite the name) → `deliverWithRetry`: max 5 attempts,
5s×2^n backoff cap 5min, 429 honors `retry_after` **without consuming an attempt**
(`attempt--`), `ErrorForbidden` fails fast, metrics `deliveries_ok/fail/429_total`.

**POST /v1/link** (`ingest.go:153`): 405 → auth(requireEnabled) → body ≤8192 →
`{service,user_ref,code,display_name?}` schema checks (display_name ≤256) → service field
must match token (400 `code_mismatch`) → `HasService` → `UserIDForLinkCode` (identity
comes from the code row) → sanitize display_name (`topic.SanitizeTopicName`; empty ⇒
absent) → `store.LinkIdentity` tx: consume code (single-use, expiry, owner), enforce
codeUserID==tgUserID, upsert service_users (different owner ⇒ 409 `already_linked`),
ensure subscription, optional display_name UPDATE → errors map to
409/400 code_invalid/code_mismatch → **topic step**: only if user already
`delivery_mode=group` with `group_chat_id`: `EnsureTopic` (idempotent; failure is a warn,
first-event fallback retries) → 200 `{"status":"linked","user_id":N,"topic_created":bool}`
(true iff row exists afterwards, incl. pre-existing on re-link).

**Menu** (`menu.go`): `/start` (govpn admin auto-claim), `/setup` S1→S2 keyboard →
`verifySetup` (getChat forum + `BotCanManageTopics` + `SenderIsGroupAdmin`) → connect code
→ `/connect <code>` in group → `ConsumeConnectCode` → `SetDeliveryMode(group)` →
`ClearUserTopics` (V-9) → no topics created here. `/link` store-driven keyboard,
`/menu` level-1/2 (mute, per-type toggles), `/status`, `/undelivered`, `/help`. Identity
always from `update.From.ID`.

**Admin CLI** (`cmd/tgntfy/main.go:35`, `admin.Run`): `tgntfy admin <db-path> <sub>` with
`service create <id> --name <d> | list | enable <id> | disable <id> | rotate <id>`,
`link <svc> <ref> <tg_user_id>`, `unlink <svc> <ref>`, `user` (list),
`events [-n N]` (default 20). Raw tokens printed exactly once, only sha256 stored.

**Health/metrics**: `GET /api/health` (200 `{"status":"ok"}` / 503 `unavailable`; 401
unless `X-Admin-Token` matches when `ADMIN_TOKEN` set — constant-time compare) and
`GET /metrics` — **the prometheus dependency IS wired**: `main.go:152`
`root.Handle("/metrics", healthz.Metrics())` (promhttp), with 11 collectors registered
via `promauto` in `ingest` (7) and `transport` (4). PM's "possibly-unused prometheus"
suspicion: **false** — keep the dependency.

### 3.4 Contract inventory (FROZEN — exact current state)

**Env vars** (verified `main.go` + `.env.example`; all 9 accounted for):

| Var | Default | Read at |
|---|---|---|
| `TG_BOT_TOKEN` | — (required, else startup error) | main.go:75 |
| `TG_API_URL` | "" (real API) | main.go:110 |
| `LISTEN_ADDR` | `:8080` | main.go:80 |
| `DB_PATH` | `/data/tgntfy.db` | main.go:78 |
| `CATALOG_PATH` | `/etc/tgntfy/events.yaml` | main.go:79 |
| `COALESCE_WINDOW_MS` | 5000 | main.go:81 |
| `ADMIN_TOKEN` | "" (health open) | main.go:82 |
| `LOG_FORMAT` | `json` (`text` opts into text) | main.go:67 |
| `LOG_LEVEL` | info (`debug` opts into debug; any other value = info) | main.go:63 |

**HTTP surface**: `POST /v1/events`, `POST /v1/link`, `GET /api/health`, `GET /metrics`,
mounted `root.Handle("/v1/", ing.Routes())`. Error envelope
`{"error":"<code>","detail":"<detail>"}` with codes: `unauthorized`(401), `schema`(400),
`service_unknown`(400), `too_large`(413), `rate_limited`(429, `Retry-After: 1`),
`duplicate`(409), `code_invalid`(400), `code_mismatch`(400), `already_linked`(409),
`method`(405), `internal`(500). Success: `{"status":"accepted","queued":N}` /
`{"status":"linked","user_id":N,"topic_created":bool}` /
`{"status":"ok"|"unavailable"}`. Body cap 8192 bytes on both POST endpoints.

**Envelope** (`ingest.go:56`, frozen §3.2): `v`==1, `event_id` 1..255, `service` 1..32
(optional in body but must equal token service when present), `user_ref` required, `type`
1..64, `severity` ∈ {info,warn,error,success}, `title` 1..200, `text` ≤500, `url` ≤500,
`metadata` freeform object.

**Catalog** `config/events.yaml`: `version: 1` + `services.<id>.{display_name,
events.<type>.{severity,drop}}`; optional since v1.1 (absent = debug log, not an error);
SIGHUP reload keeps previous snapshot on parse error.

**DB schema**: 9 tables (users, services, service_users, subscriptions, events,
deliveries, group_topics, link_codes, connect_codes), DDL in
`store.go:56` (`dbSchema`), WAL/busy_timeout=5000/synchronous=NORMAL/foreign_keys=ON.
**No migration in this epic.**

### 3.5 Baseline coverage — measured 2026-09-05 at `48d6964` (the "before" for QA)

Method A — `go test -cover ./...` (same-package attribution; headline today):

| Package | A-cov | Package | A-cov |
|---|---|---|---|
| catalog | 49.0% | menu | **0.0%** |
| coalesce | 87.9% | store | 29.2% |
| healthz | 76.9% | tgbot | 0.0% |
| ingest | 51.4% | transport | 62.0% |
| limit | 92.9% | admin | **0.0%** |
| topic | 100.0% | cmd/tgntfy | **0.0%** |

`go test -coverprofile` total: **26.2%**.

Method B — `go test -coverpkg=./internal/...,./cmd/... -coverprofile=cover_all.out ./...`
(cross-package attributed, deduped per block; the honest number — itest exercises
menu/store/ingest across package lines). Total **42.8%** (650/1520 statements):

| Package | stmts | covered | B-cov |
|---|---|---|---|
| cmd/tgntfy | 210 | 0 | 0.0% |
| internal/admin | 127 | 0 | 0.0% |
| internal/catalog | 51 | 30 | 58.8% |
| internal/coalesce | 33 | 29 | 87.9% |
| internal/healthz | 26 | 20 | 76.9% |
| internal/ingest | 220 | 154 | 70.0% |
| internal/limit | 28 | 26 | 92.9% |
| internal/menu | 276 | 73 | 26.4% |
| internal/store | 421 | 218 | 51.8% |
| internal/tgbot | 39 | 26 | 66.7% |
| internal/topic | 18 | 18 | 100.0% |
| internal/transport | 71 | 56 | 78.9% |
| **TOTAL** | **1520** | **650** | **42.8%** |

(Both profiles are reproducible with the exact commands in section 7.1 on a clean tree
at `48d6964`; module deps require the DoH proxy for `go mod download` per epic note.)

## 4. Refactoring opportunities — RANKED by impact × risk

All items are behavior-preserving by construction; "proof" states why, "verify" states the
mechanical check. Phases P1–P5 below are the execution order.

### R1 (P1) — Extract the coalesce-flush + topic-resolver wiring out of `main.go` into a shared package. Impact: HIGH · Risk: LOW

**What/where:** `cmd/tgntfy/main.go:120-126` (topicResolver closure) and
`main.go:190-231` (`batchFlusher`) are duplicated near-identically by
`internal/itest/harness_test.go:98-104` and `:127-163` (flusher). Extract both into a new
package `internal/dispatch` (e.g. `dispatch.NewBatchFlusher(st, enqueue, log)` +
`dispatch.TopicResolverFor(st, creator)`) and use the extracted code from BOTH `main.go`
and the itest harness.

**Why:** today the QA harness tests a *copy* of production logic — the worst structural
liability in the repo; main.go drops to ~170 LOC; cmd/tgntfy coverage (0%) stops being a
trap because the flush logic becomes a testable library.

**Preservation proof:** the extracted bodies are moved verbatim (no logic edits allowed);
the only permitted harness change is construction wiring, not assertions.
**Verify:** full suite green; itest delivery assertions byte-identical;
`git diff` on moved code shows pure move.

### R2 (P2) — Split `store.go` (1105 LOC) by domain into files of the same package. Impact: HIGH · Risk: LOW

**What/where:** `internal/store/store.go` → `schema.go` (56-151), `users.go`
(159-242), `identity.go` (249-342 LinkIdentity + execer), `events.go` (344-378 +
1075-1098), `routing.go` (380-441), `topics.go` (443-503), `deliveries.go` (505-662 +
859-888), `codes.go` (664-673 SetTopic + 750-841), `subscriptions.go` (675-748),
`services.go` (890-987), `admin.go` (989-1073), `util.go` (901-912, 1091-1105). File
boundaries = existing `// --- section ---` comments; package `store`, zero signature
changes.

**Why:** god-file; every epic touches it; split is mechanical and makes future diffs local.
**Preservation proof:** same package, same declarations, compile-identical output.
**Verify:** `go build ./... && go test ./...` green; `git diff --stat` shows pure moves.

### R3 (P3) — Delete verified dead code (full list in section 6). Impact: MED · Risk: LOW

Each entry in section 6 was grep-verified unused in prod AND tests (or test-only where
marked). Unreachable branch `ingest.go:373-374` is provably shadowed by `:371-372`
(identical first condition) — deleting it cannot change behavior.
**Verify:** compile + full suite; each deletion cites its section-6 row in the commit body.

### R4 (P2, parallel to R2) — Split `menu.go` (551 LOC, 0 unit tests). Impact: MED · Risk: LOW

**What/where:** `internal/menu/menu.go` → `handler.go` (dispatch + BotAPI/Handler),
`commands.go` (/start /link /connect /status /undelivered), `setup.go` (state machine +
verify/finish), `menu_kb.go` (levels 1-2 + callbacks), `codes.go` (newCode), `text.go`
(constants 543-551). Mechanical move; the dead `setNow`/`now`/`userState` entries go via
R3 (note: `userState` is **written at menu.go:149 and never read** — deleting the write
changes no observable behavior).
**Preservation proof:** pure moves + R3 dead deletions; strings untouched (user-visible
TG text is frozen). **Verify:** suite green; `grep` user strings identical before/after.

### R5 (P4) — Close the store leak: replace raw-SQL passthroughs. Impact: MED · Risk: MED

**What/where:** `store.QueryRow` (store.go:901) exposes `*sql.Row`; its callers embed SQL
outside the package: `main.go:207` and `harness_test.go:146`
(`SELECT message_thread_id FROM group_topics …`). Add
`store.GetTopicThread(ctx, userID, service) (int, bool, error)` (same query, same
zero-value-on-miss semantics: current code scans into `tid` and ignores errors → returns
0) and switch callers (the R1-extracted flusher uses it). `store.SetTopic` (666) is
test-only — keep but document.
**Preservation proof:** identical query, identical scan semantics including swallow-to-zero.
**Verify:** suite green; new method unit-tested directly (store pkg test).

### R6 (P5) — Naming & hygiene pass. Impact: LOW · Risk: LOW

`transport.go:79` field `refill43s` → `refillPerSec` (typo); `transport.go:156` comment
says *per-chat* pace but `chatPace` is one **shared** bucket — fix the comment, do NOT
change semantics; `PendingDeliveries` dead `var nr sql.NullString` + `_ = nr`
(store.go:547,551) — delete; `admin.listUsers` `_ = args` (admin.go:158) — take `[]string`
away or use it, either is contract-neutral (args are ignored today and README documents no
`user` flags); unify `DeliveryAttemptFailed`/`DeliveryExhausted` wrappers (store.go:859,
872) with `MarkDeliveryFailed`/`FailDeliveryPermanently` — either delete the wrappers and
point `transport.StoreIface` at the real methods (internal interface, no external impact)
or keep wrappers and inline the targets; pick one, not three layers.
**Verify:** compile + suite; `deliveriesFail/OK` metric names untouched.

### Explicitly REJECTED (with reasons)

- **Per-chat token buckets** (transport.go:156): would change pacing semantics →
  behavior change, out of scope.
- **Wiring `PruneEvents` into a retention scheduler**: new runtime behavior, out of scope.
- **Index on `services.token_hash`** (VerifyToken store.go:957 scans): schema migration =
  risk without measurable need at spluft scale.
- **Fixing defects in section 8**: behavior changes; each needs its own owner decision.
- **Rewriting `admin` flag-parsing hack** (admin.go:84-94): it is CLI contract surface;
  preserve input handling exactly.

## 5. Per-phase acceptance criteria

- **P1 (R1):** `internal/dispatch` exists; `main.go` and `itest/harness_test.go` both
  consume it; zero changed assertions in any `*_test.go` (harness *wiring* lines excepted,
  listed in the commit message); full suite green.
- **P2 (R2, R4):** `store.go` and `menu.go` gone, replaced by domain files;
  `git diff <base> -- internal/store internal/menu` contains only moves/deletions from
  section 6 (no signature diffs); suite green.
- **P3 (R3):** every deletion traceable to a section-6 row; `grep` for each deleted
  symbol returns nothing; suite green.
- **P4 (R5):** no `SELECT|INSERT|UPDATE|DELETE` string literals outside `internal/store`
  (verify: `grep -rn "SELECT " --include='*.go' . | grep -v internal/store` is empty);
  suite green.
- **P5 (R6):** hygiene items applied; metric/help/user-visible strings unchanged.
- **QA:** before/after coverage report (section 7) attached to the card; no package below
  its baseline; totals ≥ section 7.2 targets.

## 6. Dead code & structural liabilities (verified file:line, unused in prod; "T" = also unused in tests)

store.go: `SaveConnectCode` 783 (T, alias of CreateConnectCode) · `EnqueueDelivery` 508
(T, superseded by CreateDelivery) · `PendingDeliveries` 535 (T, in-memory queue replaced
it; contains dead `nr` var 547/551) · `PruneEvents` 1077 (T, no scheduler wired — defect
D3) · `ErrUnknown` 28 (T) · `SetServiceDisplayName` 477 (T) ·
`LatestDeliveriesForUser`+`DeliverySummary` 630-662 (T) · `DeliveryAttemptFailed` 859 /
`DeliveryExhausted` 873 (thin wrappers — R6) · `QueryRow` 902 (leak — R5) ·
`CreateTokenlessService` 245 (test-only helper in prod file → move to a testutil or
export_test). ingest.go: `SetCoalescer` 113 (T, `WithCoalescer` used instead) ·
`userIDForLinkCode` 252 (trivial passthrough; inline) · unreachable
`validateEnvelope` case 373-374 (shadowed by 371). catalog.go: `Empty()` 58 (T, only
*spec* mentions it) · `Catalog.DisplayName` 124 (T; store.DisplayName is canonical).
transport.go: `Queue.TryRecv` 130 (T) · `Dispatcher` interface 31 (never used as a type
outside doc comments — keep per v1 SPEC extension-point intent, or document; do not
silently delete: it is a documented extension point). coalesce.go: `Window()` 111 (T) ·
`ForceFlush` 91 exported but takes unexported `*pending` — unusable by callers outside the
package; internalize (R3/R6). menu.go: `setNow` 69 + `Handler.now` field (never read) ·
`SetupState.userState` 46 (written 149, never read). healthz.go: `Health.Routes` 21 (T;
main uses `HandleHealth`+`Metrics`) · `CatalogLoaded` field 17 (never set).
admin.go: `listUsers` unused `_ = args` 158. itest: `flusher`+resolver duplication (R1).

## 7. Coverage targets & measurement method

### 7.1 Method (binding for before/after reports)

```
go test -coverprofile=cover_same.out ./...                      # A: same-pkg
go test -coverpkg=./internal/...,./cmd/... -coverprofile=cover_all.out ./...   # B: cross-pkg
go tool cover -func=cover_all.out | tail -1                     # totals
# per-package rollup: parse cover_all.out blocks, dedupe per location (max count),
# sum statements vs covered per package (script pattern: /opt/data/tmp/rollup.py in
# the architect container; QA may commit an equivalent under itest/ if desired).
```
Report BOTH A and B per package; floor rule applies to **B**.

### 7.2 Targets (floor: no package below its 3.5-B baseline)

| Package | Baseline B | Target | Primary gap to close (QA scope) |
|---|---|---|---|
| internal/menu | 26.4% | **≥ 55%** | callback routing matrix, verifySetup failure branches, mute/type toggles, newCode range |
| internal/store | 51.8% | **≥ 70%** | LinkIdentity error matrix, EnsureTopic concurrency, codes expiry/mismatch, UserList/RecentEvents, PruneEvents SQL |
| internal/admin | 0% | **≥ 40%** | CLI end-to-end via `Run(args, buf, buf)` against a temp DB (no os.Exit paths for happy flows) |
| cmd/tgntfy | 0% | **≥ 20%** | via R1: `dispatch` flusher tests exercise the moved logic (cmd main() itself stays untested) |
| internal/catalog | 58.8% | ≥ 75% | Validate failure cases, Reload error keeps previous, ServiceTypes |
| internal/ingest | 70.0% | ≥ 80% | 413/429/409/500 paths, drop-rule combos, RenderBatch sharedURL |
| internal/tgbot | 66.7% | ≥ 75% | mock-Bot-API error paths (already mockable via TG_API_URL) |
| internal/transport | 78.9% | ≥ 85% | 429-retry loop, Forbidden fail-fast, queue-full drop |
| healthz/coalesce/limit/topic | ≥ baseline | ≥ baseline (no regression) | already high; optional polish |
| **TOTAL** | **42.8%** | **≥ 60%** | |

### 7.3 Scope split (binding)

- **backend card (t_ed26ffb7):** phases P1–P5 (code only, `internal/` + `cmd/`). May touch
  `itest/harness_test.go` **only** for P1 wiring. May add minimal *unit* tests for
  extracted helpers if a phase needs them. Must not add coverage-target tests — that is QA.
- **qa card (t_5a249aec):** runs after backend; owns all new tests (unit + itest),
  before/after coverage report vs 3.5-B baselines, and the MUST NOT BREAK verification
  against the mocked Bot API suite.

## 8. Suspected defects found (REPORT ONLY — fixing = behavior change, needs owner decision)

- **D1 (high value):** `coalesce.Add` captures the **HTTP request context**
  (ingest.go:352 `h.coalesc.Add(ctx, …)`); the flush timer fires ~5 s later and runs
  `batchFlusher.Flush` with that ctx — after the response was written, `http.Server`
  cancels it, so the flush's DB writes execute against a cancelled context in production.
  The itest harness never cancels, so the suite cannot see this. Suggested follow-up card
  (decouple flush ctx from request ctx). **Not fixed here.**
- **D2:** `/undelivered` (menu.go:470) calls `FailedDeliveries(ctx, 20)` **without** the
  user filter — lists *other users'* failed deliveries (privacy). Fix = behavior change.
- **D3:** retention is documented (v1 SPEC) but `PruneEvents` is wired to nothing; the
  `events` table grows unbounded and idempotency is therefore permanent (not 24 h).
- **D4:** 429 loop (transport.go:214 `attempt--`) has no cap — a persistent flood-limit
  retries forever; also `attempt--` at attempt 0 relies on the for-loop `attempt++` to
  land back at 0. Behavior-preserving status quo; document only.

## 9. MUST NOT BREAK (verbatim from epic; binding on every phase)

- /v1/events contract: POST with X-Service-Token, the single JSON envelope, response
  codes — FROZEN.
- Service tokens issued via admin CLI — behavior unchanged; user linking via POST
  /v1/link code flow unchanged.
- Per-chat paced delivery to Telegram forum topics (coalesce <=3, paced sending) —
  semantics byte-identical vs the mocked Bot API suite.
- Service-agnostic lazy topic creation (v1.1: service supplies name; topic created at
  service registration) unchanged.
- /healthz + admin endpoints unchanged.

## 10. Test plan outline (for QA)

1. **Regression gate after every phase:** `go vet ./... && go test ./...` (all 43 tests
   unchanged in assertions).
2. **Behavior-preservation evidence:** mocked-Bot-API itest (integration 8 + v11 5 +
   health_link 1) is the byte-identical-delivery oracle; do not touch its message
   assertions.
3. **New coverage tests** per 7.2 columns; error-path first (429/413/409/401/500,
   LinkIdentity matrix, verifySetup branches, admin CLI happy flows).
4. **Contract freeze check:** golden list of env vars (9), routes (4), error codes (11),
   success shapes (3), CLI subcommands (5 + 5 service verbs) — assert via a small itest
   or documented curl checklist.
5. **Before/after coverage report** in the method of 7.1, table format of 3.5 + targets
   met/not-met per package.

## 11. Out of scope (explicit)

Anything in 4-REJECTED and 8; schema/migration changes; dependency updates; Dockerfile;
README/DEPLOYMENT content rewrites (index added separately); logging format changes;
per-chat pacing; 429 cap; retention scheduler; `/undelivered` privacy fix; admin CLI
rework; performance work beyond what moves require.
