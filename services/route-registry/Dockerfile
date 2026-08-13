# syntax=docker/dockerfile:1
#
# Build context: services/route-registry/

FROM rust:1 AS local

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake curl protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

RUN cargo install cargo-watch

COPY Cargo.toml Cargo.lock ./
RUN mkdir -p src && echo "fn main() {}" > src/main.rs \
    && cargo build \
    && rm -rf src \
    && rm -f target/debug/deps/route_registry* \
    && mkdir -p src && echo "fn main() {}" > src/main.rs

COPY src ./src
COPY migrations ./migrations

CMD ["cargo", "watch", "-x", "run"]

FROM lukemathwalker/cargo-chef:latest-rust-1 AS chef
WORKDIR /app

FROM chef AS planner
COPY Cargo.toml Cargo.lock ./
COPY src ./src
COPY migrations ./migrations
RUN cargo chef prepare --recipe-path recipe.json

FROM rust:1 AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*

RUN cargo install cargo-chef --locked

COPY --from=planner /app/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json

COPY Cargo.toml Cargo.lock ./
COPY src ./src
COPY migrations ./migrations
RUN cargo build --release

FROM debian:bookworm-slim AS prod

ARG VERSION=dev
ARG REVISION=
ARG SOURCE=https://github.com/plat5dev/plat5

LABEL org.opencontainers.image.title="route-registry" \
      org.opencontainers.image.description="Plat5 route registry" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r app && useradd -r -g app app

COPY --from=builder /app/target/release/route-registry ./route-registry

USER app

EXPOSE 5002

CMD ["./route-registry"]
