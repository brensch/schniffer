# Cloudflare Worker proxy

Edge proxy that handles outbound fetches to recreation.gov / reservecalifornia. Same wire contract as `proxy/` (Cloud Run) but billed for CPU only, so the long `await fetch(...)` wait on the upstream is free.

## Why

- Cloud Run bills wall-clock time per request. A typical batch holds the container open ~700ms waiting on rec.gov; we pay for all of it. At 2s polling that's ~$30/mo over the free tier.
- Cloudflare Workers bill CPU time. We use a few hundred microseconds per request (JSON munging + fetch dispatch). At the same load, ~$5/mo on the paid plan, or free if usage stays under 100k/day.
- Bonus: CF auto-compresses responses at the edge (gzip/br/zstd) without code in the worker. Measured: 560 KB raw → 7 KB gzip → 4 KB brotli.

## Deploy

### One-time setup

1. Create a Cloudflare API token with `Workers Scripts:Edit` + `Workers Subdomain:Edit` permissions.
2. Add GitHub Actions secrets:
   - `CLOUDFLARE_API_TOKEN`
   - `CLOUDFLARE_ACCOUNT_ID`
   - `PROXY_SECRET` (the same value used by the Cloud Run deploy / stored in GCP Secret Manager)
3. First push to `main` with `proxy-cf/**` changes triggers `.github/workflows/deploy-proxy-cf.yml`.

### Manual deploy (from a dev machine)

```
cd proxy-cf
npx wrangler login                                # interactive auth
npx wrangler secret put PROXY_SECRET              # paste secret when prompted
npx wrangler deploy
```

The worker URL appears in the deploy output as `https://schniffer-proxy.<your-subdomain>.workers.dev`. Paste it into `internal/proxypool/endpoints.json`.

## Local test

```
cd proxy-cf
npx wrangler dev   # http://localhost:8787/fetch
```

Worker runs locally with a mock secret (`wrangler dev --var PROXY_SECRET:test`).

## Contract

Identical to `proxy/main.go`:

```
POST /fetch
Authorization: Bearer <PROXY_SECRET>
Content-Type: application/json

{"requests": [{"url": "...", "method": "GET", "headers": {...}, "body": "..."}]}
```

Response:

```
{"responses": [{"status": 200, "headers": {...}, "body": "...", "elapsed_ms": 42}], "region": "cf-<colo>"}
```

`/healthz` returns 200 OK for warmup pings.

## Limits

| Free | Paid ($5/mo) |
|---|---|
| 100k req/day | 10M req/mo included |
| 10ms CPU/req | 30s CPU/req |
| 50 subrequests/req | 1000 subrequests/req |

Schniffer's typical batch is ~8 fetches, well under both subrequest caps. At 2s polling we sit around 1.3M req/mo — paid plan is the right place.
