ARG GO_VERSION=1.25.2

# Stage 1: Builder
FROM golang:${GO_VERSION}-bookworm AS builder

ARG XCADDY_VERSION
ARG CADDY_VERSION
ARG CADDY_L4_VERSION

RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@v${XCADDY_VERSION}

WORKDIR /src
COPY . .

RUN xcaddy build v${CADDY_VERSION} \
    --with github.com/honest-hosting/caddy-consul=/src \
    --with github.com/mholt/caddy-l4@${CADDY_L4_VERSION} \
    --output /usr/local/bin/caddy

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/caddy /usr/local/bin/caddy

EXPOSE 80 443

CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile"]
