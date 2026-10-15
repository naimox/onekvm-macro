# syntax=docker/dockerfile:1

# --- build ---
# The build toolchain does not ship in the final image, but keeping it on a
# maintained Go release avoids building with an end-of-life compiler. The module
# itself only declares go 1.23, so anything at or above that works.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# Module proxy, overridable at build time. The default is the mainland-China
# mirror because proxy.golang.org is often unreachable there.
ARG GOPROXY=https://goproxy.cn,direct
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go env -w GOPROXY="${GOPROXY}" && go mod download

COPY . .

RUN set -eux; \
    os="${TARGETOS:-linux}"; \
    if [ -n "${TARGETARCH}" ]; then arch="${TARGETARCH}"; else arch="$(go env GOHOSTARCH)"; fi; \
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /out/onekvm-macro .

# --- run ---
FROM alpine:3.20

# curl rather than busybox wget: the healthcheck below has to talk HTTPS when
# TLS is enabled, and busybox's wget does not reliably support skipping
# certificate verification, which is required for a self-signed certificate
# inside the container.
RUN apk add --no-cache ca-certificates tzdata curl

COPY --from=build /out/onekvm-macro /usr/local/bin/onekvm-macro

ENV PORT=8081 \
    TZ=Asia/Shanghai

EXPOSE 8081

# The service never writes to disk (sessions live in memory), so an unprivileged
# user costs nothing. Note that TLS_CERT / TLS_KEY must therefore be readable by
# this user — a root-owned 0600 key file will not work.
USER nobody

# Probe whichever scheme the service is actually serving. Hardcoding http here
# would mark the container unhealthy as soon as TLS is enabled, and -k is needed
# because a self-signed certificate cannot be validated from inside.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD if [ -n "$TLS_CERT" ]; then curl -fsSk -o /dev/null "https://127.0.0.1:${PORT}/healthz"; else curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/healthz"; fi || exit 1

ENTRYPOINT ["/usr/local/bin/onekvm-macro"]
