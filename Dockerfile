# Build stage
FROM golang:1.26-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc g++ libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o schniffer ./cmd/schniffer

# Runtime stage: includes Chrome + Xvfb so the booker browser pool can drive
# real Chrome sessions (reCAPTCHA Enterprise fingerprints headless-shell as
# a bot, so we run headful under a virtual display).
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates wget gnupg fonts-liberation xdg-utils \
        xvfb xauth tini procps \
        libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libxkbcommon0 \
        libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 \
        libpango-1.0-0 libcairo2 libasound2 \
    && wget -qO- https://dl.google.com/linux/linux_signing_key.pub \
        | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" \
        > /etc/apt/sources.list.d/google-chrome.list \
    && apt-get update && apt-get install -y --no-install-recommends google-chrome-stable \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/schniffer .
COPY --from=builder /app/internal/db/schema.sql ./internal/db/
COPY --from=builder /app/static ./static/

# Default profiles dir; mount a volume here in prod to persist cookies +
# r1s anti-bot fingerprints across deploys.
ENV BOOKER_PROFILE_DIR=/app/.cache/recgov-profiles
ENV DISPLAY=:99
RUN mkdir -p /app/data $BOOKER_PROFILE_DIR

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["xvfb-run", "-a", "-s", "-screen 0 1280x900x24", "./schniffer"]
