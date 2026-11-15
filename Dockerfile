# One image holds every binary; each Compose service picks one with
# `command`. Build: docker build -t aegis .

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.22
# wget (part of busybox) is used by the Compose health checks.
RUN adduser -D -H -u 10001 app
COPY --from=build /out/ /app/
COPY deploy/gateway.yaml /etc/gateway/gateway.yaml
USER app
ENTRYPOINT []
CMD ["/app/gateway", "-config", "/etc/gateway/gateway.yaml"]
