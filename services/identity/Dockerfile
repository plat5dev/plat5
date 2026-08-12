# ======================================================================
# Local Stage for development
FROM golang:1.25-bookworm AS local

WORKDIR /app

RUN GOTOOLCHAIN=auto go install github.com/air-verse/air@v1.62.0

COPY go.mod go.sum ./
RUN go mod download

COPY . .

CMD ["air", "-c", ".air.toml"]

# ======================================================================
# Builder Stage
FROM golang:1.25-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -o server ./

# ======================================================================
# Prod Stage
FROM debian:bookworm-slim AS prod

ARG VERSION=dev
ARG REVISION=
ARG SOURCE=https://github.com/plat5dev/plat5

LABEL org.opencontainers.image.title="identity" \
      org.opencontainers.image.description="Plat5 identity service" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

# Install runtime dependencies:
# - ca-certificates: Required for HTTPS certificate validation
# - curl: Enables container healthchecks
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r app && useradd -r -g app app

COPY --from=builder /app/server ./server

USER app

EXPOSE 3000

CMD ["./server"]
