# schniffer

A discord bot for schniffing people cancelling their reservations at in demand campgrounds. Select a date and place you want to go and the schniffer will do the rest. 

Schniffer now features the ability to auto add to cart so you have 10 minutes to hold. It only works for reserve california sites. If you choose not to use that feature you will likely be beaten by someone paying Campsite Tonight to schniff for them. They scan every 20 seconds, we scan every 5, we beat them most of the time.

## Features

- **Providers**: recreation.gov and ReserveCalifornia, behind a common interface so more can be added.
- **Polling**: every 5 seconds per provider, backing off by 10s per failed cycle. Overlapping schniffs for the same campground share lookups, so many users watching the same campground cost the same as one.
- **Proxy fleet**: outbound polling is batched through a small Cloud Run service deployed to all 42 Cloud Run regions (`proxy/`, `internal/proxypool`), spreading requests across many IPs to avoid per-IP rate limiting. Sized to fit the free tier.
- **Change detection**: availability transitions are recorded in SQLite. When a site flips to available, competing requests are arbitrated (oldest schniff wins) and the winner is DMed an embed with booking links. Rising-edge tracking prevents repeat notifications for the same site.
- **Auto-booking** (optional): per-user Chrome sessions (chromedp under Xvfb; headless Chrome fails bot detection) stay warm with the user's recreation.gov login, pre-parked on a tab with reCAPTCHA loaded, so the hold request fires in about a second when a schniff hits. Credentials are AES-256-GCM encrypted at rest. If no key is set in the env, auto-booking is disabled and the bot runs normally. Design notes in [docs/auto-booking.md](docs/auto-booking.md).
- **Web map**: a map UI (default `:8069`) for browsing campgrounds, checking availability, and building groups for bulk schniffing. Opened via `/schniff map`.
- **Filters**: `minimum_nights` (require N consecutive free nights) and strategies (currently `full_weekend`: Friday and Saturday nights both free).
- **Observability**: Prometheus metrics on `:9090/metrics`, Grafana dashboard in `docker/grafana`, and a daily summary posted at 9pm Pacific (skipped on idle days).

## Commands

Everything lives under `/schniff`, sent as a DM to the bot:

| Command | What it does |
| --- | --- |
| `/schniff add` | Watch a campground (autocomplete) for a checkin to checkout window, with optional `minimum_nights` and `strategy` |
| `/schniff add-bulk` | Same, but for every campground in a saved group |
| `/schniff map` | Open the web map to browse availability and build groups |
| `/schniff group list` / `group remove` | Manage saved campground groups |
| `/schniff list` | List your active schniffs |
| `/schniff remove` / `remove-all` | Remove one schniff, or all of them |
| `/schniff summary` | Activity summary across all users |
| `/schniff link` / `unlink` | Store (encrypted) or delete your recreation.gov login for auto-booking |

Dates are `YYYY-MM-DD`.

## Quick start

```sh
# build & run locally (guild-scoped commands, no auto-booking)
DISCORD_TOKEN=... GUILD_ID=... make run

# or via docker
cp .env.example .env   # fill it in
docker-compose up -d   # bot + prometheus + grafana; see DOCKER.md
```

### Environment variables

| Var | Default | Purpose |
| --- | --- | --- |
| `DISCORD_TOKEN` | required | Bot token |
| `GUILD_ID` | required | Guild for command registration and the broadcast channel (first text channel) |
| `PROD` | unset | `true` registers slash commands globally instead of guild-scoped |
| `DB_PATH` | `./schniffer.sqlite` | SQLite database file |
| `WEB_ADDR` | `:8069` | Web map / API |
| `METRICS_ADDR` | `:9090` | Prometheus `/metrics` and `/healthz` |
| `SCHNIFFER_ENC_KEY` | unset | Base64 32-byte AES key; unset disables auto-booking (`make gen-enc-key` to generate one) |
| `BOOKER_PROFILE_DIR` | `.cache/recgov-profiles` | Persistent Chrome profiles so logins survive restarts |
| `PROXY_SECRET` | required for proxying | Shared secret between the bot and the proxy fleet |

## Storage

SQLite via `mattn/go-sqlite3`. Schema in `internal/db/schema.sql`: schniff requests, campsite availability, lookup log, state changes, notifications, groups, encrypted user credentials, and booking outcomes. Analytics come from Prometheus and Grafana, not the database.

## Repo tour

| Path | Contents |
| --- | --- |
| `cmd/schniffer` | The bot itself |
| `internal/manager` | Poll loops, dedup, change detection, notification arbitration, daily summary |
| `internal/bot` | Discord commands, modals, welcome DMs |
| `internal/providers` | recreation.gov and ReserveCalifornia clients |
| `internal/booker` + `internal/secrets` | Warm Chrome pool and encrypted credential storage |
| `internal/proxypool` + `proxy/` | Request batching client and the 42-region Cloud Run proxy (deploy: `proxy/deploy.sh`) |
| `internal/web` + `static/` | Map UI and JSON APIs |
| `internal/nonsense` | Randomised bot flavour text |
| `cmd/*` (the rest) | Probes and benches for the booking pipeline: `booker`, `booker-bench`, `booker-stability`, `booker-warmtab-test`, `token-probe`, `recaptcha-probe`, `refresh-probe`, plus `gen-enc-key` and `clear-commands` |
