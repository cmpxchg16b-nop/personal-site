# Multi-stage build for the personal-site server.
#
# The builder always runs on the build host's native platform ($BUILDPLATFORM)
# and uses Go's built-in cross-compilation — via the TARGETOS/TARGETARCH args
# that buildx injects — to emit a static binary for each requested target. This
# means multi-arch builds (linux/amd64 + linux/arm64) need no QEMU emulation:
# the builder runs natively every time and Go handles the arch.
#
# The web assets (web/site/out) must exist in the build context; they are
# embedded into the binary at compile time via //go:embed all:web/site/out.
# Build them first (cd web/site && npm ci && npm run build) or let CI do it.
#
# Build a multi-arch image:
#   docker buildx build --platform linux/amd64,linux/arm64 -t personal-site .

# --- Builder -----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /src

# Module files first so go mod download is cached across source changes.
COPY go.mod go.sum ./
RUN go mod download

# Rest of the source, including web/site/out for the embed.
COPY . .

# Declared here (after go mod download) so preceding layers stay cached across
# target architectures.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /personal-site ./cmd/server

# --- Runtime -----------------------------------------------------------------
FROM scratch

LABEL org.opencontainers.image.title="personal-site"

WORKDIR /app

# Run as a non-root user (numeric UID; no /etc/passwd needed with scratch).
USER 65532:65532

COPY --from=builder /personal-site /usr/local/bin/personal-site

EXPOSE 8080

# The runtime image is scratch — no shell, no curl — so the health check
# reuses the server binary itself: `serve --healthz-probe` GETs /api/healthz
# on the loopback address and exits non-zero on failure. The probe port
# follows the ADDR environment variable, like the server's --addr flag.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/personal-site", "serve", "--healthz-probe"]

ENTRYPOINT ["/usr/local/bin/personal-site"]
