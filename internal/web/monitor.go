package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/brensch/schniffer/internal/monitor"
	"github.com/brensch/schniffer/internal/proxypool"
)

const monitorCookie = "monitor_session"

// nameCacheEntry caches a resolved Discord display name so the 2s snapshot
// loop doesn't hit Discord for every user every tick.
type nameCacheEntry struct {
	name    string
	expires time.Time
}

// displayName resolves a Discord display name via the manager, cached for
// 5 minutes.
func (s *Server) displayName(userID string) string {
	s.nameMu.Lock()
	if e, ok := s.nameCn[userID]; ok && time.Now().Before(e.expires) {
		s.nameMu.Unlock()
		return e.name
	}
	s.nameMu.Unlock()

	name := s.mgr.DisplayName(userID)
	s.nameMu.Lock()
	s.nameCn[userID] = nameCacheEntry{name: name, expires: time.Now().Add(5 * time.Minute)}
	s.nameMu.Unlock()
	return name
}

// authed reports whether the request carries a valid monitor session cookie.
func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(monitorCookie)
	if err != nil {
		return false
	}
	return s.monAuth.ValidSession(c.Value)
}

// handleMonitorPage redeems a one-time token (?token=…) into a session
// cookie, then serves the dashboard. Without a token it requires an
// existing session; anything unauthenticated gets a 403 and no content.
func (s *Server) handleMonitorPage(w http.ResponseWriter, r *http.Request) {
	if token := r.URL.Query().Get("token"); token != "" {
		sid, ok := s.monAuth.Redeem(token)
		if !ok {
			slog.Warn("monitor: token redemption failed", slog.String("ua", r.UserAgent()))
			http.Error(w, "This dashboard link is invalid or has expired. Run /schniff dashboard for a fresh one.", http.StatusForbidden)
			return
		}
		slog.Info("monitor: session issued", slog.String("ua", r.UserAgent()))
		http.SetCookie(w, &http.Cookie{
			Name:     monitorCookie,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(monitor.SessionTTL / time.Second),
		})
		// Redirect to a clean URL so the single-use token doesn't linger in
		// history, referrers, or bookmarks.
		http.Redirect(w, r, "/monitor", http.StatusSeeOther)
		return
	}

	if !s.authed(r) {
		http.Error(w, "This dashboard is private. Run /schniff dashboard in Discord to get an access link.", http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, "./static/monitor.html")
}

// handleMonitorStream streams a dashboard snapshot as Server-Sent Events,
// refreshing every 2s. Requires a valid session.
func (s *Server) handleMonitorStream(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	send := func() bool {
		snap := s.buildSnapshot(r.Context())
		payload, err := json.Marshal(snap)
		if err != nil {
			slog.Warn("monitor snapshot marshal failed", slog.Any("err", err))
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false // client gone
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

// ---- snapshot model ----

type monSnapshot struct {
	TS        string                          `json:"ts"`
	Uptime    string                          `json:"uptime"`
	Stats     monStats                        `json:"stats"`
	Providers []monProvider                   `json:"providers"`
	IPHealth  map[string][]proxypool.IPHealth `json:"ipHealth"`
	Users     []monUser                       `json:"users"`
	Checks    []monCheck                      `json:"recentChecks"`
	Errors    []monError                      `json:"recentErrors"`
	Hits      []monHit                        `json:"recentHits"`
}

type monStats struct {
	ActiveSchniffs int `json:"activeSchniffs"`
	Checks5m       int `json:"checks5m"`
	Hits24h        int `json:"hits24h"`
	Campgrounds    int `json:"campgrounds"`
}

type monProvider struct {
	Name    string  `json:"name"`
	OK      int64   `json:"ok"`
	Failed  int64   `json:"failed"`
	Total   int64   `json:"total"`
	PctFail float64 `json:"pctFail"`
}

// monUser groups one user's active schniffs for the dashboard.
type monUser struct {
	User        string       `json:"user"`
	Count       int          `json:"count"`
	TotalNights int          `json:"totalNights"`
	Schniffs    []monSchniff `json:"schniffs"`
}

type monSchniff struct {
	Provider   string `json:"provider"`
	Campground string `json:"campground"`
	Checkin    string `json:"checkin"`
	Checkout   string `json:"checkout"`
	Nights     int    `json:"nights"`
	Strategy   string `json:"strategy"`
}

type monCheck struct {
	Ago        string `json:"ago"`
	Provider   string `json:"provider"`
	Campground string `json:"campground"`
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
}

type monError struct {
	Ago        string `json:"ago"`
	Provider   string `json:"provider"`
	Campground string `json:"campground"`
	Error      string `json:"error"`
}

type monHit struct {
	Ago        string `json:"ago"`
	User       string `json:"user"`
	Campground string `json:"campground"`
	Site       string `json:"site"`
	Date       string `json:"date"`
}

func (s *Server) buildSnapshot(ctx context.Context) monSnapshot {
	snap := monSnapshot{
		TS:     time.Now().Format(time.RFC3339),
		Uptime: humanSince(s.monStart),
	}
	snap.Providers = s.providerStats()
	if s.monPool != nil {
		snap.IPHealth = s.monPool.HealthByTarget()
	}
	snap.Stats = s.monTopStats(ctx)
	snap.Users = s.activeSchniffsByUser(ctx)
	snap.Checks = s.recentChecks(ctx)
	snap.Errors = s.recentErrors(ctx)
	snap.Hits = s.recentHits(ctx)
	return snap
}

func (s *Server) providerStats() []monProvider {
	if s.monPool == nil {
		return nil
	}
	stats, _ := s.monPool.Snapshot()
	byTarget := map[string]*monProvider{}
	for _, st := range stats {
		p := byTarget[st.Target]
		if p == nil {
			p = &monProvider{Name: st.Target}
			byTarget[st.Target] = p
		}
		p.Total += st.Requests
		p.Failed += st.Failed
	}
	out := make([]monProvider, 0, len(byTarget))
	for _, p := range byTarget {
		p.OK = p.Total - p.Failed
		p.PctFail = pctOf(p.Failed, p.Total)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pctOf(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func (s *Server) monTopStats(ctx context.Context) monStats {
	var st monStats
	db := s.store.ReadDB
	if db == nil {
		db = s.store.DB
	}
	db.QueryRowContext(ctx, `SELECT count(*) FROM schniff_requests WHERE active=1`).Scan(&st.ActiveSchniffs)
	db.QueryRowContext(ctx, `SELECT count(*) FROM lookup_log WHERE checked_at >= datetime('now','-5 minutes')`).Scan(&st.Checks5m)
	db.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE state='available' AND sent_at >= datetime('now','-1 day')`).Scan(&st.Hits24h)
	db.QueryRowContext(ctx, `SELECT count(DISTINCT provider||campground_id) FROM schniff_requests WHERE active=1`).Scan(&st.Campgrounds)
	return st
}

// activeSchniffsByUser returns active schniffs grouped per user, each user's
// schniffs sorted by check-in. Users are ordered by schniff count desc.
func (s *Server) activeSchniffsByUser(ctx context.Context) []monUser {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT sr.user_id, sr.provider, COALESCE(c.name, sr.campground_id),
		       sr.checkin, sr.checkout, COALESCE(sr.strategy,'')
		FROM schniff_requests sr
		LEFT JOIN campgrounds c ON c.provider=sr.provider AND c.campground_id=sr.campground_id
		WHERE sr.active=1
		ORDER BY sr.checkin ASC, sr.created_at DESC`)
	if err != nil {
		slog.Warn("monitor: active schniffs query failed", slog.Any("err", err))
		return nil
	}
	defer rows.Close()

	byUser := map[string]*monUser{}
	var order []string
	for rows.Next() {
		var uid, prov, cg, checkin, checkout, strat string
		if err := rows.Scan(&uid, &prov, &cg, &checkin, &checkout, &strat); err != nil {
			continue
		}
		u := byUser[uid]
		if u == nil {
			u = &monUser{User: s.displayName(uid)}
			byUser[uid] = u
			order = append(order, uid)
		}
		nights := nightsBetween(checkin, checkout)
		u.Schniffs = append(u.Schniffs, monSchniff{
			Provider:   prov,
			Campground: cg,
			Checkin:    shortDate(checkin),
			Checkout:   shortDate(checkout),
			Nights:     nights,
			Strategy:   strat,
		})
		u.Count++
		u.TotalNights += nights
	}

	out := make([]monUser, 0, len(order))
	for _, uid := range order {
		out = append(out, *byUser[uid])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].User < out[j].User
	})
	return out
}

// recentErrors returns the most recent failed checks with their error text.
func (s *Server) recentErrors(ctx context.Context) []monError {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT l.checked_at, l.provider, COALESCE(c.name, l.campground_id),
		       COALESCE(l.error_msg,'')
		FROM lookup_log l
		LEFT JOIN campgrounds c ON c.provider=l.provider AND c.campground_id=l.campground_id
		WHERE l.success=0
		ORDER BY l.checked_at DESC
		LIMIT 15`)
	if err != nil {
		slog.Warn("monitor: recent errors query failed", slog.Any("err", err))
		return nil
	}
	defer rows.Close()
	var out []monError
	for rows.Next() {
		var ts, prov, cg, errMsg string
		if err := rows.Scan(&ts, &prov, &cg, &errMsg); err != nil {
			continue
		}
		if errMsg == "" {
			errMsg = "(no error message recorded)"
		}
		out = append(out, monError{
			Ago:        agoOf(ts),
			Provider:   prov,
			Campground: cg,
			Error:      truncate(errMsg, 300),
		})
	}
	return out
}

// nightsBetween returns the number of nights between two schniff dates.
func nightsBetween(checkin, checkout string) int {
	ci, co := parseSQLiteTime(checkin), parseSQLiteTime(checkout)
	if ci.IsZero() || co.IsZero() {
		return 0
	}
	n := int(co.Sub(ci).Hours()/24 + 0.5)
	if n < 0 {
		return 0
	}
	return n
}

func (s *Server) recentChecks(ctx context.Context) []monCheck {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT l.checked_at, l.provider, COALESCE(c.name, l.campground_id),
		       l.success, COALESCE(l.error_msg,'')
		FROM lookup_log l
		LEFT JOIN campgrounds c ON c.provider=l.provider AND c.campground_id=l.campground_id
		ORDER BY l.checked_at DESC
		LIMIT 20`)
	if err != nil {
		slog.Warn("monitor: recent checks query failed", slog.Any("err", err))
		return nil
	}
	defer rows.Close()
	var out []monCheck
	for rows.Next() {
		var ts, prov, cg, errMsg string
		var ok bool
		if err := rows.Scan(&ts, &prov, &cg, &ok, &errMsg); err != nil {
			continue
		}
		out = append(out, monCheck{
			Ago:        agoOf(ts),
			Provider:   prov,
			Campground: cg,
			OK:         ok,
			Error:      truncate(errMsg, 80),
		})
	}
	return out
}

func (s *Server) recentHits(ctx context.Context) []monHit {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT n.sent_at, n.user_id, COALESCE(c.name, n.campground_id), n.campsite_id, n.date
		FROM notifications n
		LEFT JOIN campgrounds c ON c.provider=n.provider AND c.campground_id=n.campground_id
		WHERE n.state='available'
		ORDER BY n.sent_at DESC
		LIMIT 12`)
	if err != nil {
		slog.Warn("monitor: recent hits query failed", slog.Any("err", err))
		return nil
	}
	defer rows.Close()
	var out []monHit
	for rows.Next() {
		var ts, uid, cg, site, date string
		if err := rows.Scan(&ts, &uid, &cg, &site, &date); err != nil {
			continue
		}
		out = append(out, monHit{
			Ago:        agoOf(ts),
			User:       s.displayName(uid),
			Campground: cg,
			Site:       site,
			Date:       shortDate(date),
		})
	}
	return out
}

// ---- small formatting helpers ----

func humanSince(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanDuration(time.Since(t))
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int((d % (24 * time.Hour)) / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// agoOf renders a sqlite timestamp (UTC "2006-01-02 15:04:05") as a compact
// relative time like "3s", "5m", "2h".
func agoOf(ts string) string {
	t := parseSQLiteTime(ts)
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

func parseSQLiteTime(ts string) time.Time {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func shortDate(ts string) string {
	t := parseSQLiteTime(ts)
	if t.IsZero() {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ts
	}
	return t.Format("Jan 2")
}
