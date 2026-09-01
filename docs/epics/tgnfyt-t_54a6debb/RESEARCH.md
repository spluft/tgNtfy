# Epic t_54a6debb — tgNtfy: research (язык, единый API-контракт, топология доставки)

**Проект:** tgNtfy (`github.com/spluft/tgNtfy`)
**Board:** spluft · **Default branch:** main
**Тип:** RESEARCH / DESIGN — код НЕ пишется, деплой НЕ требуется
**Статус:** черновик-вход, дополняется architect-стадией
**Открытое решение (нужен Nikolay):** §7 — топология доставки (DM vs группа).

---

## 1. Контекст и цель

spluft — 5 self-hosted сервисов на 192.168.1.200 (Portainer): goYoutube,
goMailClient, goRecomendarr, goVpnWork, kidsEdu. Каждый живёт своей жизнью:
у goRecomendarr и goVpnWork уже есть свои TG-боты (продуктовые чат-фичи), у
остальных уведомлений нет вообще. Цель — **один bot-gate**:

1. сервисы шлют события в единый HTTP-контракт;
2. tgNtfy решает, КТО и ЧТО хочет получать (мульти-юзер, self-serve);
3. доставка в Telegram по топикам/категориям;
4. будущие сервисы подключаются одной строкой кода (POST), без изменения гейта.

Исследовать: (R1) язык, (R2) единый контракт, (R3) топология доставки.

### Инвентарь событий-источников (сверено с кодом, 2026-09-01)

| Сервис | Стек | Источник событий | События |
|---|---|---|---|
| goYoutube | Go | `internal/events` хаб, `pool.go` emit | `job_queued/started/progress/completed/failed/cancelled/paused/resumed` |
| goMailClient | Go | `internal/events` хаб (per-userID) + WS | `new-mail`, `sync-status`, `sent`, `flags-changed` |
| goRecomendarr | Go | NATS JetStream `integration.event.*` | `download_started`, `download_completed` (JobID, CanonicalID), `job_failed` (ErrorCode, Message) |
| goVpnWork | Go | monitor `ChatSender.Send` | "✅ VPN connected" / "⚠️ VPN disconnected" (сейчас → 1 chatID из env) |
| kidsEdu | Kotlin/Spring | Spring (JWT-auth, progress-сервисы) | кандидаты: task submitted, progress, daily summary — зафиксировать в impl-эпике |

Мульти-юзерность у источников: goYoutube/goMailClient — users table
(Username, ErrUserLimit); goRecomendarr — `telegram_links` (chat ↔ service user,
команда `/link`, login-code); goVpnWork — single-user (1 chatID); kidsEdu — JWT per-user.
**Следствие:** в гейте «user» = привязка Telegram-идентификации к identity
каждого сервиса (см. §6.1).

---

## 2. R1 — Выбор языка

| Критерий | **Go** | Python | Node/TS |
|---|---|---|---|
| Консистентность стека spluft | ✅ 4/5 сервисов на Go | ❌ | ❌ |
| One-binary, small image (алpine ~10-15 MB) | ✅ | ❌ (слои venv/депсов) | ⚠️ (node:alpine ок, но) |
| Долгий процесс: getUpdates-поллинг + async queue + NATS client | ✅ goroutines | ⚠️ asyncio — ок, но | ✅ |
| Библиотеки TG Bot API | ✅ go-telegram-bot-api / tgbotgo | ✅ python-telegram-bot | ✅ grammY (зрелая) |
| SQLite/Postgres драйверы | ✅ | ✅ | ✅ |
| Команда/операция (Dockerfile-паттерн, GOPROXY, build-скрипты уже настроены) | ✅ готовый конвейер | ⚠️ | ⚠️ |
| Скорость прототипирования | ⚠️ медленнее | ✅ | ✅ |

**Рекомендация: Go.**
Обоснование: (a) 4/5 сервисов на Go — один и тот же Dockerfile-паттерн, те же
GOPROXY/деплой-скрипты, тот же стиль (slog, internal/); (b) гейт — долгий
процесс с очередями (polling + dispatch + retry) — goroutines идеальны;
(c) один статический бинарь → маленький образ, быстрый push на GHCR
(узел с медленным аплинком ~100 KB/s — размер образа реально важен);
(d) NATS-клиент для goRecomendarr-пути и SQLite — стандартные, зрелые
(библиотеки: `github.com/telego/tg` или go-telegram-bot-api; NATS.go;
modernc.org/sqlite — без cgo).
Python-вариант (python-telegram-bot) жизнеспособен, если приоритет — скорость
прототипа, но ломает конвейер сборки и требует venv-слоёв. Node/TS (grammY) —
только если решим, что UI-веб-панель гейта на React (сейчас UI = TG-меню,
веб не нужен).

---

## 3. R2 — Единый API-контракт

### 3.1 Envelope (все сервисы, один формат)

`POST /v1/events` (HTTP/HTTPS, внутри LAN / по VPN):

```json
{
  "v": 1,
  "event_id": "goyoutube:job:4821:completed:2026-09-01T11:22:33Z",  // idempotency key
  "service": "goyoutube",          // зарегистрированный id сервиса
  "user_ref": "17",                // identity пользователя в этом сервисе (string!)
  "type": "job_completed",         // из каталога событий сервиса (§3.3)
  "severity": "info",              // info | warn | error | success
  "title": "Видео скачано",
  "text": "«That Italy from Pinterest | Where Billionaires Vacation» (10:47)",
  "url": "https://goyt.spluft.ru/#/queue/4821",
  "metadata": {"job_id": 4821, "channel": "UyeF5cGujes"}
}
```

Правила:
- `event_id` обязателен → гейт делает idempotent-окно (дубли в 24ч игнорируются).
- `user_ref` — строка (в разных сервисах identity разные: int/username/external).
  Для событий без конкретного пользователя (goVpnWork) — `user_ref: "admin"`.
- `metadata` — свободный JSON, гейт не интерпретирует (кроме §3.4 группировки).
- Ошибки: 401 (неверный сервис-ключ), 400 (схема), 409 (дубль event_id), 413
  (> 8 КБ), 429 (rate-limit на сервис: 30 ev/s burst, 100/мин).

### 3.2 Auth и транспорт

- **Первичный (рекомендация):** HTTP POST + `X-Service-Token` — статический
  HMAC-токен на сервис, выдаётся админом гейта (`/admin services`). Простота:
  сервису нужна одна константа в env.
- **Альтернатива (если нужен NATS):** гейт подписывается на общий NATS JetStream
  (`events.<service>.*`) — актуально только для goRecomendarr (NATS уже есть).
  Решение impl-эпика: начать с HTTP-only, NATS-адаптер позже.
- TLS: не обязателен внутри LAN; если гейт будет доступен наружу (PWA-опрос,
  внешние сервисы) — реверс-прокси с TLS (паттерн gorec.spluft.ru).

### 3.3 Каталог событий (event types) — маппинг из каждого сервиса

| service | type | severity | text (шаблон) |
|---|---|---|---|
| goyoutube | job_completed | success | «{title}» скачано ({duration}) |
| goyoutube | job_failed | error | Ошибка: «{title}» — {error} |
| goyoutube | job_cancelled | info | Отменено: «{title}» |
| gomail | new-mail | info | «{from}» — {subject} |
| gomail | sync-status | warn | Синхронизация {account}: {status} |
| gorecomendarr | download_completed | success | Готово: {canonical} |
| gorecomendarr | job_failed | error | Ошибка загрузки: {code} {message} |
| govpn | vpn_connected | success | VPN подключён |
| govpn | vpn_disconnected | error | VPN отключён — сервис недоступен |
| kidsedu | * | * | определяется в impl-эпике |

Каталог — конфигурация гейта (`events.yaml`), расширяется без ре-релиза.

### 3.4 Маршрутизация (гейт, после валидации)

1. `service` + `user_ref` → строки `service_users` (привязка identity).
2. Для каждого привязанного TG-пользователя: filter по подпискам
   (`subscriptions.service` + `event_types` — пусто = все).
3. Группировка: `coalesce`-окно (по умолчанию 5 c; job_progress — всегда drop)
   → 1 TG-сообщение на событие (или батч).
4. В очередь доставки → dispatcher.

---

## 4. R3 — Топология доставки (ключевой открытый вопрос)

Telegram-факты (Bot API, подтвердить architect-стадией live):
- **Forum/supergroup**: `is_forum`, топики = `message_thread_id`; бот создаёт
  топики `createForumTopic` (нужна права admin `can_manage_topics`); у каждого
  топика свой mute/звук/DND. `getUpdates` приносит `message_thread_id` —
  подписка на "только мои" события тривиальна.
- **DM-чат бот↔user**: простейший, 0 настройки, но: нет топиков, mute — на весь
  бота.
- Лимиты: ~30 msg/min на один чат (rate limit бота), ~20 msg/s глобально.
  Батчинг обязателен.
- Группу можно создать: (a) вручную человеком + бот admin, (b) ботом
  `createChatInviteLink` (но создание группы — только человеком, бот не
  создаёт группы без человека).

### Варианты (4)

**A. Общий форум-чат + топики на сервис** (1 группа, топики goYoutube/Mail/VPN/…)
- ✅ 1 чат у всех; на топис свой mute.
- ❌ **Мульти-юзерность сломана**: подписки per-user невозможны — событие
  user-A видно user-B (приватные данные: письма, видео). Не вариант при
  >1 пользователе.

**B. Общий форум-чат + топики на пользователя**
- ❌ топики = юзеры, а события разные сервисов в одном топике — mute "всё
  от goYoutube" невозможен; спам "кто есть кто" в заголовке. Плохой UX.

**C. Группа на каждого пользователя (forum), топики = сервисы**
- ✅ идеальная изоляция: группа видна только владельцу (private, invite-link
  через бота) + полный per-service mute/звук (топики).
- ✅ масштабируется: новый сервис = новый топис в существующих группах.
- ⚠️ UX входа: пользователю нужно 1× создать группу, добавить бота admin,
  `/connect <code>` — ~30 секунд. Для 2–5 пользователей — приемлемо.
- ⚠️ бот НЕ создаёт группу за человека — нужен ритуал (команда `/setup` с
  пошаговыми инструкциями + генерацией invite-линка).

**D. DM-чат 1:1 (без групп)**
- ✅ UX входа 0 секунд: `/start` → готово. Мгновенный push.
- ❌ нет per-service mute (mute = весь бот). При 5+ сервисах и разных
  частотах (new-mail 20/день vs VPN down 1/месяц) — быстро станет шумным.
- ✅ при 1–2 пользователях — самый простой рабочий вариант.

**E. Гибрид (рекомендация):** DM по умолчанию + опциональная персональная
forum-группа.
- `/start` → сразу DM-доставка (0 настройки).
- `/setup` → ритуал персональной forum-группы; после неё события идут в
  топики по сервисам, DM отключается для этого юзера.
- Пер-юзер флаг `delivery_mode: dm | group`.
- Это закрывает и "1 пользователь - 1 чат" (DM) и "топики по сервисам"
  (группа), с явным upgrade-путём без миграций.

### Рекомендация

**E (гибрид).** Для текущей реальности (1–2 человека: Nikolay + возможно
family/kids) фактически старт на **D (DM)**, архитектура с первого дня с
`delivery_mode` и `message_thread_id` в `subscriptions` — группа C
включается командой, когда DM станет шумным. Вариант A отклоняется
(приватность), B — отклоняется (UX).

**⚠️ Решение Nikolay (эскалация):** D / E / C. Если "всегда только один
человек (я)" — достаточно D + фильтр типов событий в /menu, группы вообще
не строить (минус ~15% работы impl-эпика).

---

## 5. Архитектура (для impl-эпика)

```
[goYoutube] [goMailClient] [goRecomendarr] [goVpnWork] [kidsEdu]
     └──────────────┬──────────────────────────────────────┘
                    │ POST /v1/events (X-Service-Token)
                    ▼
        ┌─────────────────────────┐
        │      tgNtfy (1 cont.)   │
        │ ingest → validate →     │
        │ idempotent → route →    │
        │ coalesce → queue        │
        │        (SQLite/PG)      │
        │ dispatcher (getUpdates) │
        │   → TG Bot API          │
        │ admin API (tokens,      │
        │   users, services)      │
        └─────────────────────────┘
                    │
                    ▼
            Telegram (bot)
              DM / forum topics
```

Модули (Go, internal/): `ingest` (HTTP), `router` (subscriptions),
`coalesce` (grouping windows), `store` (SQLite: users, service_users,
subscriptions, events, deliveries), `tgbot` (getUpdates long-poll, sendMessage
with message_thread_id, menu commands), `admin` (token/service mgmt CLI+API),
`healthz` (`/api/health` по конвенции).

### Data model (черновик)

```
users(tg_user_id PK, username, delivery_mode, first_seen, last_event_at)
services(service PK, display_name, token_hash, enabled)
service_users(service, user_ref, user_id FK, status, linked_at)  -- identity mapping
subscriptions(user_id FK, service, event_types JSON/NULL=all, muted, updated_at)
events(id PK, event_id UQ, service, user_ref, type, severity, title, text, url, metadata, received_at)
deliveries(id PK, user_id, event_id, tg_msg_id, thread_id, status, attempts, last_err)
```

### Non-functional
- **Idempotency:** event_id UQ, окно 24ч.
- **Rate limit:** per-service 30/s burst, 100/min; TG-лимиты: батч ≤ 30/min/чат.
- **Delivery guarantee:** очередь + retry (exp backoff 5s..5min, 5 попыток) +
  `/undelivered` (admin-команда: показать/повторить).
- **Progress-события (job_progress) — drop by default** (шум; в impl можно
  включить "heartbeat каждые 10 мин" для долгих скачиваний).
- **Наблюдаемость:** `/api/health` (200/401 по конвенции spluft), slog,
  Prometheus-метрики (events_in, deliveries_ok/fail, queue_depth).
- **Деплой (impl-эпик):** 1 контейнер, Portainer на .200, образ ~15 MB,
  SQLite-volume; TG-токен и сервис-токены в env (не в репо).

---

## 6. Мульти-юзерный onboarding (self-serve)

### 6.1 Identity linking (как сервис-юзер = TG-юзер)
1. **Code-link (основной):** в TG `/link` → 6-значный одноразовый код (паттерн
   goRecomendarr /login, проверен) → пользователь вставляет код в веб-UI своего
   сервиса (goyoutube: Profile, gomail: Settings) → сервис шлёт
   `POST /v1/link {service, user_ref, code}` → гейт биндит.
2. **Direct (1-юзерные сервисы):** goVpnWork — сервис-юзер = `admin` = первый
   `/start` (auto-link).
3. **Admin:** `/admin link <service> <user_ref> @tguser` (для сервисов без
   веб-UI-пункта, пока kidsEdu-интеграция не готова).

### 6.2 Self-serve меню (TG Bot API inline keyboards)
- `/start` — welcome + (если нет линков) `/link`.
- `/menu` — список сервисов → чекбоксы event types (toggle per type), muted.
- `/status` — последние N доставленных событий, undelivered count.
- `/setup` — (вариант E) ритуал forum-группы.
- `/help`.
- Все mутации только от привязанного TG-user (проверка users.tg_user_id).

### 6.3 Безопасность
- Service tokens: 32B random, в БД hash (sha256), ротация `/admin rotate <service>`.
- Приватность: событие user-A **никогда** не уходит в чат user-B (изоляция по
  subscriptions — ключевая гарантия, в отличие от варианта A).
- TG-токен: только в env гейта; в репо — placeholder + .env.example.

---

## 7. Открытые решения (эскалация Nikolay)

| # | Вопрос | Варианты | Рекомендация |
|---|---|---|---|
| D-1 | Топология доставки | DM / DM+group(gibrid) / group | **E — гибрид** (DM сейчас, группа позже); если 1 юзер — только DM |
| D-2 | NATS-адаптер для goRecomendarr в v1? | да/нет | нет — HTTP-адаптер (ingest service уже ходит в NATS, 10 строк) |
| D-3 | Прогресс-события (job_progress) | drop / heartbeat 10min | drop в v1, heartbeat — опция подписки |
| D-4 | KidsEdu в v1? | да/нет | нет — контракт готов, интеграция отдельным тикетом (нужен Spring-хук) |

## 8. Out of scope (для этого рисерча и v1)

- Веб-UI гейта (TG-меню достаточно; PWA — позже).
- ntfy/Gotify-транспорт (решено: только Telegram; транспорт-абстракция
  оставляем в `dispatcher` interface — добавить канал = 1 реализация).
- Пер-сервис кастомные шаблоны сообщений (v1: каталог + title/text от сервиса).
- Sharding/multi-instance (1 контейнер, SQLite; масштаб — позже при >10k ev/день).

## 9. Критерии готовности рисерча (acceptance)

- [ ] Р1 зафиксирован: Go (+ обоснование).
- [ ] Р2: envelope + auth + каталог событий для 4 готовых сервисов (kidsEdu — плейсхолдер) + rate/idempotency правила.
- [ ] Р3: 4 варианта проанализированы, рекомендация + upgrade-path, решение Nikolay по D-1 получено.
- [ ] Data model + архитектура модулей + NFR зафиксированы как основа impl-эпика.
- [ ] Документ на main, запушен в GitHub.
