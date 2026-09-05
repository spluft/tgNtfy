# tgNtfy — Documentation Index

English index of all project documentation. tgNtfy is the unified notification gate
(see the root [README.md](../README.md)). Every doc below carries a **status** and a
**last-verified date**; when a doc and the code disagree, the newest SPEC wins and this
index tells you which one that is.

Contract docs (the day-to-day reference) live at the **repo root** and are the place to
start:

| Doc | What it covers | Status | Last verified |
|-----|----------------|--------|---------------|
| [README.md](../README.md) | Product overview, API contract (`POST /v1/events`, `POST /v1/link`), status codes, `/setup` walkthrough, admin CLI, env vars, observability | CURRENT (v1.1) | 2026-09-02 |
| [DEPLOYMENT.md](../DEPLOYMENT.md) | Docker build → GHCR push → Portainer stack (host 192.168.1.200, endpoint 3), host build constraints | CURRENT | 2026-09-02 |
| [.env.example](../.env.example) | Canonical environment-variable list with defaults | CURRENT | 2026-09-02 |
| [config/events.yaml](../config/events.yaml) | Optional catalog file format (severity/drop hints) | CURRENT (optional since v1.1) | 2026-09-02 |

## Epic archive (`docs/epics/`)

Epic docs are historical design records. They are kept as written (substance is never
deleted); staleness is handled by labeling. Grouped by status:

### Superseded / historical (read for rationale, not for current behavior)

| Doc | Status | Last verified | Note |
|-----|--------|---------------|------|
| [t_352cddfe/SPEC.md](epics/tgnfyt-t_352cddfe/SPEC.md) | SUPERSEDED IN PART — the binding v1 implementation SPEC; its bindings remain in force **except where amended by the v1.1 spec** (lazy topic creation, optional catalog, coalesce AC ≤3) | 2026-09-01 | Shipping: v1 gate, merged to main. |
| [t_54a6debb/RESEARCH.md](epics/tgnfyt-t_54a6debb/RESEARCH.md) | SUPERSEDED — pre-design research (Russian). Its delivery-topology decision (per-user forum group + topic per service) was settled and is implemented; the "open decision §7" it lists has been resolved. | 2026-09-01 | Written in Russian; kept for provenance. |

### Active (the governing spec at the time of writing)

| Doc | Status | Last verified | Note |
|-----|--------|---------------|------|
| [t_a86c33cd/SPEC.md](epics/tgnfyt-t_a86c33cd/SPEC.md) | BINDING (v1.1) — service-agnostic lazy topic creation, optional catalog; amends the v1 spec where stated | 2026-09-02 | Shipping: v1.1, merged to main (tip 48d6964). |
| [t_2d992300/SPEC.md](epics/tgnfyt-t_2d992300/SPEC.md) | BINDING for epic t_2d992300 — deep analysis, baseline coverage, behavior-preserving refactoring plan | 2026-09-05 | In progress: refactoring epic (no contract changes). |

## Precedence rule

For current behavior: **root README.md + DEPLOYMENT.md** first; then the newest epic
SPEC in the "Active" group; older SPEC sections are binding only where the newer spec
does not amend them. For "why was it designed this way": the epic archive, newest first.
