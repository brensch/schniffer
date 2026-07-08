# schniffer 🐽

A Go Discord bot that schniffs out campsite cancellations so you don't have to.

People book campgrounds months out. People's plans change (usually on a Sunday night, for reasons science cannot explain). They cancel. The schniffer notices within seconds, DMs you, and — if you've linked your recreation.gov account — has already put the site in your cart before you've finished reading the message.

## What it actually does

- **Providers**: recreation.gov and ReserveCalifornia, behind a common interface so more can be schnaffed in later.
- **Polling**: every 5 seconds per provider, backing off by 10s per failed cycle. Overlapping schniffs for the same campground share lookups, so 40 people watching Yosemite costs the same as one very keen person.
- **Proxy fleet**: outbound polling is batched through a tiny Cloud Run service deployed to **all 42 Cloud Run regions** (`proxy/`, `internal/proxypool`), spreading requests across many IPs so no single one gets the 429 hammer. Fits in the free tier. We are nothing if not thrifty.
- **Change detection**: availability transitions land in SQLite; when a site flips to available, the winner is arbitrated (oldest schniff wins) and DMed an embed with links. Rising-edge tracking means you don't get re-pinged about the same site every 5 seconds forever.
- **Auto-booking** (optional): per-user real Chrome sessions (chromedp under Xvfb — headless Chrome gets bot-scored into oblivion) stay warm with your recreation.gov login, pre-parked on a tab with reCAPTCHA already loaded. When your schniff hits, the hold POST fires in ~1 second. Credentials are AES-256-GCM encrypted at rest; no key in the env, no auto-booking, bot runs fine regardless. Design notes in [docs/auto-booking.md](docs/auto-booking.md).
- **Web map**: a map UI (default `:8069`) for browsing campgrounds, checking availability, and building groups for bulk schniffing. Opened via `/schniff map`.
- **Filters**: `minimum_nights` (need N consecutive free nights) and strategies (currently `full_weekend`: Friday + Saturday both free, or it didn't happen).
- **Observability**: Prometheus metrics on `:9090/metrics`, Grafana dashboard in `docker/grafana`, daily "🏕️ 24h Schniffer Roundup" posted at 9pm Pacific (politely skipped when nothing happened).
- **Personality**: new members are announced with randomly selected competitive-eating-announcer introductions. This is load-bearing and not up for discussion.

## Commands

Everything lives under `/schniff` (DM the bot, don't clutter the channel):

| Command | What it does |
| --- | --- |
| `/schniff add` | Watch a campground (autocomplete) for a checkin→checkout window, with optional `minimum_nights` and `strategy` |
| `/schniff add-bulk` | Same, but for every campground in a saved group |
| `/schniff map` | Open the web map to browse availability and build groups |
| `/schniff group list` / `group remove` | Manage saved campground groups |
| `/schniff list` | List your active schniffs |
| `/schniff remove` / `remove-all` | Retire one schniff, or all of them |
| `/schniff summary` | Activity summary across all schniffists |
| `/schniff link` / `unlink` | Store (encrypted) or delete your recreation.gov login for auto-booking |

Dates are `YYYY-MM-DD`.

## Quick start

```sh
# build & run locally (guild-scoped commands, no auto-booking)
DISCORD_TOKEN=... GUILD_ID=... make run

# or the full production experience
cp .env.example .env   # fill it in
docker-compose up -d   # bot + prometheus + grafana; see DOCKER.md
```

### Environment variables

| Var | Default | Purpose |
| --- | --- | --- |
| `DISCORD_TOKEN` | — | Bot token. Required, obviously |
| `GUILD_ID` | — | Guild for command registration and the broadcast channel (first text channel) |
| `PROD` | unset | `true` registers slash commands globally instead of guild-scoped |
| `DB_PATH` | `./schniffer.sqlite` | SQLite database file |
| `WEB_ADDR` | `:8069` | Web map / API |
| `METRICS_ADDR` | `:9090` | Prometheus `/metrics` + `/healthz` |
| `SCHNIFFER_ENC_KEY` | unset | Base64 32-byte AES key; unset = auto-booking off (`make gen-enc-key` to mint one) |
| `BOOKER_PROFILE_DIR` | `.cache/recgov-profiles` | Persistent Chrome profiles so logins survive restarts |
| `PROXY_SECRET` | — | Shared secret between the bot and the proxy fleet |

## Storage

SQLite (yes, SQLite — earlier editions of this README claimed DuckDB and were lying). Schema in `internal/db/schema.sql`: schniff requests, campsite availability, lookup log, state changes, notifications, groups, encrypted user credentials, and booking outcomes. Analytics come from Prometheus + Grafana, not the database.

## Repo tour

| Path | Contents |
| --- | --- |
| `cmd/schniffer` | The bot itself |
| `internal/manager` | Poll loops, dedup, change detection, notification arbitration, daily summary |
| `internal/bot` | Discord commands, modals, welcome DMs |
| `internal/providers` | recreation.gov + ReserveCalifornia clients |
| `internal/booker` + `internal/secrets` | Warm Chrome pool + encrypted credential box |
| `internal/proxypool` + `proxy/` | Request batching client + the 42-region Cloud Run proxy (deploy: `proxy/deploy.sh`) |
| `internal/web` + `static/` | Map UI and JSON APIs |
| `internal/nonsense` | The most important package |
| `cmd/*` (the rest) | Probes and benches for the booking pipeline: `booker`, `booker-bench`, `booker-stability`, `booker-warmtab-test`, `token-probe`, `recaptcha-probe`, `refresh-probe`, plus `gen-enc-key` and `clear-commands` |

## Why can it find campsites that are all booked right now?

People make plans. Those plans change. They cancel. The schniffer does not make plans, does not do human stuff, and does not sleep. It schniffs.
