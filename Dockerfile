# ─── Build stage ──────────────────────────────────────────────────────────────
#
# Install by branch (default: master):
#   docker build .
#   docker build --build-arg BRANCH=develop .
#
# Install by release / tag:
#   docker build --build-arg VERSION=v1.2.3 .
#   (VERSION takes precedence over BRANCH when set)
#
FROM golang:1.24-alpine AS builder

ARG BRANCH=master
ARG VERSION=

RUN apk add --no-cache git

WORKDIR /src

# Clone from GitHub — use VERSION (tag/release) if set, otherwise BRANCH.
RUN if [ -n "${VERSION}" ]; then \
      echo "Building release ${VERSION}" && \
      git clone --depth 1 --branch "${VERSION}" https://github.com/opentalon/opentalon.git .; \
    else \
      echo "Building branch ${BRANCH}" && \
      git clone --depth 1 --branch "${BRANCH}" https://github.com/opentalon/opentalon.git .; \
    fi

RUN go mod download

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X github.com/opentalon/opentalon/internal/version.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
      -X github.com/opentalon/opentalon/internal/version.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo none) \
      -X github.com/opentalon/opentalon/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /opentalon ./cmd/opentalon/


# ─── Runtime stage ────────────────────────────────────────────────────────────
FROM golang:1.24-alpine

RUN apk add --no-cache ca-certificates tzdata git \
    && addgroup -S opentalon \
    && adduser -S -G opentalon opentalon

COPY --from=builder /opentalon /usr/local/bin/opentalon

# Mount your config.yaml here at runtime (see docker-compose.yml for example).
# VOLUME ["/config"]

# Persistent state: session history, downloaded plugins/channels/skills.
# VOLUME ["/home/opentalon/.opentalon"]
# EXPOSE 3978

USER opentalon

ENTRYPOINT ["opentalon"]

# Default: load config from the mounted /config volume.
# Override at runtime: docker run ... opentalon -config /path/to/config.yaml
# CMD ["-config", "/config/config.yaml"]
