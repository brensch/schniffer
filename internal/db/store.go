package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"strings"

	"github.com/brensch/schniffer/internal/providers"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stephennancekivell/querypulse"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	// Register the wrapped SQLite driver with query logging
	driverName, err := querypulse.Register("sqlite3", querypulse.Options{
		OnSuccess: func(ctx context.Context, query string, args []any, duration time.Duration) {
			if duration > 10*time.Millisecond {
				slog.Info("slow query succeeded", slog.Any("args", args), slog.String("query", query), slog.Duration("took", duration))
			}
		},
	})

	db, err := sql.Open(driverName, path+"?_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	err = db.Ping()
	if err != nil {
		return nil, err
	}
	err = migrate(db)
	if err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}

// OpenReadOnly opens the database in READ_ONLY mode
func OpenReadOnly(path string) (*Store, error) {
	// Register the wrapped SQLite driver with query logging
	driverName, err := querypulse.Register("sqlite3", querypulse.Options{
		OnSuccess: func(ctx context.Context, query string, args []any, duration time.Duration) {
			if duration > 10*time.Millisecond {
				slog.Debug("query succeeded", slog.String("query", query), slog.Duration("took", duration))
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register query logging driver: %w", err)
	}

	db, err := sql.Open(driverName, path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	err = db.Ping()
	if err != nil {
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

type StateChange struct {
	ID           int64
	Provider     string
	CampgroundID string
	CampsiteID   string
	Date         time.Time
	NewAvailable bool
	ChangedAt    time.Time
}

type StateChangeForRequest struct {
	ID           int64
	Provider     string
	CampgroundID string
	CampsiteID   string
	Date         time.Time
	NewAvailable bool
	ChangedAt    time.Time
	RequestID    int64
}

type Notification struct {
	ID            int64     `db:"id"`
	BatchID       string    `db:"batch_id"`
	RequestID     int64     `db:"request_id"`
	UserID        string    `db:"user_id"`
	Provider      string    `db:"provider"`
	CampgroundID  string    `db:"campground_id"`
	CampsiteID    string    `db:"campsite_id"`
	Date          time.Time `db:"date"`
	State         string    `db:"state"`
	StateChangeID *int64    `db:"state_change_id"`
	SentAt        time.Time `db:"sent_at"`
}

// NotificationResult represents the result of checking if notifications should be sent
type NotificationResult struct {
	RequestID          int64
	UserID             string
	Provider           string
	CampgroundID       string
	ShouldNotify       bool
	CurrentlyAvailable []AvailabilityItem
	NewlyAvailable     []AvailabilityItem // marked as "new"
	NewlyUnavailable   []AvailabilityItem // marked as "booked"
}

// IncomingCampsiteState represents the latest observed state for a campsite on a date.
type IncomingCampsiteState struct {
	CampsiteID   string
	Date         time.Time
	Available    bool
	Type         string
	CostPerNight float64
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

type NotificationData struct {
	RequestID        int64
	UserID           string
	Provider         string
	CampgroundID     string
	NewlyAvailable   []AvailabilityItem // campsites that just became available
	NewlyUnavailable []AvailabilityItem // campsites that just became unavailable
	StillAvailable   []AvailabilityItem // other campsites still available at this campground
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
		err := rows.Scan(&r.ID, &r.UserID, &r.Provider, &r.CampgroundID, &r.Checkin, &r.Checkout, &r.CreatedAt, &r.Active)
		if err != nil {
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
		err := rows.Scan(&r.ID, &r.UserID, &r.Provider, &r.CampgroundID, &r.Checkin, &r.Checkout, &r.CreatedAt, &r.Active)
		if err != nil {
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

// UpsertCampsiteAvailabilityBatch updates availability and detects state changes
func (s *Store) UpsertCampsiteAvailabilityBatch(ctx context.Context, states []CampsiteAvailability) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Prepare statements
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO campsite_availability(provider, campground_id, campsite_id, date, available, last_checked)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	stateChangeStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO state_changes(provider, campground_id, campsite_id, date, new_available, changed_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stateChangeStmt.Close()

	// Get previous states for comparison
	prevStates := make(map[string]bool) // key: provider_campground_campsite_date, value: was_available
	for _, st := range states {
		key := fmt.Sprintf("%s_%s_%s_%s", st.Provider, st.CampgroundID, st.CampsiteID, st.Date.Format("2006-01-02"))

		var prevAvailable bool
		err := tx.QueryRowContext(ctx, `
			SELECT available FROM campsite_availability 
			WHERE provider=? AND campground_id=? AND campsite_id=? AND date=?
		`, st.Provider, st.CampgroundID, st.CampsiteID, st.Date).Scan(&prevAvailable)

		if err == nil {
			prevStates[key] = prevAvailable
		}
		// If err != nil, this is a new entry (no previous state)
	}

	now := time.Now()

	for _, st := range states {
		// Update availability
		_, err := stmt.ExecContext(ctx, st.Provider, st.CampgroundID, st.CampsiteID, st.Date, st.Available, st.LastChecked)
		if err != nil {
			return err
		}

		// Check for state change
		key := fmt.Sprintf("%s_%s_%s_%s", st.Provider, st.CampgroundID, st.CampsiteID, st.Date.Format("2006-01-02"))
		prevAvailable, hadPrevious := prevStates[key]

		// Record state change if:
		// 1. No previous state and now available (ignore new unavailable entries)
		// 2. Previous state different from current state
		shouldRecord := false
		if !hadPrevious && st.Available {
			shouldRecord = true // New available site
		} else if hadPrevious && prevAvailable != st.Available {
			shouldRecord = true // State changed
		}

		if shouldRecord {
			_, err := stateChangeStmt.ExecContext(ctx, st.Provider, st.CampgroundID, st.CampsiteID, st.Date, st.Available, now)
			if err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// GetAvailabilityChangesForNotifications finds what has changed since the last notification batch for a schniff request
// Returns: available campsites, newly available ones, newly booked ones
func (s *Store) GetAvailabilityChangesForNotifications(ctx context.Context, providerName string, campgroundIds []string, startDate, endDate time.Time) ([]CampsiteAvailability, []CampsiteAvailability, []CampsiteAvailability, error) {
	// Build placeholders for campground IDs
	placeholders := make([]string, len(campgroundIds))
	args := []interface{}{providerName}
	for i, id := range campgroundIds {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, startDate, endDate)

	// Get current available campsites in date range
	availableQuery := fmt.Sprintf(`
		SELECT provider, campground_id, campsite_id, date, available, last_checked
		FROM campsite_availability 
		WHERE provider=? AND campground_id IN (%s) AND date >= ? AND date <= ? AND available=1
	`, strings.Join(placeholders, ","))

	availableRows, err := s.DB.QueryContext(ctx, availableQuery, args...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer availableRows.Close()

	var currentAvailable []CampsiteAvailability
	for availableRows.Next() {
		var ca CampsiteAvailability
		err := availableRows.Scan(&ca.Provider, &ca.CampgroundID, &ca.CampsiteID, &ca.Date, &ca.Available, &ca.LastChecked)
		if err != nil {
			return nil, nil, nil, err
		}
		currentAvailable = append(currentAvailable, ca)
	}

	// Get the most recent notification batch for these campgrounds in this date range
	lastBatchQuery := fmt.Sprintf(`
		SELECT DISTINCT campground_id, campsite_id, date 
		FROM notifications 
		WHERE provider=? AND campground_id IN (%s) AND date >= ? AND date <= ?
		AND batch_id = (
			SELECT batch_id 
			FROM notifications 
			WHERE provider=? AND campground_id IN (%s) AND date >= ? AND date <= ?
			ORDER BY sent_at DESC 
			LIMIT 1
		)
	`, strings.Join(placeholders, ","), strings.Join(placeholders, ","))

	// Args for last batch query: provider, campground_ids, start, end, provider, campground_ids, start, end
	lastBatchArgs := []interface{}{providerName}
	lastBatchArgs = append(lastBatchArgs, args[1:]...) // campground_ids, start, end
	lastBatchArgs = append(lastBatchArgs, providerName)
	lastBatchArgs = append(lastBatchArgs, args[1:]...) // campground_ids, start, end again

	lastBatchRows, err := s.DB.QueryContext(ctx, lastBatchQuery, lastBatchArgs...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer lastBatchRows.Close()

	// Build set of previously notified campsites
	previouslyNotified := make(map[string]bool)
	for lastBatchRows.Next() {
		var campgroundID, campsiteID string
		var date time.Time
		err := lastBatchRows.Scan(&campgroundID, &campsiteID, &date)
		if err != nil {
			return nil, nil, nil, err
		}
		key := fmt.Sprintf("%s_%s_%s", campgroundID, campsiteID, date.Format("2006-01-02"))
		previouslyNotified[key] = true
	}

	// Categorize current availability
	var newlyAvailable []CampsiteAvailability
	for _, ca := range currentAvailable {
		key := fmt.Sprintf("%s_%s_%s", ca.CampgroundID, ca.CampsiteID, ca.Date.Format("2006-01-02"))
		if !previouslyNotified[key] {
			newlyAvailable = append(newlyAvailable, ca)
		}
	}

	// Get newly booked (were available in last batch, now not available)
	newlyBookedQuery := fmt.Sprintf(`
		SELECT DISTINCT n.campground_id, n.campsite_id, n.date
		FROM notifications n
		LEFT JOIN campsite_availability ca ON (
			n.provider = ca.provider AND 
			n.campground_id = ca.campground_id AND 
			n.campsite_id = ca.campsite_id AND 
			n.date = ca.date
		)
		WHERE n.provider=? AND n.campground_id IN (%s) AND n.date >= ? AND n.date <= ?
		AND n.batch_id = (
			SELECT batch_id 
			FROM notifications 
			WHERE provider=? AND campground_id IN (%s) AND date >= ? AND date <= ?
			ORDER BY sent_at DESC 
			LIMIT 1
		)
		AND (ca.available = 0 OR ca.available IS NULL)
	`, strings.Join(placeholders, ","), strings.Join(placeholders, ","))

	newlyBookedRows, err := s.DB.QueryContext(ctx, newlyBookedQuery, lastBatchArgs...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer newlyBookedRows.Close()

	var newlyBooked []CampsiteAvailability
	for newlyBookedRows.Next() {
		var campgroundID, campsiteID string
		var date time.Time
		err := newlyBookedRows.Scan(&campgroundID, &campsiteID, &date)
		if err != nil {
			return nil, nil, nil, err
		}
		// Create a placeholder availability entry to represent the booked site
		newlyBooked = append(newlyBooked, CampsiteAvailability{
			Provider:     providerName,
			CampgroundID: campgroundID,
			CampsiteID:   campsiteID,
			Date:         date,
			Available:    false,
			LastChecked:  time.Now(),
		})
	}

	return currentAvailable, newlyAvailable, newlyBooked, nil
}

// InsertNotificationsBatch inserts notifications as a batch with shared batch ID
func (s *Store) InsertNotificationsBatch(ctx context.Context, notifications []Notification, batchID string) error {
	if len(notifications) == 0 {
		return nil
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO notifications(
			batch_id, request_id, user_id, provider, campground_id, 
			campsite_id, date, state, state_change_id, sent_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, n := range notifications {
		_, err := stmt.ExecContext(ctx,
			batchID, n.RequestID, n.UserID, n.Provider, n.CampgroundID,
			n.CampsiteID, n.Date, n.State, n.StateChangeID, n.SentAt,
		)
		if err != nil {
			return err
		}
	}

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
					_, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "available")
					if err != nil {
						return nil, err
					}
					newly[userID] = append(newly[userID], AvailabilityItem{CampsiteID: st.CampsiteID, Date: normalizeDay(st.Date)})
				}
			} else { // unavailable
				if hadOpen { // close it
					_, err := stInsert.ExecContext(ctx, reqID, userID, provider, campgroundID, st.CampsiteID, normalizeDay(st.Date), "unavailable")
					if err != nil {
						return nil, err
					}
				}
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}
	return newly, nil
}

// GetUnnotifiedStateChanges gets state changes that haven't been notified for specific requests
func (s *Store) GetUnnotifiedStateChanges(ctx context.Context, requests []SchniffRequest) ([]StateChangeForRequest, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	// Build query to get state changes for each request that haven't been notified to that specific request
	var allResults []StateChangeForRequest

	for _, req := range requests {
		query := `
			SELECT sc.id, sc.provider, sc.campground_id, sc.campsite_id, 
			       sc.date, sc.new_available, sc.changed_at, ? as request_id
			FROM state_changes sc
			WHERE sc.provider = ? 
			  AND sc.campground_id = ? 
			  AND sc.date >= ? 
			  AND sc.date < ?
			  AND NOT EXISTS (
				SELECT 1 FROM notifications n 
				WHERE n.state_change_id = sc.id 
				  AND n.request_id = ?
			  )
			ORDER BY sc.changed_at ASC`

		args := []interface{}{
			req.ID, req.Provider, req.CampgroundID, req.Checkin, req.Checkout, req.ID,
		}

		rows, err := s.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var sc StateChangeForRequest
			err := rows.Scan(&sc.ID, &sc.Provider, &sc.CampgroundID, &sc.CampsiteID,
				&sc.Date, &sc.NewAvailable, &sc.ChangedAt, &sc.RequestID)
			if err != nil {
				rows.Close()
				return nil, err
			}
			allResults = append(allResults, sc)
		}
		rows.Close()
	}

	return allResults, nil
} // GetCurrentlyAvailableCampsites gets all currently available campsites in a date range
func (s *Store) GetCurrentlyAvailableCampsites(ctx context.Context, provider, campgroundID string, startDate, endDate time.Time) ([]AvailabilityItem, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT campsite_id, date
		FROM campsite_availability 
		WHERE provider=? AND campground_id=? AND date >= ? AND date < ? AND available=1
		ORDER BY date, campsite_id
	`, provider, campgroundID, startDate, endDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []AvailabilityItem
	for rows.Next() {
		var item AvailabilityItem
		err := rows.Scan(&item.CampsiteID, &item.Date)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
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

func (s *Store) CountLookupsSinceTime(ctx context.Context, provider, campgroundID string, since time.Time) (int64, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT coalesce(count(*),0)
		FROM lookup_log
		WHERE provider=? AND campground_id=? AND datetime(checked_at) >= datetime(?)
	`, provider, campgroundID, since)
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
		err := rows.Scan(&a.Date, &a.Total, &a.Free)
		if err != nil {
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
	err := row.Scan(&available)
	switch err {
	case nil:
		return available, true, nil
	case sql.ErrNoRows:
		return false, false, nil
	default:
		return false, false, err
	}
}

// Metadata

func (s *Store) UpsertCampground(ctx context.Context, provider, id, name string, lat, lon, rating float64, amenities []string, imageURL string, priceMin, priceMax float64, priceUnit string) error {
	amenitiesJSON, _ := json.Marshal(amenities)
	_, err := s.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO campgrounds(provider, campground_id, name, latitude, longitude, rating, amenities, image_url, price_min, price_max, price_unit, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, provider, id, name, lat, lon, rating, string(amenitiesJSON), imageURL, priceMin, priceMax, priceUnit, time.Now())
	return err
}

// UpsertCampsiteMetadataBatch inserts all campsite metadata in a batch
func (s *Store) UpsertCampsiteMetadataBatch(ctx context.Context, provider string, campgroundID string, metadata []providers.CampsiteInfo) error {
	if len(metadata) == 0 {
		return nil
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now()

	// Clear existing equipment entries for this campground to avoid duplicates
	_, err = tx.ExecContext(ctx, `
		DELETE FROM campsite_equipment
		WHERE provider = ? AND campground_id = ?
	`, provider, campgroundID)
	if err != nil {
		return err
	}

	// Prepare statements for efficiency
	metadataStmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO campsite_metadata(provider, campground_id, campsite_id, name, campsite_type, cost_per_night, rating, last_updated, image_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer metadataStmt.Close()

	equipmentStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO campsite_equipment(provider, campground_id, campsite_id, equipment_type, created_at)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer equipmentStmt.Close()

	// Process all metadata in batch
	for _, m := range metadata {
		_, err := metadataStmt.ExecContext(ctx, provider, campgroundID, m.ID, m.Name, m.Type, m.CostPerNight, m.Rating, now, m.PreviewImageURL)
		if err != nil {
			return err
		}

		// Insert equipment types for this campsite
		for _, equipmentType := range m.Equipment {
			_, err = equipmentStmt.ExecContext(ctx, provider, campgroundID, m.ID, equipmentType, now)
			if err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// UpdateCampgroundBasedOnCampsites updates a campground with provided campsite types and equipment arrays, plus max and min cost
func (s *Store) UpdateCampgroundBasedOnCampsites(ctx context.Context, provider, campgroundID string, campsiteTypes, equipment []string, minPrice, maxPrice float64) error {
	// Marshal to JSON
	campsiteTypesJSON, _ := json.Marshal(campsiteTypes)
	equipmentJSON, _ := json.Marshal(equipment)

	// Update the campground with aggregated data
	_, err := s.DB.ExecContext(ctx, `
		UPDATE campgrounds 
		SET campsite_types = ?, equipment = ?, last_updated = ?, price_min = ?, price_max = ?
		WHERE provider = ? AND campground_id = ?
	`, string(campsiteTypesJSON), string(equipmentJSON), time.Now(), minPrice, maxPrice, provider, campgroundID)

	return err
}

// GetCampsiteEquipmentTypes returns all unique equipment types available at a campground
func (s *Store) GetCampsiteEquipmentTypes(ctx context.Context, provider, campgroundID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT equipment_type 
		FROM campsite_equipment 
		WHERE provider = ? AND campground_id = ?
		ORDER BY equipment_type
	`, provider, campgroundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var equipmentTypes []string
	for rows.Next() {
		var equipmentType string
		if err := rows.Scan(&equipmentType); err != nil {
			return nil, err
		}
		equipmentTypes = append(equipmentTypes, equipmentType)
	}
	return equipmentTypes, rows.Err()
}

// GetCampsiteTypes returns all unique campsite types available at a campground
func (s *Store) GetCampsiteTypes(ctx context.Context, provider, campgroundID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT campsite_type 
		FROM campsite_metadata 
		WHERE provider = ? AND campground_id = ? AND campsite_type != ''
		ORDER BY campsite_type
	`, provider, campgroundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var campsiteTypes []string
	for rows.Next() {
		var campsiteType string
		if err := rows.Scan(&campsiteType); err != nil {
			return nil, err
		}
		campsiteTypes = append(campsiteTypes, campsiteType)
	}
	return campsiteTypes, rows.Err()
}

type Campground struct {
	Provider    string
	ID          string
	Name        string
	Lat         float64
	Lon         float64
	Rating      float64
	Amenities   []string
	LastUpdated time.Time
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
		SELECT provider, campground_id, name, coalesce(latitude, 0.0), coalesce(longitude, 0.0)
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
		err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetAllCampgrounds returns all campgrounds without any limit
func (s *Store) GetAllCampgrounds(ctx context.Context) ([]Campground, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT provider, campground_id, name, coalesce(latitude, 0.0), coalesce(longitude, 0.0)
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
		err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCampgroundByID(ctx context.Context, provider, campgroundID string) (Campground, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT provider, campground_id, name, coalesce(latitude, 0.0), coalesce(longitude, 0.0)
		FROM campgrounds
		WHERE provider=? AND campground_id=?
	`, provider, campgroundID)
	var c Campground
	err := row.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon)
	if err != nil {
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
	err := row.Scan(&t)
	switch err {
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
	err := row.Scan(&notifications24h, &lookups24h, &activeRequests)
	if err != nil {
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
		err := rows.Scan(&userID)
		if err != nil {
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
		err := rows.Scan(&userID)
		if err != nil {
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
		err := rows.Scan(&name)
		if err != nil {
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

		err = json.Unmarshal([]byte(campgroundsJSON), &group.Campgrounds)
		if err != nil {
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

	err = json.Unmarshal([]byte(campgroundsJSON), &group.Campgrounds)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal campgrounds for group %d: %w", group.ID, err)
	}

	return &group, nil
}

// GetCampgroundsByProvider retrieves all campgrounds for a specific provider
func (s *Store) GetCampgroundsByProvider(ctx context.Context, provider string) ([]Campground, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT provider, campground_id, name, latitude, longitude, rating, amenities, last_updated
		FROM campgrounds 
		WHERE provider = ?
		ORDER BY name
	`, provider)
	if err != nil {
		return nil, fmt.Errorf("failed to query campgrounds by provider: %w", err)
	}
	defer rows.Close()

	var campgrounds []Campground
	for rows.Next() {
		var c Campground
		var amenitiesJSON string
		err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon, &c.Rating, &amenitiesJSON, &c.LastUpdated)
		if err != nil {
			return nil, fmt.Errorf("failed to scan campground: %w", err)
		}

		// Parse amenities JSON
		if amenitiesJSON != "" {
			err = json.Unmarshal([]byte(amenitiesJSON), &c.Amenities)
			if err != nil {
				return nil, fmt.Errorf("failed to unmarshal amenities for campground %s: %w", c.ID, err)
			}
		}

		campgrounds = append(campgrounds, c)
	}

	return campgrounds, rows.Err()
}

// GetCampsiteTypesForCampground retrieves distinct campsite types for a specific campground
func (s *Store) GetCampsiteTypesForCampground(ctx context.Context, provider, campgroundID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT campsite_type
		FROM campsite_metadata 
		WHERE provider = ? AND campground_id = ? AND campsite_type != ''
		ORDER BY campsite_type
	`, provider, campgroundID)
	if err != nil {
		// Handle database locked errors gracefully
		if strings.Contains(err.Error(), "database is locked") {
			return []string{}, nil // Return empty slice instead of error
		}
		return nil, fmt.Errorf("failed to query campsite types: %w", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var campsiteType string
		err := rows.Scan(&campsiteType)
		if err != nil {
			return nil, fmt.Errorf("failed to scan campsite type: %w", err)
		}
		types = append(types, campsiteType)
	}

	return types, rows.Err()
}

// RefreshCampgroundTypes rebuilds the campground_types table from campsite_metadata
func (s *Store) RefreshCampgroundTypes(ctx context.Context) error {
	// Clear existing data
	_, err := s.DB.ExecContext(ctx, "DELETE FROM campground_types")
	if err != nil {
		return fmt.Errorf("failed to clear campground_types: %w", err)
	}

	// Populate with current campsite type data
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO campground_types (provider, campground_id, campsite_type)
		SELECT DISTINCT provider, campground_id, campsite_type
		FROM campsite_metadata 
		WHERE campsite_type != '' AND campsite_type IS NOT NULL
	`)
	if err != nil {
		return fmt.Errorf("failed to populate campground_types: %w", err)
	}

	return nil
}

// getCampgroundTypesBatch processes a small batch of campground keys
func (s *Store) getCampgroundTypesBatch(ctx context.Context, campgroundKeys []string) (map[string][]string, error) {
	if len(campgroundKeys) == 0 {
		return make(map[string][]string), nil
	}

	// Parse keys into provider/campground_id pairs for index-friendly query
	var conditions []string
	var args []interface{}

	for _, key := range campgroundKeys {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) == 2 {
			conditions = append(conditions, "(provider = ? AND campground_id = ?)")
			args = append(args, parts[0], parts[1])
		}
	}

	if len(conditions) == 0 {
		return make(map[string][]string), nil
	}

	query := fmt.Sprintf(`
		SELECT provider, campground_id, campsite_type
		FROM campground_types 
		WHERE %s
		ORDER BY provider, campground_id, campsite_type
	`, strings.Join(conditions, " OR "))

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query campground types: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var provider, campgroundID, campsiteType string
		err := rows.Scan(&provider, &campgroundID, &campsiteType)
		if err != nil {
			return nil, fmt.Errorf("failed to scan campground type: %w", err)
		}
		key := provider + ":" + campgroundID
		result[key] = append(result[key], campsiteType)
	}

	return result, rows.Err()
}

// CampsiteDetails holds detailed information about a campsite
type CampsiteDetails struct {
	CampsiteID   string
	Name         string
	Type         string
	CostPerNight float64
	Rating       float64
	Equipment    []string
	ImageURL     string
}

// GetCampsiteDetails retrieves detailed information for a specific campsite
func (s *Store) GetCampsiteDetails(ctx context.Context, provider, campgroundID, campsiteID string) (CampsiteDetails, error) {
	// Get campsite metadata
	row := s.DB.QueryRowContext(ctx, `
		SELECT campsite_id, coalesce(name, ''), coalesce(campsite_type, ''), 
		       coalesce(cost_per_night, 0.0), coalesce(rating, 0.0), coalesce(image_url, '')
		FROM campsite_metadata
		WHERE provider=? AND campground_id=? AND campsite_id=?
	`, provider, campgroundID, campsiteID)

	var details CampsiteDetails
	err := row.Scan(&details.CampsiteID, &details.Name, &details.Type,
		&details.CostPerNight, &details.Rating, &details.ImageURL)
	if err != nil {
		if err == sql.ErrNoRows {
			// If no metadata found, return basic info
			details.CampsiteID = campsiteID
			details.Name = ""
			details.Type = ""
		} else {
			// Log error but don't fail notification
			details.CampsiteID = campsiteID
		}
	}

	// Get equipment types for this campsite
	equipmentRows, err := s.DB.QueryContext(ctx, `
		SELECT equipment_type
		FROM campsite_equipment
		WHERE provider=? AND campground_id=? AND campsite_id=?
		ORDER BY equipment_type
	`, provider, campgroundID, campsiteID)

	if err == nil {
		defer equipmentRows.Close()
		var equipment []string
		for equipmentRows.Next() {
			var equipType string
			if err := equipmentRows.Scan(&equipType); err == nil {
				equipment = append(equipment, equipType)
			}
		}
		details.Equipment = equipment
	}

	return details, nil
}

// GetCampsiteDetailsBatch retrieves detailed information for multiple campsites efficiently
func (s *Store) GetCampsiteDetailsBatch(ctx context.Context, provider, campgroundID string, campsiteIDs []string) (map[string]CampsiteDetails, error) {
	if len(campsiteIDs) == 0 {
		return make(map[string]CampsiteDetails), nil
	}

	result := make(map[string]CampsiteDetails)

	// Initialize with basic info for all requested campsites
	for _, id := range campsiteIDs {
		result[id] = CampsiteDetails{
			CampsiteID: id,
			Equipment:  []string{},
		}
	}

	// Build placeholders for IN clause
	placeholders := make([]string, len(campsiteIDs))
	args := []interface{}{provider, campgroundID}
	for i, id := range campsiteIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	// Get metadata for all campsites
	metadataQuery := fmt.Sprintf(`
		SELECT campsite_id, coalesce(name, ''), coalesce(campsite_type, ''), 
		       coalesce(cost_per_night, 0.0), coalesce(rating, 0.0), coalesce(image_url, '')
		FROM campsite_metadata
		WHERE provider=? AND campground_id=? AND campsite_id IN (%s)
	`, strings.Join(placeholders, ","))

	metadataRows, err := s.DB.QueryContext(ctx, metadataQuery, args...)
	if err == nil {
		defer metadataRows.Close()
		for metadataRows.Next() {
			var campsiteID, name, campsiteType, imageURL string
			var costPerNight, rating float64
			if err := metadataRows.Scan(&campsiteID, &name, &campsiteType, &costPerNight, &rating, &imageURL); err == nil {
				if details, exists := result[campsiteID]; exists {
					details.Name = name
					details.Type = campsiteType
					details.CostPerNight = costPerNight
					details.Rating = rating
					details.ImageURL = imageURL
					result[campsiteID] = details
				}
			}
		}
	}

	// Get equipment for all campsites
	equipmentQuery := fmt.Sprintf(`
		SELECT campsite_id, equipment_type
		FROM campsite_equipment
		WHERE provider=? AND campground_id=? AND campsite_id IN (%s)
		ORDER BY campsite_id, equipment_type
	`, strings.Join(placeholders, ","))

	equipmentRows, err := s.DB.QueryContext(ctx, equipmentQuery, args...)
	if err == nil {
		defer equipmentRows.Close()
		for equipmentRows.Next() {
			var campsiteID, equipType string
			if err := equipmentRows.Scan(&campsiteID, &equipType); err == nil {
				if details, exists := result[campsiteID]; exists {
					details.Equipment = append(details.Equipment, equipType)
					result[campsiteID] = details
				}
			}
		}
	}

	return result, nil
}
