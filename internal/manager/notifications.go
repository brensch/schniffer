package manager

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
)

// detectChangesAndNotify handles change detection and notification for a specific provider+campground
func (m *Manager) detectChangesAndNotify(ctx context.Context, reqs []db.SchniffRequest, states []providers.Campsite, prov, cg string) {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return
	}

	// Convert provider states to db-friendly incoming states
	incoming := make([]db.IncomingCampsiteState, 0, len(states))
	for _, s := range states {
		incoming = append(incoming, db.IncomingCampsiteState{CampsiteID: s.ID, Date: s.Date, Available: s.Available})
	}

	// Ask DB to reconcile notifications and return per-user newly opened availability items
	newlyOpenByUser, err := m.store.ReconcileNotifications(ctx, prov, cg, reqs, incoming)
	if err != nil {
		m.logger.Warn("reconcile notifications failed", slog.Any("err", err))
		return
	}
	if len(newlyOpenByUser) == 0 {
		return
	}

	// Send formatted notifications per user
	for userID, items := range newlyOpenByUser {
		if len(items) == 0 {
			continue
		}

		// Get the first request to determine the date range
		var userReq *db.SchniffRequest
		for _, req := range reqs {
			if req.UserID == userID {
				userReq = &req
				break
			}
		}
		if userReq == nil {
			continue
		}

		// Format and send the notification
		if err := m.sendFormattedNotification(ctx, userID, prov, cg, *userReq, items); err != nil {
			m.logger.Warn("send formatted notification failed", slog.String("userID", userID), slog.Any("err", err))
		}
	}
}

// sendFormattedNotification creates and sends a beautifully formatted notification
func (m *Manager) sendFormattedNotification(ctx context.Context, userID, provider, campgroundID string, req db.SchniffRequest, items []db.AvailabilityItem) error {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return fmt.Errorf("no notifier available")
	}

	// Send the embed notification only
	if err := n.NotifyAvailabilityEmbed(userID, provider, campgroundID, req, items); err != nil {
		fmt.Println("embed notification failed:", err)
		return fmt.Errorf("embed notification failed: %w", err)
	}

	return nil
}
