# syntax=docker/dockerfile:1
#
# cloudsourced control plane image.
#
#   make image                       # cloudsourced-controlplane:<version> and :latest
#
# The runtime contains the static cloudsourced binary, the Docker CLI (the
# providers build environment images with it; local-docker also runs
# machines with it) and CA certificates. It runs as the unprivileged user
# cloudsourced (uid 10001); the Docker socket of the host is passed in with
# --group-add <gid of the socket>. See docs/CONTROLPLANE.md.
#
# The control plane runs linux/amd64; the server binary it hands to amd64
# machines is its own executable, arm64 machines get
# /usr/local/bin/cloudsourced-linux-arm64.

ARG GO_VERSION=1.26
ARG DOCKER_VERSION=29

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cloudsourced ./cmd/cloudsourced \
 && GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cloudsourced-linux-arm64 ./cmd/cloudsourced

FROM docker:${DOCKER_VERSION}-cli AS runtime
# setcap lets the unprivileged user bind :443 and :80 (ACME) without root.
RUN apk add --no-cache ca-certificates libcap-utils \
 && adduser -D -H -u 10001 -h /var/lib/cloudsourced cloudsourced \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/cloudsourced
COPY --from=build /out/cloudsourced /usr/local/bin/cloudsourced
# Server binary of arm64 machines (aws-ec2 sizes with arch = "arm64").
COPY --from=build /out/cloudsourced-linux-arm64 /usr/local/bin/cloudsourced-linux-arm64
RUN setcap cap_net_bind_service=+ep /usr/local/bin/cloudsourced \
 && apk del libcap-utils
ENV CLOUDSOURCED_STATE_DIR=/var/lib/cloudsourced \
    CLOUDSOURCED_CONFIG=/var/lib/cloudsourced/config.toml \
    CLOUDSOURCED_IN_CONTAINER=1 \
    HOME=/var/lib/cloudsourced
USER 10001:10001
WORKDIR /var/lib/cloudsourced
VOLUME /var/lib/cloudsourced
# 7443: API with the self-signed certificate; 443 + 80: API and ACME HTTP-01
# with --hostname.
EXPOSE 7443 443 80
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s CMD ["cloudsourced", "controlplane", "health"]
ENTRYPOINT ["cloudsourced"]
CMD ["controlplane"]
