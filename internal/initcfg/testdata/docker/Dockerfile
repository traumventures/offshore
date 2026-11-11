FROM rust:1.85-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler libpq-dev \
    && rm -rf /var/lib/apt/lists/*
COPY . .
RUN cargo build --release
FROM debian:bookworm-slim
