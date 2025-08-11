-- schema for schniffer

CREATE SEQUENCE IF NOT EXISTS schniff_requests_id_seq START 1;
CREATE TABLE IF NOT EXISTS schniff_requests (
    id          BIGINT PRIMARY KEY DEFAULT nextval('schniff_requests_id_seq'),
    user_id     VARCHAR,
    provider    VARCHAR,
    campground_id VARCHAR,
    start_date  DATE,
    end_date    DATE,
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
    checked_at   TIMESTAMPTZ,
    success      BOOLEAN,
    err          VARCHAR
);

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
    campground_id  VARCHAR,
    name           VARCHAR,
    parent_name    VARCHAR,
    parent_id      VARCHAR,
    lat            DOUBLE,
    lon            DOUBLE,
    PRIMARY KEY (provider, campground_id)
);

-- migrate existing installations: add columns if missing
ALTER TABLE campgrounds ADD COLUMN IF NOT EXISTS parent_name VARCHAR;
ALTER TABLE campgrounds ADD COLUMN IF NOT EXISTS parent_id VARCHAR;
ALTER TABLE campgrounds ADD COLUMN IF NOT EXISTS lat DOUBLE;
ALTER TABLE campgrounds ADD COLUMN IF NOT EXISTS lon DOUBLE;

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
