ARG GO_VERSION=1.25.2

# Stage 1: Builder
FROM golang:${GO_VERSION}-bookworm AS builder

ARG XCADDY_VERSION
ARG CADDY_VERSION

RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@v${XCADDY_VERSION}

WORKDIR /src
COPY . .

# caddy-consul manages TCP/L4 listeners in-process (no caddy-l4 dependency).
RUN xcaddy build v${CADDY_VERSION} \
    --with github.com/honest-hosting/caddy-consul=/src \
    --output /usr/local/bin/caddy

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/caddy /usr/local/bin/caddy

EXPOSE 80 443

CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile"]
