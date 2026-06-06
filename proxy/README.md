# proxy

Tiny Go HTTP forward-proxy. One container per Cloud Run region; schniffer
routes outbound polling traffic through them to rotate egress IPs.

## How it fits together

- `main.go` — POST `/fetch` accepts a JSON batch of {url, method, headers,
  body}, fans them out concurrently, returns the responses.
- `Dockerfile` — distroless Go binary.
- `regions.txt` — desired GCP region list. The `Deploy proxy` workflow
  (`.github/workflows/deploy-proxy.yml`) builds the image once, deploys it to
  every region in this file, and writes the resulting URLs into
  `internal/proxypool/endpoints.json` (so schniffer picks them up on its
  next deploy).
- `deploy.sh` — local-machine version of the workflow; useful if you want to
  push a fix without going through GitHub.

## Running the proxy locally

For a fast manual test of the batching shape (no GCP needed):

```bash
cd proxy
PROXY_SECRET=local-test go run .
```

In another shell:

```bash
curl -s -X POST http://localhost:8080/fetch \
  -H "Authorization: Bearer local-test" \
  -H "Content-Type: application/json" \
  -d '{"requests":[{"url":"https://httpbin.org/get"},{"url":"https://httpbin.org/ip"}]}' | jq
```

You should see two responses returned in a single call, each with `status:
200` and the upstream body.

## Running schniffer against the local proxy

To test the full client → batching pool → proxy → upstream flow on your
laptop without touching production:

```bash
# 1. Run the local proxy (above)

# 2. Override the endpoint list so schniffer talks to localhost
cat > /tmp/local-endpoints.json <<'EOF'
{"endpoints":[{"url":"http://localhost:8080","provider":"local","region":"dev"}]}
EOF
cp /tmp/local-endpoints.json internal/proxypool/endpoints.json   # see note

# 3. Run schniffer pointed at a temp sqlite, with the proxy enabled
PROXY_SECRET=local-test \
DB_PATH=/tmp/schniffer-dev.sqlite \
DISCORD_TOKEN=fake \
GUILD_ID=fake \
go run ./cmd/schniffer
```

Note: `endpoints.json` is embedded into the binary at compile time
(`//go:embed`), so you have to re-build (`go run` does this automatically)
after editing it. Don't commit a local-only endpoints.json.

## Manual smoke after deploying

```bash
PROXY_SECRET=$(gcloud secrets versions access latest --secret=proxy-secret --project=schniffer)
URL=$(gcloud run services describe proxyrequest --region=us-central1 --project=schniffer --format='value(status.url)')
curl -s -X POST "$URL/fetch" \
  -H "Authorization: Bearer $PROXY_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"requests":[{"url":"https://www.recreation.gov/api/camps/availability/campground/10083567/month?start_date=2026-08-01T00%3A00%3A00.000Z"}]}' | jq '.responses[0].status'
```

Should print `200`.
