# syntax=docker/dockerfile:1
#
# offshore control plane image.
#
#   make image                       # offshore-controlplane:<version> and :latest
#
# The runtime contains the static offshore binary, the Docker CLI (the
# providers build environment images with it; local-docker also runs
# machines with it) and CA certificates. It runs as the unprivileged user
# offshore (uid 10001); the Docker socket of the host is passed in with
# --group-add <gid of the socket>. See docs/control-plane.md.
#
# The control plane runs linux/amd64; the server binary it hands to amd64
# machines is its own executable, arm64 machines get
# /usr/local/bin/offshore-linux-arm64.

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
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/offshore ./cmd/offshore \
 && GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/offshore-linux-arm64 ./cmd/offshore

FROM docker:${DOCKER_VERSION}-cli AS runtime
# setcap lets the unprivileged user bind :443 and :80 (ACME) without root.
RUN apk add --no-cache ca-certificates libcap-utils \
 && adduser -D -H -u 10001 -h /var/lib/offshore offshore \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/offshore
COPY --from=build /out/offshore /usr/local/bin/offshore
# Server binary of arm64 machines (aws-ec2 sizes with arch = "arm64").
COPY --from=build /out/offshore-linux-arm64 /usr/local/bin/offshore-linux-arm64
RUN setcap cap_net_bind_service=+ep /usr/local/bin/offshore \
 && apk del libcap-utils
ENV OFFSHORE_STATE_DIR=/var/lib/offshore \
    OFFSHORE_CONFIG=/var/lib/offshore/config.toml \
    OFFSHORE_IN_CONTAINER=1 \
    HOME=/var/lib/offshore
USER 10001:10001
WORKDIR /var/lib/offshore
VOLUME /var/lib/offshore
# 7443: API with the self-signed certificate; 443 + 80: API and ACME HTTP-01
# with --hostname.
EXPOSE 7443 443 80
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s CMD ["offshore", "controlplane", "health"]
ENTRYPOINT ["offshore"]
CMD ["controlplane"]
