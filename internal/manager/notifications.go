package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/google/uuid"
)

// processNotificationsWithBatches handles the new batch-based notification system
func (m *Manager) ProcessNotificationsWithBatches(ctx context.Context, requests []db.SchniffRequest) error {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return nil
	}

	// Group requests by provider/campground
	reqsByPC := make(map[string][]db.SchniffRequest)
	for _, req := range requests {
		if !req.Active {
			continue
		}
		key := fmt.Sprintf("%s_%s", req.Provider, req.CampgroundID)
		reqsByPC[key] = append(reqsByPC[key], req)
	}

	// Generate a batch ID for this notification round
	batchID := uuid.New().String()

	// Process each provider/campground group
	for _, reqs := range reqsByPC {
		if len(reqs) == 0 {
			continue
		}

		// Use the first request to get provider/campground info
		firstReq := reqs[0]

		// Collect all campground IDs and date ranges for this group
		campgroundIDs := []string{firstReq.CampgroundID}
		startDate := firstReq.Checkin
		endDate := firstReq.Checkout

		for _, req := range reqs[1:] {
			if req.Checkin.Before(startDate) {
				startDate = req.Checkin
			}
			if req.Checkout.After(endDate) {
				endDate = req.Checkout
			}
		}

		// Get availability changes using DB-only comparison
		allAvailable, newlyAvailable, newlyBooked, err := m.store.GetAvailabilityChangesForNotifications(
			ctx, firstReq.Provider, campgroundIDs, startDate, endDate)
		if err != nil {
			m.logger.Warn("get availability changes failed",
				slog.String("provider", firstReq.Provider),
				slog.String("campground", firstReq.CampgroundID),
				slog.Any("err", err))
			continue
		}

		// Process notifications for each user request
		var notificationsToRecord []db.Notification
		now := time.Now()

		for _, req := range reqs {
			// Filter availability data for this specific request's date range
			var reqAvailable, reqNewlyAvailable, reqNewlyBooked []db.CampsiteAvailability

			for _, ca := range allAvailable {
				if !ca.Date.Before(req.Checkin) && ca.Date.Before(req.Checkout) {
					reqAvailable = append(reqAvailable, ca)
				}
			}
			for _, ca := range newlyAvailable {
				if !ca.Date.Before(req.Checkin) && ca.Date.Before(req.Checkout) {
					reqNewlyAvailable = append(reqNewlyAvailable, ca)
				}
			}
			for _, ca := range newlyBooked {
				if !ca.Date.Before(req.Checkin) && ca.Date.Before(req.Checkout) {
					reqNewlyBooked = append(reqNewlyBooked, ca)
				}
			} // Only send notification if there are changes or available sites
			if len(reqAvailable) > 0 || len(reqNewlyBooked) > 0 {
				// Send notification to user
				if err := m.sendBatchNotification(ctx, req, reqAvailable, reqNewlyAvailable, reqNewlyBooked); err != nil {
					m.logger.Warn("send batch notification failed",
						slog.String("userID", req.UserID),
						slog.Any("err", err))
				}

				// Record notifications for batch tracking
				for _, ca := range reqAvailable {
					notificationsToRecord = append(notificationsToRecord, db.Notification{
						RequestID:    req.ID,
						UserID:       req.UserID,
						Provider:     ca.Provider,
						CampgroundID: ca.CampgroundID,
						CampsiteID:   ca.CampsiteID,
						Date:         ca.Date,
						State:        "available",
						SentAt:       now,
					})
				}
			}
		}

		// Record the notification batch
		if len(notificationsToRecord) > 0 {
			if err := m.store.InsertNotificationsBatch(ctx, notificationsToRecord, batchID); err != nil {
				m.logger.Warn("record notification batch failed", slog.Any("err", err))
			} else {
				m.logger.Info("recorded notification batch",
					slog.String("batchID", batchID),
					slog.Int("count", len(notificationsToRecord)))
			}
		}
	}

	return nil
}

// sendBatchNotification sends a notification with all available sites + highlights new ones + shows booked ones
func (m *Manager) sendBatchNotification(ctx context.Context, req db.SchniffRequest, available, newlyAvailable, newlyBooked []db.CampsiteAvailability) error {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return fmt.Errorf("no notifier available")
	}

	// Convert to AvailabilityItem format expected by notifier
	var availableItems []db.AvailabilityItem

	// Convert available sites to items
	for _, ca := range available {
		availableItems = append(availableItems, db.AvailabilityItem{
			CampsiteID: ca.CampsiteID,
			Date:       ca.Date,
		})
	}

	// Send the notification
	if err := n.NotifyAvailabilityEmbed(req.UserID, req.Provider, req.CampgroundID, req, availableItems); err != nil {
		return fmt.Errorf("embed notification failed: %w", err)
	}

	return nil
}

// Legacy methods for backward compatibility

// processStateChangesForNotifications handles the new state change-based notification system
func (m *Manager) processStateChangesForNotifications(ctx context.Context, stateChanges []db.StateChange, reqs []db.SchniffRequest) {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return
	}

	// Process state changes and generate notification data
	notifications, err := m.store.ProcessStateChangesForNotifications(ctx, stateChanges, reqs)
	if err != nil {
		m.logger.Warn("process state changes for notifications failed", slog.Any("err", err))
		return
	}

	if len(notifications) == 0 {
		return
	}

	// Send notifications
	for _, notif := range notifications {
		if len(notif.NewlyAvailable) == 0 && len(notif.NewlyUnavailable) == 0 {
			continue // no changes to notify about
		}

		// Find the request for context
		var userReq *db.SchniffRequest
		for _, req := range reqs {
			if req.ID == notif.RequestID {
				userReq = &req
				break
			}
		}
		if userReq == nil {
			continue
		}

		// Send the enhanced notification
		if err := m.sendEnhancedNotification(ctx, notif, *userReq); err != nil {
			m.logger.Warn("send enhanced notification failed",
				slog.String("userID", notif.UserID),
				slog.Any("err", err))
		}

		// Record notifications in the database
		now := time.Now()
		for _, item := range notif.NewlyAvailable {
			if err := m.store.RecordNotification(ctx, db.Notification{
				RequestID:    notif.RequestID,
				UserID:       notif.UserID,
				Provider:     notif.Provider,
				CampgroundID: notif.CampgroundID,
				CampsiteID:   item.CampsiteID,
				Date:         item.Date,
				State:        "available",
				SentAt:       now,
			}); err != nil {
				m.logger.Warn("record available notification failed", slog.Any("err", err))
			}
		}

		for _, item := range notif.NewlyUnavailable {
			if err := m.store.RecordNotification(ctx, db.Notification{
				RequestID:    notif.RequestID,
				UserID:       notif.UserID,
				Provider:     notif.Provider,
				CampgroundID: notif.CampgroundID,
				CampsiteID:   item.CampsiteID,
				Date:         item.Date,
				State:        "unavailable",
				SentAt:       now,
			}); err != nil {
				m.logger.Warn("record unavailable notification failed", slog.Any("err", err))
			}
		}
	}
}

// sendEnhancedNotification sends notifications with both newly available/unavailable and still available info
func (m *Manager) sendEnhancedNotification(ctx context.Context, notif db.NotificationData, req db.SchniffRequest) error {
	m.mu.Lock()
	n := m.notifier
	m.mu.Unlock()
	if n == nil {
		return fmt.Errorf("no notifier available")
	}

	// Create comprehensive availability data for the notification
	// This includes newly available, newly unavailable, and still available
	allItems := append(notif.NewlyAvailable, notif.StillAvailable...)

	// Send the embed notification with enhanced data
	// The notifier interface may need to be extended to handle the newly unavailable items
	if err := n.NotifyAvailabilityEmbed(notif.UserID, notif.Provider, notif.CampgroundID, req, allItems); err != nil {
		return fmt.Errorf("embed notification failed: %w", err)
	}

	return nil
}
