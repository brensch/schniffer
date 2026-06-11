package manager

import (
	"context"
	"log/slog"
	"time"
)

// warmTabReconcileInterval is how often we resync the warm-tab set against
// the active-schniff set. Tabs open for new schniffs, close for deactivated
// ones, and refresh if the campground's chosen parking campsite changes.
//
// 30s is short enough to bring a freshly-created auto-book schniff to a
// warm state within one cycle, and long enough that the per-tab Nav (~1s)
// doesn't constantly recompete with normal poll-cycle DB / chromedp work.
const warmTabReconcileInterval = 30 * time.Second

// runWarmTabReconciler keeps one warm Chrome tab open per active auto-book
// schniff request. Each tab is parked on the schniff's campground overview
// page (/camping/campgrounds/{id}), where grecaptcha enterprise is loaded
// — empirically that page accepts hold POSTs for any campsite in the
// campground, so one tab serves every candidate site without re-navigating.
//
// When a schniff deactivates, its tab is closed.
//
// Idempotent + crash-safe: every cycle re-derives desired state from the DB
// and applies it. No in-memory state to drift.
func (m *Manager) runWarmTabReconciler(ctx context.Context) {
	// Tick immediately on startup so freshly-deployed bots warm up the
	// existing schniff set without waiting a full interval.
	if err := m.reconcileWarmTabsOnce(ctx); err != nil {
		m.logger.Warn("warm tab reconcile: initial run failed", slog.Any("err", err))
	}
	t := time.NewTicker(warmTabReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.reconcileWarmTabsOnce(ctx); err != nil {
				m.logger.Warn("warm tab reconcile failed", slog.Any("err", err))
			}
		}
	}
}

func (m *Manager) reconcileWarmTabsOnce(ctx context.Context) error {
	requests, err := m.store.ListActiveRequests(ctx)
	if err != nil {
		return err
	}

	// desiredByUser maps userID → requestID → campground_id we want parked.
	// We only consider requests whose owner has a warm pool session; the
	// rest aren't auto-book candidates.
	type want struct {
		requestID    int64
		campgroundID string
	}
	desiredByUser := map[string][]want{}
	for _, r := range requests {
		if !r.Active {
			continue
		}
		if !m.pool.HasUser(r.UserID) {
			continue
		}
		desiredByUser[r.UserID] = append(desiredByUser[r.UserID], want{r.ID, r.CampgroundID})
	}

	// Apply desired set per user; close any extra tabs.
	for userID, wants := range desiredByUser {
		desiredIDs := make(map[int64]struct{}, len(wants))
		for _, w := range wants {
			desiredIDs[w.requestID] = struct{}{}
			ectx, ecancel := context.WithTimeout(ctx, 90*time.Second)
			err := m.pool.EnsureWarmTabForRequest(ectx, userID, w.requestID, w.campgroundID)
			ecancel()
			if err != nil {
				m.logger.Warn("ensure warm tab failed",
					slog.String("user_id", userID),
					slog.Int64("request_id", w.requestID),
					slog.String("campground_id", w.campgroundID),
					slog.Any("err", err))
				continue
			}
		}
		// Close any tabs for requests that are no longer active.
		for _, existingID := range m.pool.ListRequestTabs(userID) {
			if _, ok := desiredIDs[existingID]; ok {
				continue
			}
			m.pool.CloseRequestTab(userID, existingID)
			m.logger.Info("closed warm tab for deactivated request",
				slog.String("user_id", userID),
				slog.Int64("request_id", existingID))
		}
	}

	// Users with no desired tabs at all: close everything they still hold.
	for _, r := range requests {
		if _, seen := desiredByUser[r.UserID]; seen {
			continue
		}
		if !m.pool.HasUser(r.UserID) {
			continue
		}
		for _, existingID := range m.pool.ListRequestTabs(r.UserID) {
			m.pool.CloseRequestTab(r.UserID, existingID)
			m.logger.Info("closed warm tab (no active auto-book requests)",
				slog.String("user_id", r.UserID),
				slog.Int64("request_id", existingID))
		}
	}

	return nil
}
