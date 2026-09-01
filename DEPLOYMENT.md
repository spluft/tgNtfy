# DEPLOYMENT.md — tgNtfy deployment (Docker → GHCR → Portainer)

This covers building the Docker image, pushing it to GHCR, and running the
**tgNtfy gate** as a new Portainer stack on the host at **192.168.1.200**
(Portainer **endpoint 3**, the local docker.sock). A Portainer Managed note: the MCP /
API endpoint uses HTTPS `:9443` with self-signed TLS.

> **Before deploying** check the epic: this document is accurate for the *tgNtfy v1 gate*
> epic branch. The live deployment gate is the epic's E2E acceptance step.
>
> **v1.1 note:** `config/events.yaml` is now **optional** — the gate boots and operates
> with an empty or absent catalog. `CATALOG_PATH` is still the default path; an absent file
> is logged at debug level, not a startup error.

---

## 1. Build the image

The Dockerfile is a classic multi-stage build (no `#syntax`/`#include` — it is inlined
verbatim so the Portainer classic builder can use it directly):

```dockerfile
FROM golang:1.25.0-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/tgntfy ./cmd/tgntfy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tgntfy /usr/local/bin/tgntfy
COPY config/events.yaml /etc/tgntfy/events.yaml
ENV LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/usr/local/bin/tgntfy"]
```

Key properties:

- **CGO_ENABLED=0** (pure-Go SQLite, `modernc.org/sqlite`), **static** binary,
  `-trimpath -ldflags="-s -w"`.
- Runtime is **distroless static** — there is **no shell and no apk** at runtime; you
  cannot `apk add` anything or run scripts inside the container. Admin is the `tgntfy`
  binary itself (`docker exec … tgntfy admin …`).
- Target image size **~15 MB**.
- `config/events.yaml` is copied to `/etc/tgntfy/events.yaml` (the default
  `CATALOG_PATH`).

### Known host build constraints (192.168.1.200) — read first

1. **Bridge networking cannot reach the Alpine CDN** for `golang:1.25.0-alpine` / module
   downloads. Use the host build pattern:
   `POST /api/endpoints/3/docker/build?t=ghcr.io/spluft/tgntfy:<tag>&dockerfile=Dockerfile`
   with a tar context, OR run the builder on the host with
   `--network=host` plus `--add-host` entries for `proxy.golang.org`, `sum.golang.org`,
   `storage.googleapis.com`.
2. Because the runtime image is static and needs no package installs, the base image is
   pulled once; the **build stage** is where network reachability matters.

Build locally (if you have a working docker daemon / GOPROXY):

```sh
docker build -t ghcr.io/spluft/tgntfy:<tag> -f Dockerfile .
```

---

## 2. Push to GHCR

Requires a GitHub **spluft** token with `repo` + `write:packages` scope (stored at
`/opt/data/ghcr_token.txt`). GHCR tokens must be scoped for `package:write`; a token that
works on `api.github.com` but returns 403 on
`ghcr.io/token?scope=repository:spluft/tgntfy:push` is a **scope** problem, not a
bad token.

```sh
echo "$GHCR_TOKEN" | docker login ghcr.io -u spluft --password-stdin

docker tag ghcr.io/spluft/tgntfy:<tag> ghcr.io/spluft/tgntfy:<tag>
docker push ghcr.io/spluft/tgntfy:<tag>

# digest-pinned stacks must be re-tagged to the new digest:
docker inspect ghcr.io/spluft/tgntfy:<tag> --format='{{index .RepoDigests 0}}'
```

---

## 3. Portainer stack (endpoint 3 / 192.168.1.200)

Create a **new stack** named **`tgntfy`** in Portainer on endpoint 3 with this
compose file (adjust image tag / env as needed):

```yaml
services:
  tgntfy:
    image: ghcr.io/spluft/tgntfy:<tag>
    ports:
      - "8080:8080"
    environment:
      - TG_BOT_TOKEN=<botfather-token>       # REQUIRED; env only — keep out of the repo
      - LISTEN_ADDR=:8080
      - DB_PATH=/data/tgntfy.db
      - CATALOG_PATH=/etc/tgntfy/events.yaml
      - COALESCE_WINDOW_MS=5000
      # - ADMIN_TOKEN=...                    # optional: gates /api/health on X-Admin-Token
      # - LOG_FORMAT=json
      # - LOG_LEVEL=info
    volumes:
      - tgntfy-data:/data                   # SQLite persistence (volume must survive restarts)
    restart: unless-stopped
```

- **Ports:** `8080:8080` (ingest + `/api/health` + `/metrics`). Map to a host port of
  your choice if 8080 is taken.
- **SQLite volume:** the DB lives at `DB_PATH` (default `/data/tgntfy.db`) on the
  mounted `tgntfy-data:/data` volume. This is a **new, dedicated volume** — never reuse
  another service's volume.
- **Env wiring:** every var the binary reads is listed in `.env.example`; only
  `TG_BOT_TOKEN` is required for the server, the rest have safe defaults.
- **Health check** (spluft convention): `GET /api/health` → `200 {"status":"ok"}`.
  With `ADMIN_TOKEN` set it requires a matching `X-Admin-Token` header (else `401`);
  DB unreachable → `503`. From the host: `curl -s http://192.168.1.200:8080/api/health`.

Portainer API update flow (digest-pinned stacks must get the new digest/tag, not just the
tag — `PUT /api/stacks/{id}?endpointId=3` with `{"stackFileContent": "...", "pullImage": true}`
auto-redeploys; Portainer create/exec calls need `Content-Type: application/json`).

After deploy, verify from the host:
`docker inspect <container> --format='{{.State.Health.Status}}'` (or container status
`running`), and
`curl -s http://<host>:8080/api/health` →
`{"status":"ok"}`.

---

## 4. First-use after deploy

1. `curl :8080/api/health` → `ok`.
2. Issue a service token: `docker exec <container> tgntfy admin /data/tgntfy.db`
   `service create goyoutube --name "goYouTube"` (prints the raw token once).
3. Point the service at `POST /v1/events` with that `X-Service-Token`.
4. Do the `/setup` ritual in Telegram (see README) so events reach the user's forum group.

---

## Notes / gotchas

- **DNS is broken on the host docker daemon** (pinned to 8.8.8.8/8.8.4.4 where
  host UDP/53 is blocked); a plain build/one-shot container may time out on
  `lookup … on 8.8.4.4:53`. Use the host build / `networkmode=host` + `extrahosts`
  pattern above.
- **No shell in the runtime image** — use `docker exec <container> tgntfy admin …`, not sh commands.
- **Static binary** — all config is env + `CATALOG_PATH` file; there is nothing to
  install at runtime.
