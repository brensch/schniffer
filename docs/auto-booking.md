# Auto-booking design

## Goal

When a watched campground hits available, automatically hold a campsite on
behalf of users who've linked their recreation.gov credentials, so they only
have to log in and check out. Race-day-tight (sub-second from notification to
cart POST).

Scope: ~10 users. Recreation.gov only for now.

---

## Architecture

```
┌──────────────┐  schniff hit  ┌──────────────┐ creds?  ┌──────────────┐
│ manager/     │ ────────────► │  bookengine  │ ──────► │ browser pool │
│ notifications│               │ (per batch)  │         │ per-user JWT │
└──────────────┘               └──────────────┘         │  + Chrome    │
       │                              │                 └──────────────┘
       │ 1. notify hit                │ 2. pick site
       │ 4. notify result             │ 3. POST /multi
       ▼                              ▼
   Discord                       bookings table
```

Three new packages:

- `internal/secrets/` — AES-GCM wrapper around the master key
- `internal/booker/` — Chrome session per user + the rec.gov booking flow
  (the existing `cmd/booker` move/refactor lives here)
- Integration glue inside `internal/manager/notifications.go` (no new package)

---

## Database

### `user_credentials`

```sql
CREATE TABLE IF NOT EXISTS user_credentials (
    user_id        TEXT NOT NULL,
    provider       TEXT NOT NULL,          -- 'recreation_gov'
    username       TEXT NOT NULL,
    password_ct    BLOB NOT NULL,          -- AES-GCM ciphertext
    password_nonce BLOB NOT NULL,          -- 12-byte nonce
    disabled_at    DATETIME,               -- set when creds fail; NULL = active
    disabled_reason TEXT,
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, provider)
);
```

### `bookings`

```sql
CREATE TABLE IF NOT EXISTS bookings (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id      TEXT NOT NULL,            -- ties to notifications.batch_id
    user_id       TEXT NOT NULL,
    request_id    INTEGER NOT NULL,         -- schniff_requests.id we acted on
    provider      TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id   TEXT NOT NULL,            -- the site we picked
    checkin       DATE NOT NULL,
    checkout      DATE NOT NULL,
    status        TEXT NOT NULL,            -- 'attempted'|'held'|'failed'|'taken'
    external_id   TEXT,                     -- rec.gov reservation_id when held
    error         TEXT,                     -- API/error message on failure
    attempted_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at  DATETIME,
    FOREIGN KEY (request_id) REFERENCES schniff_requests(id)
);
CREATE INDEX IF NOT EXISTS idx_bookings_batch ON bookings(batch_id);
CREATE INDEX IF NOT EXISTS idx_bookings_user ON bookings(user_id, attempted_at DESC);
```

Status meanings:
- `attempted` — POST in flight, transient
- `held` — `ok:true` from `/multi`, reservation_id captured
- `failed` — error from API (recaptcha, network, server)
- `taken` — site got reserved by someone else between schniff hit and our POST

---

## Secrets

`internal/secrets/secrets.go`:

```go
type Cipher struct{ key []byte }                  // 32-byte master key
func NewFromEnv(envVar string) (*Cipher, error)   // base64-decode SCHNIFFER_ENC_KEY
func (c *Cipher) Seal(plaintext string) (ct, nonce []byte, err error)
func (c *Cipher) Open(ct, nonce []byte) (string, error)
```

- Algorithm: AES-256-GCM (stdlib `crypto/cipher`)
- Nonce: 12-byte random per encrypt, stored alongside ciphertext
- Key derivation: none — the env var IS the key, base64-decoded
- `make gen-enc-key`: emit a fresh `openssl rand -base64 32` for onboarding

If `SCHNIFFER_ENC_KEY` is unset at startup, the bot logs a warning, **disables
the `/schniff link` command and all booking**, but otherwise runs normally
(notifications still go out). This way the existing flow doesn't break for the
operator while they generate a key.

---

## Browser pool

Single struct per user:

```go
type session struct {
    userID     string
    profileDir string
    allocCtx   context.Context  // chromedp ExecAllocator
    browserCtx context.Context  // chromedp browser context
    cancel     context.CancelFunc
    accessToken string            // rec.gov JWT pulled from localStorage
    accountID   string
    expiresAt   time.Time         // JWT exp; we re-login before this
    mu         sync.Mutex
}
```

Pool:

```go
type Pool struct {
    sessions map[string]*session     // user_id -> session
    mu       sync.Mutex
    chrome   string                   // path to google-chrome-stable
}
func (p *Pool) Get(ctx, userID, username, password string) (*session, error)
func (p *Pool) Close()
```

### Lifecycle

- **Boot**: on bot startup, iterate `user_credentials WHERE disabled_at IS NULL`
  and spin up a `*session` for each. Login in parallel. Park each browser on
  `recreation.gov/` so r1s cookies + reCAPTCHA Enterprise JS are warm. **Never
  evict** — these are persistent for the bot lifetime. (10 sessions ≈ 1.5 GB
  worst case; fine.)
- **`/schniff link`** → after saving creds, spawn session immediately.
- **`/schniff unlink`** → close session, delete DB row.
- **Chrome crash** → detect via context `Done()`, restart the session.
  Exponential backoff capped at 5 min between attempts. Mark user as
  `disabled` after 5 consecutive failures.
- **JWT expiry**: rec.gov tokens last ~30 min. Background goroutine per
  session re-authenticates 5 min before expiry. Simpler than detecting 401
  mid-booking.
- **Process exit**: defer `pool.Close()` which cancels all browser contexts.

### Session warmth strategy

> "make sure it's ready to go as soon as possible"

Each session keeps the browser parked on `https://www.recreation.gov/` with a
fresh access token in localStorage. On booking trigger:

1. Navigate to `/camping/campsites/<id>` — this is what loads reCAPTCHA JS
   for the right reservation context. ~400ms.
2. `grecaptcha.enterprise.execute(...)` — ~300ms when warm.
3. POST `/api/camps/reservations/campgrounds/<cg>/multi` — ~150ms.

Total cold (already-logged-in but not on the campsite page): ~850ms. Acceptable.

We can pre-warm one step further: when a schniff request is created for a
specific campground, optimistically navigate that user's tab to the campsite
page in the background. But: a user with 30 active schniffs would thrash. Skip
this until profiling shows it matters.

---

## Booking selection

When a batch of state changes fires for one user:

1. Group changes by `(provider, campground_id)`. Schniff requests are also
   grouped this way.
2. For each `(provider, campground_id)` the user has an active schniff for and
   that has at least one newly-available night in their window:
   - Across all sites in that campground with availability inside the user's
     `[checkin, checkout)`, pick the one with the longest contiguous
     available stretch that intersects the window.
   - Trim the booking dates to that available stretch ∩ user window.
   - One booking attempt per (provider, campground_id) per batch.

Pseudocode:

```go
type candidate struct {
    campsiteID string
    bestRange  DateRange         // available ∩ user window, longest contiguous
}

func selectForBatch(batch StateChanges, schniffs []Schniff) []Booking {
    byCG := groupByCampground(batch)
    out := []Booking{}
    for cgKey, changes := range byCG {
        matching := schniffsForCampground(schniffs, cgKey)
        if len(matching) == 0 { continue }
        // Pick the *user's* widest window across schniffs at this CG so we
        // maximise our chance of a long stretch.
        window := unionWindows(matching)
        sites := availableSitesIn(changes, window)
        best := pickLongestContiguous(sites, window)
        if best == nil { continue }
        // Trim to the actual stretch that's contiguous and bookable.
        out = append(out, Booking{
            CampgroundID: cgKey.campgroundID,
            CampsiteID:   best.campsiteID,
            Checkin:      best.bestRange.Start,
            Checkout:     best.bestRange.End,
            RequestID:    pickRequestForRange(matching, best.bestRange),
        })
    }
    return out
}
```

Edge cases:
- User window is 4 nights, longest available stretch at any site is 2 nights →
  book the 2 nights. Better partial than nothing.
- Multiple sites tie on length → pick the one with the lower numeric site
  number (stable). Could later score by rating.
- A site becomes available but is outside any user window → skip.

---

## Discord UX

### `/schniff link`

Slash command opens a Discord modal with:
- `username` (rec.gov email) — TextInput, single line
- `password` — TextInput, single line, `Style: Short`

(Note: Discord modals don't have password-masked input, but the value is
submitted via interaction payload, not echoed in the channel.)

On submit:
1. Sanity-check username looks like an email.
2. Encrypt password, upsert `user_credentials`.
3. Spin up browser session, attempt login.
   - Success → reply ephemerally with success + warnings.
   - Failure → reply ephemerally with the error, don't store creds.

Reply text:

> ✅ Linked **\<username\>** to recreation.gov.
>
> **Important — please read both:**
>
> 1. **Use a unique password for this account.** Schniffer encrypts your
>    password before storing it, but if our database AND encryption key are
>    ever compromised, an attacker could log in as you. So don't reuse a
>    password you use anywhere else.
>
> 2. **Don't save a credit card to your recreation.gov account.** Schniffer
>    will hold a campsite in your cart for you, but you finish checkout in
>    your browser. If your account is compromised and a card is on file, an
>    attacker could complete a booking. Remove any saved cards at
>    https://www.recreation.gov/account/wallet.
>
> Run `/schniff unlink` any time to remove your credentials.

### `/schniff unlink`

Closes browser session, deletes `user_credentials` row, replies ephemerally
"unlinked".

### `/schniff status` (existing or new)

Show whether creds are linked + whether session is healthy.

---

## Notification flow

Existing batched-notification flow stays as the canonical event. We hook in
**after** the batch is computed and **before** the Discord send:

```go
// in internal/manager/notifications.go
batch := computeBatch(...)
for userID, userBatch := range batch.PerUser {
    sendHitMessage(userID, userBatch)               // existing
    if !bookerEnabled || !hasCreds(userID) { continue }
    plans := bookingPlanner.Plan(userBatch, userSchniffs[userID])
    for _, p := range plans {
        sendAttemptingMessage(userID, p)            // new
        result := booker.Attempt(ctx, userID, p)    // new
        recordBooking(p, result)                    // new
        sendResultMessage(userID, p, result)        // new
    }
}
```

Ordering within a batch:
1. **Hit message** (existing, one per batch): "📣 Availability at Baker Dam,
   Yosemite Pines for 9/22–9/24…"
2. For each campground in the batch:
   1. **Attempt message**: "🤖 Trying to hold site 7 at Baker Dam for
      9/22–9/24…"
   2. **Result message** (after ~1s): one of
      - "✅ Held — finish checkout at \<url\>" — link to
        `/camping/reservations/orderdetails?id=<reservation_id>`
      - "❌ Site got snapped up before we could hold it"
      - "❌ Hold failed: \<error\>" — generic API error
      - "🔐 Your saved password didn't work — please run `/schniff link`
        again" — auto-disables creds, sent only once per disable event

---

## Bad creds handling

On login failure inside the pool (server returns `lockout` or 401, or our
captured token field is empty):

1. `UPDATE user_credentials SET disabled_at = now(), disabled_reason = ?`
2. Close the browser session.
3. DM the user with the re-link prompt.
4. Future schniff hits for this user skip the booking step (still notify).

User running `/schniff link` again clears `disabled_at` and respawns the
session.

---

## File / package layout

```
internal/
  secrets/
    secrets.go               # AES-GCM wrapper + key loader
    secrets_test.go
  booker/
    booker.go                # Attempt(ctx, userID, plan) → BookResult
    pool.go                  # per-user Chrome lifecycle
    session.go               # login, navigate, captcha, POST
    selection.go             # Plan(batch, schniffs) → []BookingPlan
    selection_test.go
  bot/
    handler_link.go          # /schniff link modal + submit
    handler_unlink.go        # /schniff unlink
  db/
    schema.sql               # + user_credentials, bookings tables
    store.go                 # + StoreCreds, GetCreds, DisableCreds,
                              #   RecordBookingAttempt, etc.
  manager/
    notifications.go         # add booker hook after computing batch

docs/
  auto-booking.md            # this file
docker/
  Dockerfile.booker          # already exists; add to main image so prod
                              # process has Chrome available
cmd/
  booker/                    # standalone CLI booker (current) — keep as
                              # debug tool, but make it use internal/booker
  schniffer/                 # main bot — wire pool + handlers
```

---

## Implementation phases

A way to ship this incrementally, each phase reviewable on its own:

1. **Secrets package + key plumbing**
   - `internal/secrets`, `make gen-enc-key`, env wiring, tests.
   - Won't be visibly used yet, but lets future PRs depend on a real key.

2. **DB schema + credentials store**
   - Schema migration for `user_credentials` and `bookings`.
   - Store methods: `UpsertCredentials`, `GetCredentials`,
     `DisableCredentials`, `RecordBookingAttempt`, `UpdateBookingStatus`.

3. **Discord `/schniff link` and `/schniff unlink`**
   - Modal-based credential capture.
   - Calls store + secrets, with the warning reply.
   - No browser interaction yet — just storage.

4. **Refactor `cmd/booker` into `internal/booker`**
   - Move the chromedp logic into a library.
   - `cmd/booker` becomes a thin wrapper for debugging.

5. **Browser pool with always-warm sessions**
   - Pool boot on bot startup (parallel logins).
   - Crash recovery, JWT refresh.
   - Standalone integration test that boots, logs in, holds, and asserts
     `held` against a known-available test campsite.

6. **Booking selection algorithm**
   - Pure function, table-driven tests.

7. **Notification flow integration**
   - Hook into `notifications.go`.
   - Attempt + result Discord messages.
   - Booking records persisted.

8. **Bad-creds handling**
   - Detect, disable, DM, skip future.

9. **Production Dockerfile**
   - Roll the Chrome + Xvfb layers from `docker/Dockerfile.booker` into the
     main bot image so the bot process can launch Chrome subprocesses.

---

## Open questions (defer until needed)

- **Multi-night strategy**: do we let the user say "book the best partial
  match" vs "book only if you can cover my whole window"? Phase 1 just books
  the best partial.
- **Preference signals**: rating, equipment compatibility. Phase 1 picks by
  contiguous nights only.
- **Cancellation**: should the bot release a hold if the user doesn't
  complete checkout? rec.gov auto-expires holds in ~15 min, so we get this
  for free.
- **Other providers** (reservecalifornia): same pattern, different captcha
  and endpoint surface. Out of scope until rec.gov is solid.

---

## Operational requirements (production)

The booking pool runs Chrome as a subprocess of the schniffer Go binary
inside the same container. That has hard environmental requirements that
**must** be set by whoever runs the container; the code can't compensate
for them.

### 1. Shared memory (`/dev/shm`)

Chrome's renderer, GPU, and IPC layers use POSIX shared memory for almost
every page draw. Docker's default `/dev/shm` is **64 MB**, which Chrome
will exhaust within a few navigations and die with `SIGBUS`, `ENOSPC`, or
silent process exit. The `Pool` will then return `nav: context canceled`
for every booking until the container is restarted (or, with the
self-healing watchdog, until the next relaunch — but that wastes the
warmed cookie state and may itself fail to allocate).

`docker-compose.yml` sets:

```yaml
shm_size: 2gb
```

If you ever run the container outside of compose, pass `--shm-size=2g`
explicitly. We also pass Chrome `--disable-dev-shm-usage` defensively (it
makes Chrome use `/tmp` for some allocations), but that only mitigates the
problem; it doesn't replace a properly sized shm mount.

### 2. Memory limit

`mem_limit: 6g` + `memswap_limit: 6g`. Generous on a 16 GB host. Caps
runaway pool growth without throttling normal operation. Chrome processes
plus the Go binary plus Xvfb peak around 1.5 GB for a single warm session;
ten users would land around 4 GB.

### 3. Persistent profile dir (cookies survive deploys)

`BOOKER_PROFILE_DIR=/app/.cache/recgov-profiles` bind-mounted to a host
directory. Without the bind, every `docker compose up --build` (which our
deploy workflow runs on every push to `main`) wipes the
`r1s-fingerprint` cookie and JWT. A cold profile + immediate booking POST
is the worst possible signal to rec.gov's risk model and reliably returns
"abnormal activity detected".

`docker-compose.yml` sets:

```yaml
volumes:
  - ${SCHNIFFER_PROFILES:-./profiles}:/app/.cache/recgov-profiles
```

The deploy workflow (`.github/workflows/deploy-schniffer.yml`) creates
`/home/brensch/schniffprofiles` on the runner and passes it as
`SCHNIFFER_PROFILES`.

### 4. Xvfb wrapping

The production `Dockerfile` `CMD` is:

```
xvfb-run -a -s "-screen 0 1280x900x24" ./schniffer
```

Anything that overrides `CMD` must preserve the `xvfb-run` wrapper.
Without it, Chrome starts headless and gets bot-scored to zero by
reCAPTCHA Enterprise — every booking will fail human-verification with no
useful error.

### 5. Self-healing pool

The `Pool` runs two background loops:

- `RunRefreshLoop` — every 25 min, navigates each session to the homepage
  so cookies + JWT don't go stale.
- `RunWatchdog` — every 60 s, checks `sess.Ctx().Err()` plus a 3 s
  `chromedp.Evaluate("true")` ping. If a session is dead or hung, it
  closes it and relaunches Chrome + re-logs in. This means a single
  Chrome crash does *not* permanently break auto-booking for that user;
  the next watchdog tick or the next booking attempt will detect and
  recover.

`Pool.HoldCampsite` also re-checks aliveness inline before each booking
and relaunches if needed, so a hit between watchdog ticks doesn't pay
"context canceled" on a dead session.

### 6. Visibility

Chrome's stderr is filtered + routed through `slog` so a crash shows up in
`docker logs schniffer-bot`. DBus + GPU noise is dropped, FATAL/CHECK
lines and renderer aborts surface at `ERROR`, generic ERROR lines at
`WARN`. Look for `component=chrome level=ERROR`.

Booking attempts log `auto-booking attempt` / `auto-booking held` /
`auto-booking failed` with structured fields (user, campsite,
checkin/checkout, batch_id) — useful for grepping in conjunction with the
`bookings` table.

### 7. Disk

`docker compose up --build` rebuilds the image on every deploy. The build
cache + dangling images grow quickly. Periodically:

```
docker system prune -af --volumes
docker builder prune -af
```

(safe — the persistent volumes are named, not anonymous, so this doesn't
touch the sqlite db or the profile dir).

### 8. Polling cost analysis (GCP Cloud Run free tier)

Outbound API calls go through `internal/proxypool`, which batches multiple
upstream requests into one POST per Cloud Run invocation (up to 50 in
flight, 10ms flush window). At the time of writing the proxy runs in 5
GCP regions (us-central1, us-west1, us-west2, us-west4, us-south1).

Measured baseline (live prod, 10s poll cadence, ~22 active campgrounds,
24h window via Cloud Monitoring API):

| Resource | Used (24h) | Projected (30d) | Free tier (30d) | % used |
|---|---|---|---|---|
| `request_count` | 7,260 | ~218,000 | 2,000,000 | 11 % |
| `cpu/allocation_time` (vCPU-s) | 2,791 | ~84,000 | 180,000 | 47 % |
| `memory/allocation_time` (GB-s) | 634 | ~19,000 | 360,000 | 5 % |
| Egress (estimated) | ~370 MB | ~11 GB | 1 GB | ~$1.20/mo |

At 5s cadence (2× upstream throughput):

| Resource | Projected (30d) | Free tier (30d) | % used |
|---|---|---|---|
| `request_count` | ~436,000 | 2,000,000 | 22 % |
| `cpu/allocation_time` | ~168,000 | 180,000 | **93 %** ⚠️ |
| `memory/allocation_time` | ~38,000 | 360,000 | 11 % |
| Egress | ~22 GB | 1 GB | ~$2.50/mo |

So **5s polling fits inside the free tier on requests/CPU/memory** and
costs roughly $2–5/month total in egress + spillover. Going below 5s
starts to push vCPU-seconds over the free tier and risks tripping
`recreation.gov`'s per-IP throttle (we already see startup 429s when 22
campgrounds fire in one cycle — adding more pressure won't help). The
`runProviderLoop` exponential-backoff on 429 caps the effective rate, but
that means more "rate limited" Discord notifications and longer tail
latency on hits.

How to re-check the numbers yourself:

```bash
gcloud config set project schniffer
TOKEN=$(gcloud auth print-access-token)
START=$(date -u -d "1 day ago" +%Y-%m-%dT%H:%M:%SZ)
END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for METRIC in run.googleapis.com/request_count \
              run.googleapis.com/container/cpu/allocation_time \
              run.googleapis.com/container/memory/allocation_time; do
  echo "=== $METRIC ==="
  curl -sG "https://monitoring.googleapis.com/v3/projects/schniffer/timeSeries" \
    -H "Authorization: Bearer $TOKEN" \
    --data-urlencode "filter=metric.type=\"$METRIC\"" \
    --data-urlencode "interval.startTime=$START" --data-urlencode "interval.endTime=$END" \
    --data-urlencode 'aggregation.alignmentPeriod=86400s' \
    --data-urlencode 'aggregation.perSeriesAligner=ALIGN_SUM' \
    --data-urlencode 'aggregation.crossSeriesReducer=REDUCE_SUM' \
    | python3 -c "
import json,sys
d=json.load(sys.stdin)
total=sum(float(p['value'].get('doubleValue', p['value'].get('int64Value','0')))
          for ts in d.get('timeSeries',[]) for p in ts.get('points',[]))
print(f'  24h: {total:,.0f}')"
done
```

The local `lookup_log` table is the other source of truth for upstream
throughput:

```bash
sqlite3 /home/brensch/schniffdata/schniffer.sqlite "
  select strftime('%Y-%m-%d %H', checked_at) h, count(*) n
  from lookup_log
  where checked_at > datetime('now','-6 hour')
  group by h order by h;"
```

The ratio `local_lookups / cloud_run_invocations` tells you how well the
batching is working; we see ~10× in production, which is the right ball-
park for 22 simultaneous campgrounds funnelled through a 10ms-flush
batcher.

### 9. Quick health check

```bash
# Container running, Chrome process tree alive
docker exec schniffer-bot pgrep -af chrome | head

# /dev/shm sized correctly
docker exec schniffer-bot df -h /dev/shm   # expect 2.0G

# Profile dir populated and bind-mounted
docker inspect schniffer-bot --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'

# Recent booking outcomes
sqlite3 /home/brensch/schniffdata/schniffer.sqlite \
  "select id, user_id, campsite_id, outcome, substr(error_msg,1,80), attempted_at \
   from bookings order by id desc limit 10;"
```
