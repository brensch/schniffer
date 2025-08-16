package manager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

	// Group state changes by provider/campground for batched notifications
	changesByPC := make(map[string][]db.StateChange)
	for _, change := range stateChanges {
		key := fmt.Sprintf("%s|%s", change.Provider, change.CampgroundID)
		changesByPC[key] = append(changesByPC[key], change)
	}

	m.logger.Info("grouped state changes", slog.Int("groups", len(changesByPC)))

	// Generate a batch ID for this notification round
	batchID := uuid.New().String()

	// Process each provider/campground group
	for key, changes := range changesByPC {
		m.logger.Info("processing group", slog.String("key", key), slog.Int("changes", len(changes)))

		parts := strings.Split(key, "|")
		m.logger.Info("split parts", slog.Any("parts", parts), slog.Int("count", len(parts)))
		if len(parts) != 2 {
			m.logger.Warn("invalid group key", slog.String("key", key), slog.Any("parts", parts))
			continue
		}
		provider, campgroundID := parts[0], parts[1]
		m.logger.Info("parsed group", slog.String("provider", provider), slog.String("campgroundID", campgroundID))

		// Find relevant requests for this provider/campground
		var relevantRequests []db.SchniffRequest
		for _, req := range requests {
			if req.Provider == provider && req.CampgroundID == campgroundID && req.Active {
				relevantRequests = append(relevantRequests, req)
			}
		}

		if len(relevantRequests) == 0 {
			continue
		}

		// Get date range from all relevant requests
		var minDate, maxDate time.Time
		for i, req := range relevantRequests {
			if i == 0 {
				minDate, maxDate = req.Checkin, req.Checkout
			} else {
				if req.Checkin.Before(minDate) {
					minDate = req.Checkin
				}
				if req.Checkout.After(maxDate) {
					maxDate = req.Checkout
				}
			}
		}

		// Get currently available campsites for context
		allAvailable, err := m.store.GetCurrentlyAvailableCampsites(ctx, provider, campgroundID, minDate, maxDate)
		if err != nil {
			m.logger.Warn("get currently available campsites failed", slog.Any("err", err))
			continue
		}

		// Process notifications for each user request
		var notificationsToRecord []db.Notification
		now := time.Now()

		for _, req := range relevantRequests {
			// Filter changes that are relevant to this request's date range
			var relevantChanges []db.StateChange
			var newlyAvailable, newlyBooked []db.AvailabilityItem

			for _, change := range changes {
				if !change.Date.Before(req.Checkin) && change.Date.Before(req.Checkout) {
					relevantChanges = append(relevantChanges, change)

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
			}

			if len(relevantChanges) == 0 {
				continue // No relevant changes for this request
			}

			// Filter currently available sites to this request's date range
			var reqAvailable []db.AvailabilityItem
			for _, item := range allAvailable {
				if !item.Date.Before(req.Checkin) && item.Date.Before(req.Checkout) {
					reqAvailable = append(reqAvailable, item)
				}
			}

			// Send notification to user
			err := m.sendStateChangeNotification(ctx, req, reqAvailable, newlyAvailable, newlyBooked)
			if err != nil {
				m.logger.Warn("send state change notification failed",
					slog.String("userID", req.UserID),
					slog.Any("err", err))
			}

			// Record notifications for each relevant state change
			for _, change := range relevantChanges {
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

		// Record the notification batch
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
