package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"golang.org/x/time/rate"
)

// SyncCampgrounds pulls all campgrounds from a provider and stores them in DB.
func (m *Manager) SyncCampgrounds(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}
	// Check last successful sync within 24h
	if last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, "campgrounds", providerName); err == nil && ok {
		if time.Since(last) < 24*time.Hour {
			m.logger.Info("skip campground sync; recently synced", slog.String("provider", providerName), slog.Time("last", last))
			return 0, nil
		}
	} else if err != nil {
		m.logger.Warn("get last sync failed", slog.Any("err", err))
	}
	started := time.Now()

	// Use the provider's FetchAllCampgrounds method directly - it now handles all the amenities extraction
	all, err := prov.FetchAllCampgrounds(ctx)
	if err != nil {
		// store failed sync
		if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: 0}); err2 != nil {
			m.logger.Warn("record sync failed", slog.Any("err", err2))
		}
		return 0, err
	}
	count := 0
	for _, cg := range all {
		err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon, cg.Rating, cg.Amenities, cg.ImageURL, cg.PriceMin, cg.PriceMax, cg.PriceUnit)
		if err != nil {
			// store failed sync
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: false, ErrorMsg: err.Error(), Count: count}); err2 != nil {
				m.logger.Warn("record sync failed", slog.Any("err", err2))
			}
			return count, err
		}
		count++
	}
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{SyncType: "campgrounds", Provider: providerName, CampgroundID: nil, StartedAt: started, FinishedAt: time.Now(), Success: true, Count: count})
	if err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}
	return count, nil
}

// SyncCampsites pulls all campsite metadata from a provider and stores them in DB.
func (m *Manager) SyncCampsites(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}

	// Check last successful sync within 24h
	if last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, "campsites", providerName); err == nil && ok {
		if time.Since(last) < 24*time.Hour {
			m.logger.Info("skip campsite sync; recently synced", slog.String("provider", providerName), slog.Time("last", last))
			return 0, nil
		}
	} else if err != nil {
		m.logger.Warn("get last campsite sync failed", slog.Any("err", err))
	}

	started := time.Now()

	// Get all campgrounds from the database to sync campsites for
	campgrounds, err := m.store.GetCampgroundsByProvider(ctx, providerName)
	if err != nil {
		m.logger.Warn("failed to get campgrounds", slog.String("provider", providerName), slog.Any("err", err))
		return 0, fmt.Errorf("failed to get campgrounds: %w", err)
	}

	if len(campgrounds) == 0 {
		m.logger.Info("no campgrounds found for provider", slog.String("provider", providerName))
		return 0, nil
	}

	// Create rate limiter: 1 request every 2 seconds with burst of 5
	rateLimiter := rate.NewLimiter(rate.Every(2*time.Second), 5)

	count := 0
	processed := 0
	skipped := 0
	totalCampgrounds := len(campgrounds)

	m.logger.Info("starting campsite sync",
		slog.String("provider", providerName),
		slog.Int("total_campgrounds", totalCampgrounds),
		slog.String("estimated_duration", fmt.Sprintf("~%.1f hours", float64(totalCampgrounds)*2.1/3600))) // 2s rate limit + 0.1s delay

	for i, campground := range campgrounds {
		if ctx.Err() != nil {
			m.logger.Warn("context canceled", slog.String("provider", providerName), slog.String("campground", campground.ID))
			return processed, ctx.Err()
		}
		processed++

		// Log progress every 100 campgrounds
		if processed%100 == 0 {
			m.logger.Info("campsite sync progress",
				slog.String("provider", providerName),
				slog.Int("processed", processed),
				slog.Int("total", totalCampgrounds),
				slog.Int("successful", count),
				slog.Int("skipped", skipped),
				slog.Float64("percent", float64(processed)/float64(totalCampgrounds)*100))
		}

		// Check if this specific campground was recently synced (within 1 hour)
		if last, ok, err := m.store.GetLastSuccessfulCampgroundSync(ctx, "campsites", providerName, campground.ID); err == nil && ok {
			if time.Since(last) < 1*time.Hour {
				skipped++
				continue
			}
		} else if err != nil {
			m.logger.Warn("get last campground sync failed", slog.Any("err", err))
		}

		// Rate limit the request
		if err := rateLimiter.Wait(ctx); err != nil {
			m.logger.Warn("rate limiter context canceled", slog.Any("err", err))
			return processed, err
		}

		// Add a small delay between requests to be extra respectful to the API
		if i > 0 {
			select {
			case <-ctx.Done():
				return processed, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}

		// Fetch campsite metadata for this campground
		campsiteInfos, err := prov.FetchCampsites(ctx, campground.ID)
		if err != nil {
			m.logger.Warn("failed to fetch campsite metadata",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))

			msg := fmt.Sprintf("⚠️ %s error while syncing campsite metadata for campground %s. Slowing down requests.", providerName, campground.ID)
			_, err = m.notifier.ChannelMessageSend(m.GetSummaryChannel(), msg)
			if err != nil {
				m.logger.Warn("failed to send rate limit notification", slog.Any("err", err))
			}

			select {
			case <-ctx.Done():
				return processed, ctx.Err()
			case <-time.After(1 * time.Minute):
			}

		}

		// Store each campsite metadata
		err = m.store.UpsertCampsiteMetadataBatch(ctx, providerName, campground.ID, campsiteInfos)
		if err != nil {
			m.logger.Warn("failed to store campsite metadata",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))

			// Record failed sync for this campground
			campgroundIDPtr := &campground.ID
			if err2 := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
				SyncType:     "campsites",
				Provider:     providerName,
				CampgroundID: campgroundIDPtr,
				StartedAt:    time.Now(),
				FinishedAt:   time.Now(),
				Success:      false,
				ErrorMsg:     err.Error(),
				Count:        0,
			}); err2 != nil {
				m.logger.Warn("record campground sync failed", slog.Any("err", err2))
			}
			continue
		}

		// Extract unique campsite types and equipment from the fetched data
		campsiteTypesSet := make(map[string]struct{})
		equipmentSet := make(map[string]struct{})

		for _, campsite := range campsiteInfos {
			if campsite.Type != "" {
				campsiteTypesSet[campsite.Type] = struct{}{}
			}
			for _, eq := range campsite.Equipment {
				if eq != "" {
					equipmentSet[eq] = struct{}{}
				}
			}
		}

		// Convert sets to slices
		var campsiteTypes []string
		for t := range campsiteTypesSet {
			campsiteTypes = append(campsiteTypes, t)
		}

		var equipment []string
		for e := range equipmentSet {
			equipment = append(equipment, e)
		}

		// get max min price from campsites
		var minPrice, maxPrice float64
		for _, campsite := range campsiteInfos {
			if campsite.CostPerNight < minPrice || minPrice == 0 {
				minPrice = campsite.CostPerNight
			}
			if campsite.CostPerNight > maxPrice {
				maxPrice = campsite.CostPerNight
			}
		}

		// Update campground with aggregated campsite types and equipment
		err = m.store.UpdateCampgroundBasedOnCampsites(ctx, providerName, campground.ID, campsiteTypes, equipment, minPrice, maxPrice)
		if err != nil {
			m.logger.Warn("failed to update campground with campsite data",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))
			// Don't skip - this is not critical
		}

		// Record successful sync for this campground
		campgroundIDPtr := &campground.ID
		if err := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
			SyncType:     "campsites",
			Provider:     providerName,
			CampgroundID: campgroundIDPtr,
			StartedAt:    time.Now(),
			FinishedAt:   time.Now(),
			Success:      true,
			Count:        len(campsiteInfos),
		}); err != nil {
			m.logger.Warn("record campground sync failed", slog.Any("err", err))
		}

		count++
	}

	// Record overall sync completion
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
		SyncType:     "campsites",
		Provider:     providerName,
		CampgroundID: nil, // NULL for provider-level sync
		StartedAt:    started,
		FinishedAt:   time.Now(),
		Success:      true,
		Count:        count,
	})
	if err != nil {
		m.logger.Warn("record campsite sync failed", slog.Any("err", err))
	}

	m.logger.Info("campsite sync completed",
		slog.String("provider", providerName),
		slog.Int("campgrounds_processed", processed),
		slog.Int("campsites_synced", count))

	return count, nil
}

// RunCampgroundSync runs periodic campground syncs in the background.
func (m *Manager) RunCampgroundSync(ctx context.Context, provider string, interval time.Duration) {
	doSync := func() {
		// First sync campgrounds
		n, err := m.SyncCampgrounds(ctx, provider)
		if err != nil {
			m.logger.Error("campground sync failed", slog.String("provider", provider), slog.Any("err", err))
			return
		}
		m.logger.Info("campground sync completed", slog.String("provider", provider), slog.Int("count", n))

		// Then sync campsites
		campsiteCount, err := m.SyncCampsites(ctx, provider)
		if err != nil {
			m.logger.Error("campsite sync failed", slog.String("provider", provider), slog.Any("err", err))
			return
		}
		m.logger.Info("campsite sync completed", slog.String("provider", provider), slog.Int("count", campsiteCount))
	}
	doSync()
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			doSync()
		}
	}
}
