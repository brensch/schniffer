package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

//go:embed schema.sql
var schemaFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("%s?access_mode=READ_WRITE", path)
	db, err := sql.Open("duckdb", dsn)
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
	StartDate    time.Time
	EndDate      time.Time
	CreatedAt    time.Time
	Active       bool
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
		INSERT INTO schniff_requests(user_id, provider, campground_id, start_date, end_date, created_at, active)
		VALUES (?, ?, ?, ?, ?, now(), true)
		RETURNING id
	`, r.UserID, r.Provider, r.CampgroundID, r.StartDate, r.EndDate)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) ListActiveRequests(ctx context.Context) ([]SchniffRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, user_id, provider, campground_id, start_date, end_date, created_at, active
		FROM schniff_requests WHERE active=true
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SchniffRequest
	for rows.Next() {
		var r SchniffRequest
		if err := rows.Scan(&r.ID, &r.UserID, &r.Provider, &r.CampgroundID, &r.StartDate, &r.EndDate, &r.CreatedAt, &r.Active); err != nil {
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
		INSERT INTO lookup_log(provider, campground_id, month, checked_at, success, err)
		VALUES (?, ?, ?, ?, ?, ?)
	`, l.Provider, l.CampgroundID, l.Month, l.CheckedAt, l.Success, l.Err)
	return err
}

func (s *Store) RecordNotification(ctx context.Context, n Notification) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO notifications(request_id, user_id, provider, campground_id, campsite_id, date, state, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, n.RequestID, n.UserID, n.Provider, n.CampgroundID, n.CampsiteID, n.Date, n.State, n.SentAt)
	return err
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

func (s *Store) UpsertCampground(ctx context.Context, provider, campgroundID, name, parentName, parentID string, lat, lon float64) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO campgrounds(provider, campground_id, name, parent_name, parent_id, lat, lon)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, provider, campgroundID, name, parentName, parentID, lat, lon)
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
	Provider     string
	CampgroundID string
	Name         string
	ParentName   string
	ParentID     string
	Lat          float64
	Lon          float64
}

func (s *Store) ListCampgrounds(ctx context.Context, like string) ([]Campground, error) {
	// Fuzzy search across both name and parent_name with simple ranking.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT provider, campground_id, name, coalesce(parent_name, '') AS parent_name, coalesce(parent_id, ''), coalesce(lat, 0.0), coalesce(lon, 0.0)
		FROM campgrounds
		WHERE lower(name) LIKE '%' || lower(?) || '%' OR lower(coalesce(parent_name, '')) LIKE '%' || lower(?) || '%'
		ORDER BY
			CASE
				WHEN lower(name) = lower(?) OR lower(coalesce(parent_name, '')) = lower(?) THEN 0
				WHEN lower(name) LIKE lower(?) || '%' OR lower(coalesce(parent_name, '')) LIKE lower(?) || '%' THEN 1
				WHEN lower(name) LIKE '%' || lower(?) || '%' OR lower(coalesce(parent_name, '')) LIKE '%' || lower(?) || '%' THEN 2
				ELSE 3
			END,
			name
		LIMIT 25
	`, like, like, like, like, like, like, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Campground
	for rows.Next() {
		var c Campground
		if err := rows.Scan(&c.Provider, &c.CampgroundID, &c.Name, &c.ParentName, &c.ParentID, &c.Lat, &c.Lon); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCampgroundByID(ctx context.Context, provider, campgroundID string) (Campground, bool, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT provider, campground_id, name, coalesce(parent_name, ''), coalesce(parent_id, ''), coalesce(lat, 0.0), coalesce(lon, 0.0)
		FROM campgrounds
		WHERE provider=? AND campground_id=?
	`, provider, campgroundID)
	var c Campground
	if err := row.Scan(&c.Provider, &c.CampgroundID, &c.Name, &c.ParentName, &c.ParentID, &c.Lat, &c.Lon); err != nil {
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
