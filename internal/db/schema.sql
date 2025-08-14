-- schema for schniffer

CREATE SEQUENCE IF NOT EXISTS schniff_requests_id_seq START 1;
CREATE TABLE IF NOT EXISTS schniff_requests (
    id          BIGINT PRIMARY KEY DEFAULT nextval('schniff_requests_id_seq'),
    user_id     VARCHAR,
    provider    VARCHAR,
    campground_id VARCHAR,
    start_date  DATE,
    end_date    DATE,
    -- new semantics: checkin (inclusive) and checkout (exclusive)
    checkin     DATE,
    checkout    DATE,
    created_at  TIMESTAMPTZ,
    active      BOOLEAN
);

CREATE TABLE IF NOT EXISTS campsite_state (
    provider     VARCHAR,
    campground_id VARCHAR,
    campsite_id  VARCHAR,
    date         DATE,
    available    BOOLEAN,
    checked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_campsite_state_lookup ON campsite_state(provider, campground_id, campsite_id, date, checked_at);

CREATE TABLE IF NOT EXISTS lookup_log (
    provider     VARCHAR,
    campground_id VARCHAR,
    month        DATE,
    -- new: explicit request date span for this lookup
    start_date   DATE,
    end_date     DATE,
    checked_at   TIMESTAMPTZ,
    success      BOOLEAN,
    err          VARCHAR
);

CREATE INDEX IF NOT EXISTS idx_lookup_log_bucket ON lookup_log(provider, campground_id, month, checked_at);
-- supporting index for range queries
CREATE INDEX IF NOT EXISTS idx_lookup_log_range ON lookup_log(provider, campground_id, start_date, end_date, checked_at);

CREATE TABLE IF NOT EXISTS notifications (
    request_id    BIGINT,
    user_id       VARCHAR,
    provider      VARCHAR,
    campground_id VARCHAR,
    campsite_id   VARCHAR,
    date          DATE,
    state         VARCHAR, -- available|unavailable
    sent_at       TIMESTAMPTZ
);

-- daily stats snapshots
CREATE TABLE IF NOT EXISTS daily_summary (
    date          DATE,
    total_requests BIGINT,
    active_requests BIGINT,
    lookups        BIGINT,
    notifications  BIGINT,
    created_at     TIMESTAMPTZ
);

-- campground metadata
CREATE TABLE IF NOT EXISTS campgrounds (
    provider       VARCHAR,
    id             VARCHAR,
    name           VARCHAR,
    lat            DOUBLE,
    lon            DOUBLE,
    PRIMARY KEY (provider, id)
);

-- add new request columns if missing
ALTER TABLE schniff_requests ADD COLUMN IF NOT EXISTS checkin DATE;
ALTER TABLE schniff_requests ADD COLUMN IF NOT EXISTS checkout DATE;
-- backfill from old columns if present and new ones are null
UPDATE schniff_requests SET checkin = COALESCE(checkin, start_date);
UPDATE schniff_requests SET checkout = COALESCE(checkout, end_date);

-- migrate lookup_log to add date range columns if missing
ALTER TABLE lookup_log ADD COLUMN IF NOT EXISTS start_date DATE;
ALTER TABLE lookup_log ADD COLUMN IF NOT EXISTS end_date DATE;

CREATE TABLE IF NOT EXISTS campsites_meta (
    provider       VARCHAR,
    campground_id  VARCHAR,
    campsite_id    VARCHAR,
    name           VARCHAR,
    PRIMARY KEY (provider, campground_id, campsite_id)
);

-- sync logs (e.g., campground syncs)
CREATE TABLE IF NOT EXISTS sync_log (
    sync_type    VARCHAR,
    provider     VARCHAR,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    success      BOOLEAN,
    err          VARCHAR,
    count        BIGINT
);
CREATE INDEX IF NOT EXISTS idx_sync_log_recent ON sync_log(sync_type, provider, finished_at);

-- user groups for saving campground selections
CREATE SEQUENCE IF NOT EXISTS groups_id_seq START 1;
CREATE TABLE IF NOT EXISTS groups (
    id          BIGINT PRIMARY KEY DEFAULT nextval('groups_id_seq'),
    user_id     VARCHAR NOT NULL,
    name        VARCHAR NOT NULL,
    campgrounds JSON NOT NULL, -- array of {provider: string, campground_id: string}
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_groups_user ON groups(user_id);
