# Stage 1: Builder
# Same builder image as caddy-sidekick and the fleet's frankenwp-base build, so Go comes from one
# place (/usr/local/go, already first on PATH) instead of a separate pin it could silently override.
FROM dunglas/frankenphp:builder-php8.3-bookworm AS builder

ARG XCADDY_VERSION
ARG CADDY_VERSION

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates     \
    curl                \
    git                 \
    && apt-get clean    \
    && rm -rf /var/lib/apt/lists/*

# Install xcaddy
WORKDIR /tmp
RUN curl --silent -Lo - https://github.com/caddyserver/xcaddy/releases/download/v${XCADDY_VERSION}/xcaddy_${XCADDY_VERSION}_linux_amd64.tar.gz | tar -zxf - && install -D /tmp/xcaddy /usr/local/bin/xcaddy

WORKDIR /src
COPY . .

# caddy-consul manages TCP/L4 listeners in-process (no caddy-l4 dependency).
RUN xcaddy build v${CADDY_VERSION} \
    --with github.com/honest-hosting/caddy-consul=/src \
    --output /usr/local/bin/caddy

RUN /usr/local/bin/caddy version

# Stage 2: Runtime
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/caddy /usr/local/bin/caddy

EXPOSE 80 443

CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile"]
