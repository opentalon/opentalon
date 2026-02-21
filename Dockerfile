FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/opentalon/opentalon/internal/version.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
                     -X github.com/opentalon/opentalon/internal/version.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo none) \
                     -X github.com/opentalon/opentalon/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /opentalon ./cmd/opentalon/

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S opentalon \
    && adduser -S -G opentalon opentalon

COPY --from=builder /opentalon /usr/local/bin/opentalon

USER opentalon

ENTRYPOINT ["opentalon"]
