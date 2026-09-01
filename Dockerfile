# tgNtfy v1 — multi-stage static build. Classic builder Dockerfile (no #syntax/#include).
# Target ~15 MB static binary; runtime needs no apk add (golang-alpine provides ca-certs).

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