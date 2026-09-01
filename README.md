# tgNtfy

Единый Telegram bot-gate для сервисов spluft: принимает события от
сервисов и доставляет их пользователям в Telegram, по топикам, с
мультипользовательским режимом (самообслуживание: авторизация в боте,
выбор сервисов и типов уведомлений).

## Статус

Проект в фазе **рисерча** — см. `docs/epics/`.

## Сервисы-источники событий (spluft)

| Сервис | Стек | Механизм событий |
|---|---|---|
| goYoutube | Go | events-хаб (job_completed / job_failed / ...) |
| goMailClient | Go | events-хаб + WebSocket (new-mail / sync-status / sent) |
| goRecomendarr | Go (NATS JetStream) | integration.event.download_started/completed, job_failed |
| goVpnWork | Go | monitor ChatSender (VPN connected/disconnected) |
| kidsEdu | Kotlin/Spring Boot | Spring events / REST (определяется в рисерче) |

## Документация

- `docs/epics/` — эпики (SPEC/RESEARCH) по конвенции spluft.
