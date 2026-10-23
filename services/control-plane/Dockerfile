# syntax=docker/dockerfile:1
#
# Build context: services/control-plane/, plus a named context `schema`: the
# repo's schema/v2/ (compose: additional_contexts; CI: build-contexts).

FROM rust:1 AS local

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake curl \
    && rm -rf /var/lib/apt/lists/*

RUN cargo install cargo-watch

ENV PLAT5_SCHEMA_DIR=/schema/v2
COPY --from=schema . /schema/v2
COPY Cargo.toml Cargo.lock build.rs ./
RUN mkdir -p src && echo "fn main() {}" > src/main.rs \
    && cargo build \
    && rm -rf src \
    && rm -f target/debug/deps/control_plane* \
    && mkdir -p src && echo "fn main() {}" > src/main.rs

COPY src ./src
COPY migrations ./migrations
RUN cargo build

CMD ["cargo", "watch", "-x", "run"]

FROM lukemathwalker/cargo-chef:latest-rust-1 AS chef
WORKDIR /app

FROM chef AS planner
COPY Cargo.toml Cargo.lock build.rs ./
COPY src ./src
COPY migrations ./migrations
RUN cargo chef prepare --recipe-path recipe.json

FROM rust:1 AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential cmake \
    && rm -rf /var/lib/apt/lists/*

RUN cargo install cargo-chef --locked

COPY --from=planner /app/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json

ENV PLAT5_SCHEMA_DIR=/schema/v2
COPY --from=schema . /schema/v2
COPY Cargo.toml Cargo.lock build.rs ./
COPY src ./src
COPY migrations ./migrations
RUN cargo build --release

FROM debian:bookworm-slim AS prod

ARG VERSION=dev
ARG REVISION=
ARG SOURCE=https://github.com/plat5dev/plat5

LABEL org.opencontainers.image.title="control-plane" \
      org.opencontainers.image.description="Plat5 control plane" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -r app && useradd -r -g app app

COPY --from=builder /app/target/release/control-plane ./control-plane

USER app

EXPOSE 5002

CMD ["./control-plane"]
