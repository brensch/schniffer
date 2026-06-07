# Booker prototype — how the working flow runs today

Snapshot of the current end-to-end flow, taken after the 2026-06-07 round of
testing. Two successful holds were placed during that session:

- Campsite **10085607** (Baker Dam), 2026-10-15 → 2026-10-16. Order
  `13c48a05-cb69-4642-9254-f025fcdc35e6`. Took ~1.5s booking time.
- Campsite **89223** (Site 026, Rock Creek Lake), 2026-07-07 → 2026-07-08.
  Order `21f8a377-c371-4202-b5b1-b6b668969fb0`. Login 0.6s + booking 1.6s.

Both holds auto-expire ~15 minutes after creation.

This doc explains every moving piece so anyone (human or LLM) can rerun the
flow against a different campsite or debug a regression.

## Components

### 1. Container image — `docker/Dockerfile.booker`

`golang:1.26-bookworm` with `google-chrome-stable`, `xvfb`, `xauth`, `tini`,
and the Chrome runtime libs installed. We use **stable Chrome under Xvfb
rather than headless-shell** because reCAPTCHA Enterprise fingerprints
headless-shell as a bot and scores it near zero.

Key env vars baked into the image:

| Var               | Value     | Why                                                                     |
| ----------------- | --------- | ----------------------------------------------------------------------- |
| `CHROME_PROFILE`  | `/profile`| Bind-mount target for the persistent Chrome user-data-dir.              |
| `DISPLAY`         | `:99`     | Default virtual display number. `xvfb-run` picks a free `:NN` per run.  |

### 2. Persistent profile dir — `.cache/recgov-profile`

The same user-data-dir is reused across runs. It survives:

- The `recaccount` JWT in `localStorage` (skip re-login next run).
- Cookies including the `r1s-fingerprint` anti-bot token. **This is the
  load-bearing one** — letting the residential IP build up a high score with
  rec.gov's Akamai layer is the difference between Phase A succeeding in
  ~1.5s and failing with "abnormal activity".

If Chrome cannot reuse the profile (stale `SingletonLock` from a previous
container that had a different hostname), `booker.Open` deletes
`SingletonLock`, `SingletonCookie`, `SingletonSocket` before launching.

### 3. Booker library — `internal/booker`

- `Session` (booker.go): owns one Chrome instance via chromedp; exposes
  `Login`, `HoldCampsite`, `HoldCampsiteWithToken`, `Refresh`, `Close`.
- `Pool` (pool.go): per-user warm sessions for the production bot path.
- `selection.go`: pure selection algorithm (longest contiguous stretch,
  tie-break by rating).

### 4. CLI driver — `cmd/booker`

Thin wrapper that calls the library once and exits. Used for smoke tests.
Flags worth knowing:

| Flag             | Default                          | Notes                                                   |
| ---------------- | -------------------------------- | ------------------------------------------------------- |
| `-campsite`      | `10085607` (Baker Dam)           | Leaf campsite id.                                       |
| `-campground`    | `10085599`                       | Parent facility id.                                     |
| `-from`          | (required)                       | Arrival date `YYYY-MM-DD`.                              |
| `-nights`        | `1`                              | Length of stay.                                         |
| `-profile`       | `$CHROME_PROFILE`                | Chrome user-data-dir.                                   |
| `-login-only`    | `false`                          | Just warm cookies + JWT, don't book.                    |
| `-use-2captcha`  | `false`                          | Force the 2captcha fallback path (see "What 2captcha did" below). |
| `-debug-dir`     | `/work/.cache/debug`             | Last response JSON + (on failure) screenshot/html.      |
| `-timeout`       | `5m`                             | Overall context timeout.                                |

### 5. Wrapper scripts

- `scripts/booker-run.sh` — one-shot: builds the image if missing, then runs
  the booker once under `xvfb-run`. Mounts the repo, the Chrome profile,
  and the Go caches as the host UID. Passes `.env` straight through.
- `scripts/booker-shell.sh` — opens an interactive shell in the same
  container for fast iteration without a docker rebuild per run.

## Environment

`.env` at the repo root (not committed):

```
REC_GOV_EMAIL=you@example.com
REC_GOV_PASSWORD=...
2CAPTCHA_API_KEY=...               # only needed for -use-2captcha
```

`scripts/booker-run.sh` aliases `2CAPTCHA_API_KEY` → `TWOCAPTCHA_API_KEY`
inside the container because `xvfb-run` strips env vars with POSIX-invalid
names (any var starting with a digit). The Go code reads
`TWOCAPTCHA_API_KEY` first, falls back to `2CAPTCHA_API_KEY`.

## Step-by-step: what one successful booking actually does

Take the Rock Creek Lake run as the reference. Command:

```bash
./scripts/booker-run.sh -campsite 89223 -campground 233907 -from 2026-07-07 -nights 1
```

What happens, in order:

1. **`booker-run.sh`** (`scripts/booker-run.sh`):
   - `docker build -f docker/Dockerfile.booker -t schniffer-booker:dev .` if
     the image is missing.
   - `docker run --rm --user $UID:$GID --shm-size=2g …` mounting:
     - `$PWD:/work` (the repo)
     - `$PWD/.cache/go-build:/work/.cache/go-build`
     - `$PWD/.cache/go-mod:/go/pkg/mod`
     - `$PWD/.cache/recgov-profile:/profile`
   - Inside, executes:
     ```
     xvfb-run -a -s "-screen 0 1280x900x24" go run ./cmd/booker <flags>
     ```

2. **`cmd/booker/main.go`** parses flags, requires `REC_GOV_EMAIL` +
   `REC_GOV_PASSWORD`, calls `booker.Open(Config{ProfileDir, ChromePath})`.

3. **`booker.Open`** (`internal/booker/booker.go`):
   - `os.MkdirAll(ProfileDir, 0o700)`
   - Removes `SingletonLock`, `SingletonCookie`, `SingletonSocket` from the
     profile dir.
   - Builds chromedp exec allocator options. **Critically, we do not use
     `chromedp.DefaultExecAllocatorOptions`** because it adds `--headless`,
     which gets us bot-scored. Instead:
     ```
     --user-data-dir=<ProfileDir>
     --no-first-run
     --no-default-browser-check
     --no-sandbox
     --disable-dev-shm-usage
     --disable-blink-features=AutomationControlled
     --window-size=1280,900
     ```
   - Starts Chrome eagerly via `chromedp.Run(ctx)` so launch failures
     surface in `Open`, not on the first navigation.

4. **`Session.Login`** (`booker.go`):
   - `chromedp.Navigate("https://www.recreation.gov/")` — seeds the
     `r1s-fingerprint` cookie if it's not already there.
   - `chromedp.Evaluate("!!window.localStorage.getItem('recaccount')")` —
     if true, we have a JWT from a previous run and skip the form.
   - Otherwise navigate to `/log-in` and poll the DOM for an email-like and
     a password-like input. The SPA's field IDs vary across bundle versions
     so we discover them at runtime via `findLoginFields`, preferring stable
     IDs (`#rec-acct-sign-in-email-address` / `#rec-acct-sign-in-password`)
     and falling back to "first visible email-type + first password-type".
   - Set values via `Object.getOwnPropertyDescriptor(HTMLInputElement,
     'value').set` so React's controlled inputs notice; dispatch
     `input` + `change` events.
   - Click the sign-in button.
   - Poll `localStorage.recaccount` for up to 20s. Appearance ⇒ logged in.
     Timeout ⇒ `ErrBadCredentials`.

5. **`Session.HoldCampsite`**:
   - `chromedp.Navigate("/camping/campsites/89223")`.
   - `chromedp.Poll(typeof grecaptcha !== 'undefined' && …, 30s)` until
     reCAPTCHA Enterprise has finished loading in the page.
   - Build a `nightMap` keyed by ISO date for every night in `[checkin,
     checkout)`. Each value is `{campsite_id, campsite_loop:"",
     campsite_name:""}`.
   - Evaluate this JS inside the page context, awaiting the returned promise:

     ```js
     (async (p) => {
       const token = await grecaptcha.enterprise.execute(p.siteKey, {action: p.action});
       const rec = JSON.parse(window.localStorage.getItem('recaccount'));
       const body = {
         reservations: [{
           account_id: rec.account.account_id,
           campsite_id: p.campsiteID,
           check_in: p.checkIn,
           check_out: p.checkOut,
           reservation_options: {
             night_map: p.nightMap,
             recommendation_referrer: 'campground-vnull:campsitePage',
           },
         }],
         gate_a: {
           value: token,
           description: p.action,
           success: true,
           terminal: 'east',
         },
       };
       const response = await fetch(
         '/api/camps/reservations/campgrounds/' + p.campgroundID + '/multi',
         {method: 'POST',
          headers: {'content-type':'application/json',
                    'authorization': 'Bearer ' + rec.access_token},
          body: JSON.stringify(body),
          credentials: 'include'});
       const text = await response.text();
       return {status: response.status, ...(JSON.parse(text))};
     })({…})
     ```

   Doing the POST **from inside the page** is what makes the whole approach
   tractable: cookies attach automatically (no CORS headache), the
   `r1s-fingerprint` is sent, and the grecaptcha token has the same browser
   fingerprint Google issued it to.

6. **Response handling** (`bookingResponseError`):
   - Non-2xx → `fmt.Errorf("rec.gov returned status %v", status)`.
   - `ok:false` with message containing "abnormal activity" → wrapped as
     `ErrHumanVerification`. This is the load-bearing signal: when it
     fires, our session's risk score has dropped below the threshold.
   - Other `ok:false` → bubble the message up verbatim.
   - Success but no reservation id anywhere → "booking response contained
     no reservation id" (defensive — has never fired in testing).

7. **Order id extraction** (`ExtractOrderID`) walks
   `reservations[0].reservation.reservation_id` first, then a handful of
   fallback shapes. Used to print the order-details URL:
   `https://www.recreation.gov/camping/reservations/orderdetails?id=<id>`.

## Logs from a successful run

```
time=… level=INFO msg="logging in"
time=… level=INFO msg=booking campsite=89223 campground=233907 arrival=2026-07-07 depart=2026-07-08 use_2captcha=false

Reservation held: https://www.recreation.gov/camping/reservations/orderdetails?id=21f8a377-c371-4202-b5b1-b6b668969fb0
```

The lifecycle from `INFO booking` to "Reservation held" is the relevant
hot path. In the 2026-06-07 runs that took **~1.5–1.6s** end to end.

The raw JSON response is dumped to `/work/.cache/debug/last-response.json`
inside the container (i.e. `.cache/debug/last-response.json` on the host)
every run, success or failure.

## How to reproduce against an arbitrary site

1. **Pick a campground id.** Use the rec.gov search URL or our DB.
2. **Find a campsite + date with availability.** The `availability/campground/<id>/month` endpoint is enough:

   ```bash
   curl -s "https://www.recreation.gov/api/camps/availability/campground/<cg>/month?start_date=2026-07-01T00:00:00.000Z" \
     -H "user-agent: Mozilla/5.0" \
     | jq '.campsites | to_entries[] | select(.value.availabilities | to_entries[] | select(.value=="Available")) | {id: .key, site: .value.site}' \
     | head
   ```

   Each `campsite_id` returned by this endpoint is exactly what the booker
   expects as `-campsite`. The campground id you queried is `-campground`.
3. **Run the booker.**
   ```bash
   ./scripts/booker-run.sh -campsite <id> -campground <cg> -from YYYY-MM-DD -nights N
   ```
4. **Visit the printed URL** to finish checkout, or let the hold expire in
   ~15min.

## Debugging when it fails

- **`fatal: book: human verification required: Our system has detected
  abnormal activity from your computer network`** — risk score dropped.
  Causes seen: VPN/datacenter IP, fresh profile dir with no
  `r1s-fingerprint` history, using `--headless`, using a 2captcha-minted
  token (see below). Cures: slow down, run from residential IP, warm a
  profile with manual browsing first, never use `--headless`.
- **`fatal: login: bad credentials`** — wrong password, MFA enabled on
  account, or rec.gov is rejecting the login challenge. The form-fill
  path doesn't yet handle the latter two; you'd need to log in manually
  once in the same profile dir to bootstrap.
- **Stale Chrome lock** (`SingletonLock`) is auto-cleared on launch but
  occasionally Chrome still refuses; deleting `.cache/recgov-profile`
  entirely and re-running is the brute-force fix (you lose the cookie
  warmup).
- **Inspect the last response**:
  ```bash
  jq . .cache/debug/last-response.json
  ```
- **Inspect the page state at the moment of failure**: on errors,
  `internal/booker/booker.go::Open` doesn't currently dump
  screenshots/HTML — the older `cmd/booker` did via `dumpDebug`; that
  helper has not been ported into the library. If you need it, copy
  `dumpDebug` from the git history of `cmd/booker/main.go` pre-refactor.

## What the 2captcha test showed (2026-06-07)

We added a `-use-2captcha` flag and ran the same booking flow but had
2captcha mint the reCAPTCHA Enterprise v3 token instead of letting the
page do it. Result:

| Path                                       | Outcome                                                                 | Time            |
| ------------------------------------------ | ----------------------------------------------------------------------- | --------------- |
| In-page `grecaptcha.enterprise.execute()`  | ✅ Held                                                                  | ~1.5s           |
| 2captcha-minted token spliced into `gate_a`| ❌ "abnormal activity" (mapped to `ErrHumanVerification`)                | ~16s + rejection|

reCAPTCHA Enterprise v3 tokens are silently bound to the issuing browser's
fingerprint (IP, UA, cookie state). 2captcha mints from their farm; when
our session POSTs that token, Google's risk engine sees the mismatch and
returns a near-zero score, which rec.gov surfaces as the abnormal-activity
rejection. The flag is left in place as a kill-switch but should be
considered non-functional until we find a captcha provider that supports
Enterprise v3 with proper fingerprint binding.

## Files that matter

| Path                                | Role                                                              |
| ----------------------------------- | ----------------------------------------------------------------- |
| `docker/Dockerfile.booker`          | Container image: golang:1.26-bookworm + Chrome + Xvfb + tini.     |
| `scripts/booker-run.sh`             | Build-and-run wrapper. Handles `2CAPTCHA_API_KEY` aliasing.       |
| `scripts/booker-shell.sh`           | Interactive shell in the dev container.                           |
| `cmd/booker/main.go`                | CLI; parses flags, calls the library.                             |
| `internal/booker/booker.go`         | `Session`: launch Chrome, `Login`, `HoldCampsite[WithToken]`.     |
| `internal/booker/pool.go`           | Per-user warm pool (for production bot integration).              |
| `internal/booker/selection.go`      | Pure selection algorithm + tests.                                 |
| `internal/booker/captcha.go`        | (planned — 2captcha solver wrapper; currently inline in `cmd/booker/main.go`.) |
| `.cache/recgov-profile/`            | Persistent Chrome user-data-dir.                                  |
| `.cache/debug/last-response.json`   | Most recent rec.gov response body.                                |
