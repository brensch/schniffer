package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/robfig/cron/v3"
	"golang.org/x/time/rate"
)

// SyncCampgrounds pulls all campgrounds from a provider and stores them in DB.
func (m *Manager) SyncCampgrounds(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
	}

	started := time.Now()

	// Use the provider's FetchAllCampgrounds method directly - it now handles all the amenities extraction
	all, err := prov.FetchAllCampgrounds(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, cg := range all {
		err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon, cg.Rating, cg.Amenities, cg.ImageURL)
		if err != nil {
			return count, err
		}
		count++
	}
	err = m.store.RecordMetadataSync(ctx,
		db.MetadataSyncLog{
			SyncType:     db.MetadataSyncTypeAllCampgrounds,
			Provider:     providerName,
			CampgroundID: nil,
			StartedAt:    started,
			FinishedAt:   time.Now(),
			Count:        count,
		})
	if err != nil {
		m.logger.Warn("record sync failed", slog.Any("err", err))
	}
	m.logger.Info("campground sync completed",
		slog.String("provider", providerName),
		slog.Int("total_campgrounds", count),
		slog.Duration("duration", time.Since(started)),
	)
	return count, nil
}

// SyncCampsites pulls all campsite metadata from a provider and stores them in DB.
func (m *Manager) SyncCampsites(ctx context.Context, providerName string) (int, error) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return 0, fmt.Errorf("unknown provider: %s", providerName)
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

	for _, campground := range campgrounds {
		if ctx.Err() != nil {
			m.logger.Warn("context canceled", slog.String("provider", providerName), slog.String("campground", campground.ID))
			return processed, ctx.Err()
		}
		processed++

		// Check if this specific campground was recently synced
		// we allow iterating through them all in case a full campsite sync got halfway through
		last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, db.MetadataSyncTypeCampgroundMetadata, providerName, &campground.ID)
		if err == nil && ok {
			if time.Since(last) < 24*time.Hour {
				skipped++
				continue
			}
		} else if err != nil {
			m.logger.Warn("get last campground sync failed", slog.Any("err", err))
		}

		// Rate limit the request
		err = rateLimiter.Wait(ctx)
		if err != nil {
			m.logger.Warn("rate limiter context canceled", slog.Any("err", err))
			return processed, err
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
		campgroundID := campground.ID
		err = m.store.UpsertCampsiteMetadataBatch(ctx, providerName, campgroundID, campsiteInfos)
		if err != nil {
			m.logger.Warn("failed to store campsite metadata",
				slog.String("provider", providerName),
				slog.String("campground", campground.ID),
				slog.Any("err", err))
			return processed, fmt.Errorf("failed to store campsite metadata: %w", err)
		}

		// // get max min price from campsites
		// var minPrice, maxPrice float64
		// for _, campsite := range campsiteInfos {
		// 	if campsite.CostPerNight < minPrice || minPrice == 0 {
		// 		minPrice = campsite.CostPerNight
		// 	}
		// 	if campsite.CostPerNight > maxPrice {
		// 		maxPrice = campsite.CostPerNight
		// 	}
		// }

		// note not doing this because relation databases rule
		// // Update campground with aggregated campsite types and equipment
		// err = m.store.UpdateCampgroundBasedOnCampsites(ctx, providerName, campground.ID, campsiteTypes, equipment, minPrice, maxPrice)
		// if err != nil {
		// 	m.logger.Warn("failed to update campground with campsite data",
		// 		slog.String("provider", providerName),
		// 		slog.String("campground", campground.ID),
		// 		slog.Any("err", err))
		// 	// Don't skip - this is not critical
		// }

		// Record successful sync for this campground
		if err := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
			SyncType:     db.MetadataSyncTypeCampgroundMetadata,
			Provider:     providerName,
			CampgroundID: &campgroundID,
			StartedAt:    time.Now(),
			FinishedAt:   time.Now(),
			Count:        len(campsiteInfos),
		}); err != nil {
			m.logger.Warn("record campground sync failed", slog.Any("err", err))
		}

		count++
	}

	// Record overall sync completion
	err = m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
		SyncType:     db.MetadataSyncTypeAllCampsites,
		Provider:     providerName,
		CampgroundID: nil, // NULL for provider-level sync
		StartedAt:    started,
		FinishedAt:   time.Now(),
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

const (
	metadataSyncCron = "0 4 1 * *" // 4am on 1st of the month
)

// RunCampgroundSync runs periodic campground syncs in the background using robfig/cron.
func (m *Manager) RunCampgroundSync(ctx context.Context, provider string) {

	// if the metadata sync hasn't ever been run, run it once now.
	_, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, db.MetadataSyncTypeAllCampgrounds, provider, nil)
	if err != nil {
		m.logger.Error("failed to get last successful metadata sync", slog.String("provider", provider), slog.Any("err", err))
		return
	}
	if !ok {
		// If no successful sync exists, run a full sync
		m.logger.Info("no successful campground metadata sync found, running full sync", slog.String("provider", provider))
		campgroundCount, err := m.SyncCampgrounds(ctx, provider)
		if err != nil {
			m.notifier.ChannelMessageSend(m.GetSummaryChannel(), fmt.Sprintf("⚠️ %s campground sync failed: %s", provider, err))
			// don't return because we should still attempt
		}

		slog.Error("retrieved first ever metadata for campground", slog.String("provider", provider), slog.Int("campgrounds", campgroundCount))
	}

	// if the campsite sync hasn't ever been run, run it now.
	_, ok, err = m.store.GetLastSuccessfulMetadataSync(ctx, db.MetadataSyncTypeAllCampsites, provider, nil)
	if err != nil {
		m.logger.Error("failed to get last successful metadata sync", slog.String("provider", provider), slog.Any("err", err))
		return
	}
	if !ok {
		// If no successful sync exists, run a full sync
		m.logger.Info("no successful campsite metadata sync found, running full sync", slog.String("provider", provider))
		campsiteCount, err := m.SyncCampsites(ctx, provider)
		if err != nil {
			m.notifier.ChannelMessageSend(m.GetSummaryChannel(), fmt.Sprintf("⚠️ %s campsite sync failed: %s", provider, err))
			// don't return because we should still attempt
		}

		slog.Error("retrieved first ever metadata for campsites", slog.String("provider", provider), slog.Int("campsites", campsiteCount))
	}

	// now that the first ever sync has been run, start up the cronjob
	cronRunner := cron.New()
	_, err = cronRunner.AddFunc(metadataSyncCron, func() {
		m.logger.Info("running scheduled metadata sync", slog.String("provider", provider))
		_, err := m.SyncCampgrounds(ctx, provider)
		if err != nil {
			m.logger.Error("scheduled metadata sync failed", slog.String("provider", provider), slog.Any("err", err))
		}
		_, err = m.SyncCampsites(ctx, provider)
		if err != nil {
			m.logger.Error("scheduled metadata sync failed", slog.String("provider", provider), slog.Any("err", err))
		}
		m.logger.Info("scheduled metadata sync completed", slog.String("provider", provider))
	})
	if err != nil {
		m.logger.Error("failed to add metadata sync cron job", slog.String("provider", provider), slog.Any("err", err))
	}

}
