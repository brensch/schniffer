package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"strings"

	_ "github.com/marcboeker/go-duckdb"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	return OpenWithMode(path, "READ_WRITE")
}

// OpenReadOnly opens the database in READ_ONLY mode (no write lock)
func OpenReadOnly(path string) (*Store, error) { return OpenWithMode(path, "READ_ONLY") }

// OpenWithMode allows specifying DuckDB access_mode (READ_WRITE or READ_ONLY)
func OpenWithMode(path, mode string) (*Store, error) {
	if mode == "" {
		mode = "READ_WRITE"
	}
	dsn := fmt.Sprintf("%s?access_mode=%s", path, mode)
	fmt.Println("Connecting to DuckDB:", dsn)
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if strings.EqualFold(mode, "READ_WRITE") {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func migrate(db *sql.DB) error {
	schemaBytes, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	_, err = db.Exec(string(schemaBytes))
	return err
}

// Models

type SchniffRequest struct {
	ID           int64
	UserID       string
	Provider     string
	CampgroundID string
	// Checkin is the arrival date (inclusive), Checkout is the departure date (exclusive)
	Checkin   time.Time
	Checkout  time.Time
	CreatedAt time.Time
	Active    bool
}

type CampsiteState struct {
	Provider     string
	CampgroundID string
	CampsiteID   string
	Date         time.Time
	Available    bool
	CheckedAt    time.Time
}

type LookupLog struct {
	Provider     string
	CampgroundID string
	Month        time.Time
	StartDate    time.Time
	EndDate      time.Time
	CheckedAt    time.Time
	Success      bool
	Err          string
}

type Notification struct {
	RequestID    int64
	UserID       string
	Provider     string
	CampgroundID string
	CampsiteID   string
	Date         time.Time
	State        string // available|unavailable
	SentAt       time.Time
}

// IncomingCampsiteState represents the latest observed state for a campsite on a date.
type IncomingCampsiteState struct {
	CampsiteID string
	Date       time.Time
	Available  bool
}

// AvailabilityItem describes a newly opened availability to notify a user about.
type AvailabilityItem struct {
	CampsiteID string
	Date       time.Time
}

type SyncLog struct {
	SyncType   string
	Provider   string
	StartedAt  time.Time
	FinishedAt time.Time
	Success    bool
	Err        string
	Count      int64
}

// CRUD

func (s *Store) AddRequest(ctx context.Context, r SchniffRequest) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		INSERT INTO schniff_requests(user_id, provider, campground_id, start_date, end_date, checkin, checkout, created_at, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, now(), true)
		RETURNING id
	`, r.UserID, r.Provider, r.CampgroundID, r.Checkin, r.Checkout, r.Checkin, r.Checkout)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) ListActiveRequests(ctx context.Context) ([]SchniffRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `
	SELECT id, user_id, provider, campground_id, coalesce(checkin, start_date) as checkin, coalesce(checkout, end_date) as checkout, created_at, active
		FROM schniff_requests WHERE active=true
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchniffRequest
	for rows.Next() {
		var r SchniffRequest
		if err := rows.Scan(&r.ID, &r.UserID, &r.Provider, &r.CampgroundID, &r.Checkin, &r.Checkout, &r.CreatedAt, &r.Active); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeactivateRequest(ctx context.Context, id int64, userID string) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE schniff_requests SET active=false WHERE id=? AND user_id=?
	`, id, userID)
	if err != nil {
		return err
	}
	a, _ := res.RowsAffected()
	if a == 0 {
		return errors.New("not found or not owner")
	}
	return nil
}

// Convenience: list active requests for a specific user
func (s *Store) ListUserActiveRequests(ctx context.Context, userID string) ([]SchniffRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, provider, campground_id, coalesce(checkin, start_date) as checkin, coalesce(checkout, end_date) as checkout, created_at, active
		FROM schniff_requests WHERE active=true AND user_id=?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchniffRequest
	for rows.Next() {
		var r SchniffRequest
		if err := rows.Scan(&r.ID, &r.UserID, &r.Provider, &r.CampgroundID, &r.Checkin, &r.Checkout, &r.CreatedAt, &r.Active); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertCampsiteStateBatch(ctx context.Context, states []CampsiteState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO campsite_state(provider, campground_id, campsite_id, date, available, checked_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, st := range states {
		if _, err := stmt.ExecContext(ctx, st.Provider, st.CampgroundID, st.CampsiteID, st.Date, st.Available, st.CheckedAt); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

func (s *Store) RecordLookup(ctx context.Context, l LookupLog) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO lookup_log(provider, campground_id, month, start_date, end_date, checked_at, success, err)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, l.Provider, l.CampgroundID, l.Month, l.StartDate, l.EndDate, l.CheckedAt, l.Success, l.Err)
	return err
}

func (s *Store) RecordNotification(ctx context.Context, n Notification) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO notifications(request_id, user_id, provider, campground_id, campsite_id, date, state, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, n.RequestID, n.UserID, n.Provider, n.CampgroundID, n.CampsiteID, n.Date, n.State, n.SentAt)
	return err
}

// ReconcileNotifications uses current campsite states to open or close notifications per user.
// For each (campsite_id,date):
//   - If current Available=true and the user's last recorded notification state is not "available",
//     insert an "available" row and include it in the return for bundling.
//   - If current Available=false and the user's last state is "available",
//     insert an "unavailable" row to close it.
//
// Dedup is per user (even if they have multiple overlapping requests). We record the row with
// the smallest request_id among the user's matching active requests for that date.
func (s *Store) ReconcileNotifications(ctx context.Context, provider, campgroundID string, reqs []SchniffRequest, states []IncomingCampsiteState) (map[string][]AvailabilityItem, error) {
	// Build per-date user -> requestID mapping from active requests
	perDateUserReq := map[string]map[string]int64{}
	for _, r := range reqs {
		// Only consider matching provider/campground (callers pass filtered; keep defensive)
		if r.Provider != provider || r.CampgroundID != campgroundID || !r.Active {
			continue
		}
		for d := normalizeDay(r.Checkin); d.Before(normalizeDay(r.Checkout)); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			m, ok := perDateUserReq[key]
			if !ok {
				m = map[string]int64{}
				perDateUserReq[key] = m
			}
			id, ok := m[r.UserID]
			if !ok || r.ID < id {
				m[r.UserID] = r.ID
			}
		}
	}

	// Prepare statements for performance
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		// If not committed due to early return, rollback
		_ = tx.Rollback()
	}()

	stLast, err := tx.PrepareContext(ctx, `
		SELECT state FROM notifications
		WHERE user_id=? AND provider=? AND campground_id=? AND campsite_id=? AND date=?
		ORDER BY sent_at DESC LIMIT 1
	`)
	if err != nil {
		return nil, err
	}
	defer stLast.Close()

	stInsert, err := tx.PrepareContext(ctx, `
		INSERT INTO notifications(request_id, user_id, provider, campground_id, campsite_id, date, state, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return nil, err
	}
	defer stInsert.Close()

	newly := map[string][]AvailabilityItem{}
	now := time.Now()
	for _, st := range states {
		dateKey := normalizeDay(st.Date).Format("2006-01-02")
		userReqs, ok := perDateUserReq[dateKey]
		if !ok || len(userReqs) == 0 {
			continue
		}
		for userID, reqID := range userReqs {
			// check last
			var last string
			err := stLast.QueryRowContext(ctx, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date)).Scan(&last)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			hadOpen := (err == nil && strings.EqualFold(last, "available"))
			if st.Available {
				if !hadOpen { // open it
					if _, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "available", now); err != nil {
						return nil, err
					}
					newly[userID] = append(newly[userID], AvailabilityItem{CampsiteID: st.CampsiteID, Date: normalizeDay(st.Date)})
				}
			} else { // unavailable
				if hadOpen { // close it
					if _, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "unavailable", now); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return newly, nil
}

// normalizeDay returns time truncated to 00:00:00 UTC
func normalizeDay(t time.Time) time.Time {
	tt := t.UTC()
	return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, time.UTC)
}

// Aggregations & stats

func (s *Store) CountLookupsLast24h(ctx context.Context, provider, campgroundID string) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM lookup_log
	WHERE provider=? AND campground_id=? AND CAST(checked_at AS TIMESTAMP) >= CAST(now() AS TIMESTAMP) - INTERVAL '1 day'
	`, provider, campgroundID)
	var n int64
	return n, row.Scan(&n)
}

// CountLookupsTotal returns total number of lookup_log rows for a provider+campground.
func (s *Store) CountLookupsTotal(ctx context.Context, provider, campgroundID string) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM lookup_log
		WHERE provider=? AND campground_id=?
	`, provider, campgroundID)
	var n int64
	return n, row.Scan(&n)
}

// CountLookupsForRequest returns the number of lookup_log rows for a provider+campground
// bounded by the schniff's lifecycle and date span. We consider checks with checked_at >= createdAt
// (start of schniff) up to now (for active schniffs) and only those where the lookup month/start
// falls within [checkin .. checkout] to approximate the relevant date span.
func (s *Store) CountLookupsForRequest(ctx context.Context, provider, campgroundID string, createdAt, checkin, checkout time.Time) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM lookup_log
		WHERE provider=? AND campground_id=?
					AND CAST(checked_at AS TIMESTAMP) >= CAST(? AS TIMESTAMP)
					AND (
						-- any overlap between logged lookup date span and schniff date span
						(start_date IS NOT NULL AND end_date IS NOT NULL AND start_date < ? AND end_date > ?)
						OR (start_date IS NULL AND end_date IS NULL AND month BETWEEN ? AND ?)
					)
		`, provider, campgroundID, createdAt, checkout, checkin, checkin, checkout)
	var n int64
	return n, row.Scan(&n)
}

func (s *Store) CountNotificationsLast24hByRequest(ctx context.Context, requestID int64) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM notifications
	WHERE request_id=? AND CAST(sent_at AS TIMESTAMP) >= CAST(now() AS TIMESTAMP) - INTERVAL '1 day'
	`, requestID)
	var n int64
	return n, row.Scan(&n)
}

type AvailabilityByDate struct {
	Date  time.Time
	Total int
	Free  int
}

// LatestAvailabilityByDate returns latest per-campsite state aggregated by date in [start, end] inclusive.
func (s *Store) LatestAvailabilityByDate(ctx context.Context, provider, campgroundID string, start, end time.Time) ([]AvailabilityByDate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT date, COUNT(DISTINCT campsite_id) AS total,
			   SUM(CASE WHEN available THEN 1 ELSE 0 END) AS free
		FROM (
			SELECT provider, campground_id, campsite_id, date, available,
				   ROW_NUMBER() OVER (PARTITION BY provider, campground_id, campsite_id, date ORDER BY checked_at DESC) AS rn
			FROM campsite_state
			WHERE provider=? AND campground_id=? AND date BETWEEN ? AND ?
		) t
		WHERE rn = 1
		GROUP BY date
		ORDER BY date
	`, provider, campgroundID, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AvailabilityByDate{}
	for rows.Next() {
		var a AvailabilityByDate
		if err := rows.Scan(&a.Date, &a.Total, &a.Free); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StatsToday returns active, lookups today, notifications today
func (s *Store) StatsToday(ctx context.Context) (active int64, lookups int64, notes int64, err error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
			   coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),0),
			   coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),0)
	`)
	err = row.Scan(&active, &lookups, &notes)
	return
}

// CountTotalRequests returns the total number of schniff_requests (active + inactive)
func (s *Store) CountTotalRequests(ctx context.Context) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM schniff_requests`)
	var n int64
	return n, row.Scan(&n)
}

func (s *Store) GetLastState(ctx context.Context, provider, campgroundID, campsiteID string, date time.Time) (bool, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT available FROM campsite_state
		WHERE provider=? AND campground_id=? AND campsite_id=? AND date=?
		ORDER BY checked_at DESC LIMIT 1
	`, provider, campgroundID, campsiteID, date)
	var available bool
	switch err := row.Scan(&available); err {
	case nil:
		return available, true, nil
	case sql.ErrNoRows:
		return false, false, nil
	default:
		return false, false, err
	}
}

// Metadata

func (s *Store) UpsertCampground(ctx context.Context, provider, id, name string, lat, lon float64) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO campgrounds(provider, id, name, lat, lon)
		VALUES (?, ?, ?, ?, ?)
	`, provider, id, name, lat, lon)
	return err
}

func (s *Store) UpsertCampsiteMeta(ctx context.Context, provider, campgroundID, campsiteID, name string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO campsites_meta(provider, campground_id, campsite_id, name)
		VALUES (?, ?, ?, ?)
	`, provider, campgroundID, campsiteID, name)
	return err
}

type Campground struct {
	Provider string
	ID       string
	Name     string
	Lat      float64
	Lon      float64
}

func (s *Store) ListCampgrounds(ctx context.Context, like string) ([]Campground, error) {
	// Fuzzy search across campground names with simple ranking.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT provider, id, name, coalesce(lat, 0.0), coalesce(lon, 0.0)
		FROM campgrounds
		WHERE lower(name) LIKE '%' || lower(?) || '%'
		ORDER BY
			CASE
				WHEN lower(name) = lower(?) THEN 0
				WHEN lower(name) LIKE lower(?) || '%' THEN 1
				WHEN lower(name) LIKE '%' || lower(?) || '%' THEN 2
				ELSE 3
			END,
			name
		LIMIT 25
	`, like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campground
	for rows.Next() {
		var c Campground
		if err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCampgroundByID(ctx context.Context, provider, campgroundID string) (Campground, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT provider, id, name, coalesce(lat, 0.0), coalesce(lon, 0.0)
		FROM campgrounds
		WHERE provider=? AND id=?
	`, provider, campgroundID)
	var c Campground
	if err := row.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon); err != nil {
		if err == sql.ErrNoRows {
			return Campground{}, false, nil
		}
		return Campground{}, false, err
	}
	return c, true, nil
}

// Sync helpers
func (s *Store) RecordSync(ctx context.Context, l SyncLog) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO sync_log(sync_type, provider, started_at, finished_at, success, err, count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, l.SyncType, l.Provider, l.StartedAt, l.FinishedAt, l.Success, l.Err, l.Count)
	return err
}

func (s *Store) GetLastSuccessfulSync(ctx context.Context, syncType, provider string) (time.Time, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT finished_at FROM sync_log
		WHERE sync_type=? AND provider=? AND success=true
		ORDER BY finished_at DESC LIMIT 1
	`, syncType, provider)
	var t time.Time
	switch err := row.Scan(&t); err {
	case nil:
		return t, true, nil
	case sql.ErrNoRows:
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, err
	}
}

// InsertDailySummarySnapshot aggregates and inserts today's snapshot into daily_summary.
func (s *Store) InsertDailySummarySnapshot(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO daily_summary(date, total_requests, active_requests, lookups, notifications, created_at)
		SELECT current_date,
			(SELECT count(*) FROM schniff_requests),
			(SELECT count(*) FROM schniff_requests WHERE active=true),
			(SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),
			(SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),
			now()
	`)
	return err
}
