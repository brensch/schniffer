package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"strings"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}

// OpenReadOnly opens the database in READ_ONLY mode
func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
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
	Checkin      time.Time
	Checkout     time.Time
	CreatedAt    time.Time
	Active       bool
}

type CampsiteAvailability struct {
	Provider     string
	CampgroundID string
	CampsiteID   string
	Date         time.Time
	Available    bool
	LastChecked  time.Time
}

type LookupLog struct {
	ID            int64
	Provider      string
	CampgroundID  string
	StartDate     time.Time
	EndDate       time.Time
	CheckedAt     time.Time
	Success       bool
	ErrorMsg      string
	CampsiteCount int
}

type Notification struct {
	ID           int64
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

type MetadataSyncLog struct {
	ID         int64
	SyncType   string
	Provider   string
	StartedAt  time.Time
	FinishedAt time.Time
	Success    bool
	ErrorMsg   string
	Count      int
}

type DetailedSummaryStats struct {
	Notifications24h int64
	Lookups24h       int64
	ActiveRequests   int64
	RequestsPerHour  float64
}

// CRUD

func (s *Store) AddRequest(ctx context.Context, r SchniffRequest) (int64, error) {
	result, err := s.DB.ExecContext(ctx, `
		INSERT INTO schniff_requests(user_id, provider, campground_id, checkin, checkout, created_at, active)
		VALUES (?, ?, ?, ?, ?, datetime('now'), true)
	`, r.UserID, r.Provider, r.CampgroundID, r.Checkin, r.Checkout)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) ListActiveRequests(ctx context.Context) ([]SchniffRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, provider, campground_id, checkin, checkout, created_at, active
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
		SELECT id, user_id, provider, campground_id, checkin, checkout, created_at, active
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

// DeactivateExpiredRequests deactivates all active requests where the checkout date is before the current date
func (s *Store) DeactivateExpiredRequests(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE schniff_requests 
		SET active=false 
		WHERE active=true AND checkout < date('now')
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) UpsertCampsiteAvailabilityBatch(ctx context.Context, states []CampsiteAvailability) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO campsite_availability(provider, campground_id, campsite_id, date, available, last_checked)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, st := range states {
		if _, err := stmt.ExecContext(ctx, st.Provider, st.CampgroundID, st.CampsiteID, st.Date, st.Available, st.LastChecked); err != nil {
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
		INSERT INTO lookup_log(provider, campground_id, start_date, end_date, checked_at, success, error_msg, campsite_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, l.Provider, l.CampgroundID, l.StartDate, l.EndDate, l.CheckedAt, l.Success, l.ErrorMsg, l.CampsiteCount)
	return err
}

func (s *Store) RecordNotification(ctx context.Context, n Notification) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO notifications(request_id, user_id, provider, campground_id, campsite_id, date, state, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`, n.RequestID, n.UserID, n.Provider, n.CampgroundID, n.CampsiteID, n.Date, n.State)
	return err
}

// ReconcileNotifications uses current campsite states to open or close notifications per user.
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
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`)
	if err != nil {
		return nil, err
	}
	defer stInsert.Close()

	newly := map[string][]AvailabilityItem{}
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
					if _, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "available"); err != nil {
						return nil, err
					}
					newly[userID] = append(newly[userID], AvailabilityItem{CampsiteID: st.CampsiteID, Date: normalizeDay(st.Date)})
				}
			} else { // unavailable
				if hadOpen { // close it
					if _, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "unavailable"); err != nil {
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
		WHERE provider=? AND campground_id=? AND checked_at >= datetime('now', '-1 day')
	`, provider, campgroundID)
	var n int64
	return n, row.Scan(&n)
}

func (s *Store) CountNotificationsLast24hByRequest(ctx context.Context, requestID int64) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM notifications
		WHERE request_id=? AND sent_at >= datetime('now', '-1 day')
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
		FROM campsite_availability
		WHERE provider=? AND campground_id=? AND date BETWEEN ? AND ?
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
		SELECT 
			coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
			coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=date('now')),0),
			coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=date('now')),0)
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
		SELECT available FROM campsite_availability
		WHERE provider=? AND campground_id=? AND campsite_id=? AND date=?
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

type Campground struct {
	Provider string
	ID       string
	Name     string
	Lat      float64
	Lon      float64
}

type CampgroundRef struct {
	Provider     string `json:"provider"`
	CampgroundID string `json:"campground_id"`
}

type Group struct {
	ID          int64           `json:"id"`
	UserID      string          `json:"user_id"`
	Name        string          `json:"name"`
	Campgrounds []CampgroundRef `json:"campgrounds"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
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

// GetAllCampgrounds returns all campgrounds without any limit
func (s *Store) GetAllCampgrounds(ctx context.Context) ([]Campground, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT provider, id, name, coalesce(lat, 0.0), coalesce(lon, 0.0)
		FROM campgrounds
		ORDER BY name
	`)
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
func (s *Store) RecordMetadataSync(ctx context.Context, l MetadataSyncLog) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO metadata_sync_log(sync_type, provider, started_at, finished_at, success, error_msg, count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, l.SyncType, l.Provider, l.StartedAt, l.FinishedAt, l.Success, l.ErrorMsg, l.Count)
	return err
}

func (s *Store) GetLastSuccessfulMetadataSync(ctx context.Context, syncType, provider string) (time.Time, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT finished_at FROM metadata_sync_log
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

// GetDetailedSummaryStats returns comprehensive stats for the detailed summary
func (s *Store) GetDetailedSummaryStats(ctx context.Context) (DetailedSummaryStats, error) {
	// Get basic stats for last 24 hours
	row := s.DB.QueryRowContext(ctx, `
		SELECT 
			coalesce((SELECT count(*) FROM notifications WHERE sent_at >= datetime('now', '-1 day') AND state = 'available'), 0) as notifications_24h,
			coalesce((SELECT count(*) FROM lookup_log WHERE checked_at >= datetime('now', '-1 day')), 0) as lookups_24h,
			coalesce((SELECT count(*) FROM schniff_requests WHERE active=true), 0) as active_requests
	`)

	var notifications24h, lookups24h, activeRequests int64
	if err := row.Scan(&notifications24h, &lookups24h, &activeRequests); err != nil {
		return DetailedSummaryStats{}, err
	}

	// Calculate requests per hour (last 24h lookups / 24)
	requestsPerHour := float64(lookups24h) / 24.0

	return DetailedSummaryStats{
		Notifications24h: notifications24h,
		Lookups24h:       lookups24h,
		ActiveRequests:   activeRequests,
		RequestsPerHour:  requestsPerHour,
	}, nil
}

// GetUsersWithNotifications returns users who got notifications in last 24h
func (s *Store) GetUsersWithNotifications(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT user_id 
		FROM notifications 
		WHERE sent_at >= datetime('now', '-1 day')
		ORDER BY user_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	return users, rows.Err()
}

// GetUsersWithActiveRequests returns users who have active schniffs
func (s *Store) GetUsersWithActiveRequests(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT user_id 
		FROM schniff_requests 
		WHERE active=true
		ORDER BY user_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	return users, rows.Err()
}

// GetTrackedCampgrounds returns list of campgrounds being actively tracked
func (s *Store) GetTrackedCampgrounds(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT c.name
		FROM campgrounds c
		JOIN schniff_requests sr ON c.provider = sr.provider AND c.id = sr.campground_id
		WHERE sr.active = true
		ORDER BY c.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var campgrounds []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		campgrounds = append(campgrounds, name)
	}
	return campgrounds, rows.Err()
}

// Group methods

func (s *Store) CreateGroup(ctx context.Context, userID, name string, campgrounds []CampgroundRef) (*Group, error) {
	if len(campgrounds) > 10 {
		return nil, errors.New("cannot create group with more than 10 campgrounds")
	}

	campgroundsJSON, err := json.Marshal(campgrounds)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal campgrounds: %w", err)
	}

	result, err := s.DB.ExecContext(ctx, `
		INSERT INTO groups (user_id, name, campgrounds, created_at, updated_at)
		VALUES (?, ?, ?, datetime('now'), datetime('now'))
	`, userID, name, string(campgroundsJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create group: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Group{
		ID:          id,
		UserID:      userID,
		Name:        name,
		Campgrounds: campgrounds,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}, nil
}

func (s *Store) GetUserGroups(ctx context.Context, userID string) ([]Group, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, name, campgrounds, created_at, updated_at
		FROM groups
		WHERE user_id = ?
		ORDER BY updated_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to query groups: %w", err)
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var group Group
		var campgroundsJSON string

		err := rows.Scan(&group.ID, &group.UserID, &group.Name, &campgroundsJSON, &group.CreatedAt, &group.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}

		if err := json.Unmarshal([]byte(campgroundsJSON), &group.Campgrounds); err != nil {
			return nil, fmt.Errorf("failed to unmarshal campgrounds for group %d: %w", group.ID, err)
		}

		groups = append(groups, group)
	}

	return groups, rows.Err()
}

func (s *Store) GetGroup(ctx context.Context, groupID int64, userID string) (*Group, error) {
	var group Group
	var campgroundsJSON string

	err := s.DB.QueryRowContext(ctx, `
		SELECT id, user_id, name, campgrounds, created_at, updated_at
		FROM groups
		WHERE id = ? AND user_id = ?
	`, groupID, userID).Scan(&group.ID, &group.UserID, &group.Name, &campgroundsJSON, &group.CreatedAt, &group.UpdatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errors.New("group not found")
		}
		return nil, fmt.Errorf("failed to get group: %w", err)
	}

	if err := json.Unmarshal([]byte(campgroundsJSON), &group.Campgrounds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal campgrounds for group %d: %w", group.ID, err)
	}

	return &group, nil
}
