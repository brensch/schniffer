package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/google/uuid"
)

// processNotificationsWithBatches handles the new state-change-based notification system
func (m *Manager) ProcessNotificationsWithBatches(ctx context.Context, requests []db.SchniffRequest) error {
	m.logger.Info("processing notifications", slog.Int("request_count", len(requests)))

	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		m.logger.Warn("no notifier available")
		return nil
	}

	// Get unnotified state changes for all requests
	stateChanges, err := m.store.GetUnnotifiedStateChanges(ctx, requests)
	if err != nil {
		m.logger.Warn("get unnotified state changes failed", slog.Any("err", err))
		return err
	}

	m.logger.Info("found unnotified state changes", slog.Int("count", len(stateChanges)))

	if len(stateChanges) == 0 {
		return nil // No new state changes to notify about
	}

	// Group state changes by request for processing
	changesByRequest := make(map[int64][]db.StateChangeForRequest)
	for _, change := range stateChanges {
		changesByRequest[change.RequestID] = append(changesByRequest[change.RequestID], change)
	}

	m.logger.Info("grouped state changes by request", slog.Int("requests", len(changesByRequest)))

	// Generate a batch ID for this notification round
	batchID := uuid.New().String()
	var notificationsToRecord []db.Notification
	now := time.Now()

	// Process each request separately
	for requestID, changes := range changesByRequest {
		// Find the request details
		var req db.SchniffRequest
		found := false
		for _, r := range requests {
			if r.ID == requestID {
				req = r
				found = true
				break
			}
		}
		if !found {
			m.logger.Warn("request not found for state changes", slog.Int64("requestID", requestID))
			continue
		}

		m.logger.Info("processing request",
			slog.Int64("requestID", requestID),
			slog.String("provider", req.Provider),
			slog.String("campgroundID", req.CampgroundID),
			slog.Int("changes", len(changes)))

		// Get currently available campsites for context
		allAvailable, err := m.store.GetCurrentlyAvailableCampsites(ctx, req.Provider, req.CampgroundID, req.Checkin, req.Checkout)
		if err != nil {
			m.logger.Warn("get currently available campsites failed", slog.Any("err", err))
			continue
		}

		// Separate newly available vs newly booked from the state changes
		var newlyAvailable, newlyBooked []db.AvailabilityItem
		for _, change := range changes {
			item := db.AvailabilityItem{
				CampsiteID: change.CampsiteID,
				Date:       change.Date,
			}

			if change.NewAvailable {
				newlyAvailable = append(newlyAvailable, item)
			} else {
				newlyBooked = append(newlyBooked, item)
			}
		}

		// Send notification to user
		err = m.sendStateChangeNotification(ctx, req, allAvailable, newlyAvailable, newlyBooked)
		if err != nil {
			m.logger.Warn("send state change notification failed",
				slog.String("userID", req.UserID),
				slog.Any("err", err))
		}

		// Record notifications for each relevant state change
		for _, change := range changes {
			state := "available"
			if !change.NewAvailable {
				state = "unavailable"
			}

			notificationsToRecord = append(notificationsToRecord, db.Notification{
				RequestID:     req.ID,
				UserID:        req.UserID,
				Provider:      change.Provider,
				CampgroundID:  change.CampgroundID,
				CampsiteID:    change.CampsiteID,
				Date:          change.Date,
				State:         state,
				StateChangeID: &change.ID,
				SentAt:        now,
			})
		}
	}

	// Record all notifications in batch
	if len(notificationsToRecord) > 0 {
		err := m.store.InsertNotificationsBatch(ctx, notificationsToRecord, batchID)
		if err != nil {
			m.logger.Warn("record notification batch failed", slog.Any("err", err))
		} else {
			m.logger.Info("recorded state change notification batch",
				slog.String("batchID", batchID),
				slog.Int("count", len(notificationsToRecord)))
		}
	}

	return nil
}

// sendStateChangeNotification sends a notification with all available sites + highlights state changes
func (m *Manager) sendStateChangeNotification(ctx context.Context, req db.SchniffRequest, available, newlyAvailable, newlyBooked []db.AvailabilityItem) error {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return fmt.Errorf("no notifier available")
	}

	// Only send notification if there are state changes
	if len(newlyAvailable) == 0 && len(newlyBooked) == 0 {
		return nil
	}

	// Send the notification with state change information
	err := n.NotifyAvailabilityEmbed(req.UserID, req.Provider, req.CampgroundID, req, available, newlyAvailable, newlyBooked)
	if err != nil {
		return fmt.Errorf("embed notification failed: %w", err)
	}

	return nil
}
