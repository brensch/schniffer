# schniffer

A Go Discord bot that monitors campground availability, records all activity to DuckDB, and notifies users when sites become available or unavailable.

## Features

- Discord-driven: add/list/remove monitoring requests ("schniffs").
- Pluggable campground providers via a common interface.
- Recreation.gov provider built-in.
- Deduplicated lookups per campground per month every 5 seconds.
- Change detection on campsite availability; notify on available and unavailable transitions.
- DuckDB-backed storage for requests, state, lookups, notifications, and daily stats.
- Daily summary posted to a channel and stored for Grafana.

## Quick start

Environment variables:

- DISCORD_TOKEN: Bot token.
- DUCKDB_PATH: Path to DuckDB file (e.g., ./schniffer.duckdb).
- SUMMARY_CHANNEL_ID: Discord channel ID for daily summary messages (optional).
- GUILD_ID: Optional; if provided, slash commands will be registered guild-scoped for faster availability.

Run:

```
# build
go build ./cmd/schniffer

# run
DISCORD_TOKEN=... DUCKDB_PATH=./schniffer.duckdb go run ./cmd/schniffer
```

## Commands

- /schniff add provider:<recreation_gov> campground_id:<id> start_date:<YYYY-MM-DD> end_date:<YYYY-MM-DD>
- /schniff list
- /schniff remove id:<request_id>
- /schniff stats

Dates are inclusive.

## Notes

- Recreation.gov API is public and queried per-month. We dedupe lookups per campground/month.
- All events are recorded to DuckDB for downstream analytics (Grafana, etc.).
