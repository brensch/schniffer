-- schema for schniffer (SQLite)

CREATE TABLE IF NOT EXISTS schniff_requests (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    checkin     DATE NOT NULL,
    checkout    DATE NOT NULL,
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    active      BOOLEAN DEFAULT TRUE
);

CREATE INDEX IF NOT EXISTS idx_schniff_requests_active ON schniff_requests(active);
CREATE INDEX IF NOT EXISTS idx_schniff_requests_user ON schniff_requests(user_id);
-- Additional indexes for request queries
CREATE INDEX IF NOT EXISTS idx_schniff_requests_active_provider ON schniff_requests(active, provider, campground_id) WHERE active=1;
CREATE INDEX IF NOT EXISTS idx_schniff_requests_dates ON schniff_requests(provider, campground_id, checkin, checkout) WHERE active=1;

-- Enhanced schniffer database schema
-- Version 2: Added campsite type, cost, campground ratings and amenities

PRAGMA user_version = 2;

CREATE TABLE IF NOT EXISTS campsite_availability (
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id  TEXT NOT NULL,
    date         DATE NOT NULL,
    available    BOOLEAN NOT NULL,
    last_checked DATETIME NOT NULL,
    PRIMARY KEY (provider, campground_id, campsite_id, date)
);

CREATE INDEX IF NOT EXISTS idx_availability_lookup ON campsite_availability(provider, campground_id, date);
CREATE INDEX IF NOT EXISTS idx_availability_stale ON campsite_availability(last_checked);
CREATE INDEX IF NOT EXISTS idx_availability_available_filtered ON campsite_availability(provider, campground_id, available, date) WHERE available=1;
CREATE INDEX IF NOT EXISTS idx_availability_date_range ON campsite_availability(provider, campground_id, date, available);

-- CREATE TABLE IF NOT EXISTS requests (
--     id           INTEGER PRIMARY KEY AUTOINCREMENT,
--     user_id      TEXT NOT NULL,
--     provider     TEXT NOT NULL,
--     campground_id TEXT NOT NULL,
--     start_date   DATE NOT NULL,
--     end_date     DATE NOT NULL,
--     status       TEXT NOT NULL,
--     created_at   DATETIME NOT NULL,
--     last_checked DATETIME NOT NULL
-- );

CREATE TABLE IF NOT EXISTS campgrounds (
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    name         TEXT NOT NULL,
    latitude     REAL DEFAULT 0,
    longitude    REAL DEFAULT 0,
    rating       REAL DEFAULT 0,
    amenities    TEXT DEFAULT '[]',
    image_url    TEXT DEFAULT '',
    price_min    REAL DEFAULT 0,
    price_max    REAL DEFAULT 0,
    price_unit   TEXT DEFAULT 'night',
    last_updated DATETIME NOT NULL,

    -- adding these for more efficient queries
    campsite_types TEXT DEFAULT '[]', -- JSON array of campsite types
    equipment    TEXT DEFAULT '[]', -- JSON array of equipment types

    PRIMARY KEY (provider, campground_id)
);

CREATE INDEX IF NOT EXISTS idx_campgrounds_location ON campgrounds(latitude, longitude);
CREATE INDEX IF NOT EXISTS idx_campgrounds_rating ON campgrounds(rating);
CREATE INDEX IF NOT EXISTS idx_campgrounds_price ON campgrounds(price_min, price_max);

-- Campsites metadata table (separate from availability) for filtering and detailed information
CREATE TABLE IF NOT EXISTS campsite_metadata (
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id  TEXT NOT NULL,
    name         TEXT NOT NULL,
    campsite_type TEXT DEFAULT '',
    cost_per_night REAL DEFAULT 0,
    rating       REAL DEFAULT 0,
    last_updated DATETIME NOT NULL,
    image_url    TEXT DEFAULT '',
    PRIMARY KEY (provider, campground_id, campsite_id)
);

CREATE INDEX IF NOT EXISTS idx_campsite_metadata_campground ON campsite_metadata(provider, campground_id);
CREATE INDEX IF NOT EXISTS idx_campsite_metadata_type ON campsite_metadata(campsite_type);
CREATE INDEX IF NOT EXISTS idx_campsite_metadata_rating ON campsite_metadata(rating);

-- Equipment types available at each campsite (normalized many-to-many)
CREATE TABLE IF NOT EXISTS campsite_equipment (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    provider TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id TEXT NOT NULL,
    equipment_type TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider, campground_id, campsite_id, equipment_type)
);

-- -- Table to store pre-computed campsite types for each campground
-- CREATE TABLE IF NOT EXISTS campground_types (
--     id INTEGER PRIMARY KEY AUTOINCREMENT,
--     provider TEXT NOT NULL,
--     campground_id TEXT NOT NULL,
--     campsite_type TEXT NOT NULL,
--     created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
--     updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
--     UNIQUE(provider, campground_id, campsite_type)
-- );

-- Indexes for campground_types table (commented out since table is commented out)
-- CREATE INDEX IF NOT EXISTS idx_campground_types_lookup ON campground_types(provider, campground_id);
-- CREATE INDEX IF NOT EXISTS idx_campground_types_composite ON campground_types(provider, campground_id, campsite_type);

CREATE INDEX IF NOT EXISTS idx_campsite_equipment_campground ON campsite_equipment(provider, campground_id);
CREATE INDEX IF NOT EXISTS idx_campsite_equipment_type ON campsite_equipment(equipment_type);

-- Lookup log for API calls (for summaries)
CREATE TABLE IF NOT EXISTS lookup_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    start_date   DATE NOT NULL,
    end_date     DATE NOT NULL,
    checked_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    success      BOOLEAN NOT NULL,
    error_msg    TEXT,
    campsite_count INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_lookup_log_time ON lookup_log(checked_at);
CREATE INDEX IF NOT EXISTS idx_lookup_log_provider ON lookup_log(provider, campground_id);
-- Additional index for recent success lookups
CREATE INDEX IF NOT EXISTS idx_lookup_log_recent_success ON lookup_log(provider, campground_id, checked_at DESC) WHERE success=1;

-- State changes tracking
CREATE TABLE IF NOT EXISTS state_changes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id  TEXT NOT NULL,
    date         DATE NOT NULL,
    new_available BOOLEAN NOT NULL,
    changed_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(provider, campground_id, campsite_id, date, changed_at)
);

CREATE INDEX IF NOT EXISTS idx_state_changes_lookup ON state_changes(provider, campground_id, date, changed_at);
CREATE INDEX IF NOT EXISTS idx_state_changes_campsite ON state_changes(provider, campground_id, campsite_id, date, changed_at);

-- Notifications history
CREATE TABLE IF NOT EXISTS notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    batch_id     TEXT NOT NULL,  -- UUID to group notifications sent together
    request_id   INTEGER NOT NULL,
    user_id      TEXT NOT NULL,
    provider     TEXT NOT NULL,
    campground_id TEXT NOT NULL,
    campsite_id  TEXT NOT NULL,
    date         DATE NOT NULL,
    state        TEXT NOT NULL, -- available|unavailable
    state_change_id INTEGER,     -- Reference to the state change that triggered this notification
    sent_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (request_id) REFERENCES schniff_requests(id),
    FOREIGN KEY (state_change_id) REFERENCES state_changes(id)
);

CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id);
CREATE INDEX IF NOT EXISTS idx_notifications_time ON notifications(sent_at);
CREATE INDEX IF NOT EXISTS idx_notifications_request ON notifications(request_id);
CREATE INDEX IF NOT EXISTS idx_notifications_batch ON notifications(batch_id);
CREATE INDEX IF NOT EXISTS idx_notifications_last_batch ON notifications(request_id, sent_at);
-- Additional indexes for notification comparison queries
CREATE INDEX IF NOT EXISTS idx_notifications_provider_lookup ON notifications(provider, campground_id, date, batch_id);
CREATE INDEX IF NOT EXISTS idx_notifications_batch_latest ON notifications(provider, campground_id, sent_at DESC, batch_id);
CREATE INDEX IF NOT EXISTS idx_notifications_composite ON notifications(provider, campground_id, date, sent_at DESC);

-- Metadata sync log (for campground syncing)
CREATE TABLE IF NOT EXISTS metadata_sync_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    sync_type   TEXT NOT NULL,
    provider    TEXT NOT NULL,
    started_at  DATETIME NOT NULL,
    finished_at DATETIME,
    success     BOOLEAN,
    error_msg   TEXT,
    count       INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_metadata_sync_recent ON metadata_sync_log(sync_type, provider, finished_at);

-- User groups for saving campground selections
CREATE TABLE IF NOT EXISTS groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     TEXT NOT NULL,
    name        TEXT NOT NULL,
    campgrounds TEXT NOT NULL, -- JSON array of {provider: string, campground_id: string}
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_groups_user ON groups(user_id);
