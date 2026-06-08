package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type UserCredential struct {
	UserID         string
	Provider       string
	Email          string
	PasswordCT     []byte
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DisabledAt     *time.Time
	DisabledReason *string
}

type Booking struct {
	ID           int64
	BatchID      string
	UserID       string
	Provider     string
	CampgroundID string
	CampsiteID   string
	Checkin      time.Time
	Checkout     time.Time
	Outcome      string
	OrderID      *string
	ErrorMsg     *string
	AttemptedAt  time.Time
}

const (
	BookingOutcomeHeld    = "held"
	BookingOutcomeFailed  = "failed"
	BookingOutcomeSkipped = "skipped"
)

func (s *Store) UpsertUserCredential(ctx context.Context, userID, provider, email string, passwordCT []byte) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO user_credentials(user_id, provider, email, password_ct, created_at, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(user_id) DO UPDATE SET
			provider=excluded.provider,
			email=excluded.email,
			password_ct=excluded.password_ct,
			updated_at=datetime('now'),
			disabled_at=NULL,
			disabled_reason=NULL
	`, userID, provider, email, passwordCT)
	return err
}

func (s *Store) GetUserCredential(ctx context.Context, userID string) (*UserCredential, error) {
	row := s.ReadConnection().QueryRowContext(ctx, `
		SELECT user_id, provider, email, password_ct, created_at, updated_at, disabled_at, disabled_reason
		FROM user_credentials WHERE user_id=?
	`, userID)
	var c UserCredential
	err := row.Scan(&c.UserID, &c.Provider, &c.Email, &c.PasswordCT, &c.CreatedAt, &c.UpdatedAt, &c.DisabledAt, &c.DisabledReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListActiveUserCredentials returns all non-disabled credentials. Used on bot
// startup to warm the browser pool.
func (s *Store) ListActiveUserCredentials(ctx context.Context) ([]UserCredential, error) {
	rows, err := s.ReadConnection().QueryContext(ctx, `
		SELECT user_id, provider, email, password_ct, created_at, updated_at, disabled_at, disabled_reason
		FROM user_credentials WHERE disabled_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserCredential
	for rows.Next() {
		var c UserCredential
		if err := rows.Scan(&c.UserID, &c.Provider, &c.Email, &c.PasswordCT, &c.CreatedAt, &c.UpdatedAt, &c.DisabledAt, &c.DisabledReason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DisableUserCredential(ctx context.Context, userID, reason string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE user_credentials SET disabled_at=datetime('now'), disabled_reason=? WHERE user_id=?
	`, reason, userID)
	return err
}

func (s *Store) DeleteUserCredential(ctx context.Context, userID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM user_credentials WHERE user_id=?`, userID)
	return err
}

func (s *Store) InsertBooking(ctx context.Context, b Booking) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO bookings(batch_id, user_id, provider, campground_id, campsite_id, checkin, checkout, outcome, order_id, error_msg, attempted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`, b.BatchID, b.UserID, b.Provider, b.CampgroundID, b.CampsiteID, b.Checkin, b.Checkout, b.Outcome, b.OrderID, b.ErrorMsg)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentHoldOwner describes the user whose held booking on a given campsite
// is the most recent within the lookup window. Used to detect cart-hold
// expiries: when a campsite flips back to available shortly after we held
// it, the original holder gets a "basket expired" follow-up DM instead of
// another auto-book attempt for them.
type RecentHoldOwner struct {
	CampsiteID  string
	UserID      string
	Checkin     time.Time
	Checkout    time.Time
	AttemptedAt time.Time
}

// RecentHoldOwnersForCampground returns the most recent held booking per
// campsite within the given campground that was attempted after `since`.
// One row per campsite (latest wins on ties). Callers intersect the held
// window with the dates that just became available.
func (s *Store) RecentHoldOwnersForCampground(ctx context.Context, provider, campgroundID string, since time.Time) ([]RecentHoldOwner, error) {
	rows, err := s.ReadConnection().QueryContext(ctx, `
		SELECT campsite_id, user_id, checkin, checkout, attempted_at
		FROM bookings
		WHERE provider=? AND campground_id=?
		  AND outcome='held'
		  AND attempted_at > ?
		ORDER BY campsite_id, attempted_at DESC
	`, provider, campgroundID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentHoldOwner
	seen := map[string]bool{}
	for rows.Next() {
		var r RecentHoldOwner
		if err := rows.Scan(&r.CampsiteID, &r.UserID, &r.Checkin, &r.Checkout, &r.AttemptedAt); err != nil {
			return nil, err
		}
		if seen[r.CampsiteID] {
			continue
		}
		seen[r.CampsiteID] = true
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListBookingsByBatch(ctx context.Context, batchID string) ([]Booking, error) {
	rows, err := s.ReadConnection().QueryContext(ctx, `
		SELECT id, batch_id, user_id, provider, campground_id, campsite_id, checkin, checkout, outcome, order_id, error_msg, attempted_at
		FROM bookings WHERE batch_id=? ORDER BY attempted_at
	`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Booking
	for rows.Next() {
		var b Booking
		if err := rows.Scan(&b.ID, &b.BatchID, &b.UserID, &b.Provider, &b.CampgroundID, &b.CampsiteID, &b.Checkin, &b.Checkout, &b.Outcome, &b.OrderID, &b.ErrorMsg, &b.AttemptedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
