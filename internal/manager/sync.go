package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/brensch/schniffer/internal/proxypool"
)

// Metadata (campground lists, campsite details) is refreshed by a slow
// always-on loop per provider: once an hour it refreshes the campgrounds
// whose metadata is oldest, sized so every campground comes round once per
// metadataCycle. The campground list itself is re-pulled once per cycle.
//
// The loop exists to keep the map current, never at the expense of the
// active schniff polls: it makes at most one retry per request, yields while a
// provider's poll loop is failing, and pauses on any 403/429.
const (
	metadataTickInterval = time.Hour
	metadataStartDelay   = 5 * time.Minute
	metadataCycle        = 30 * 24 * time.Hour

	// metadataRetryAfter is how soon a campground whose refresh failed
	// comes back round, instead of waiting a whole cycle.
	metadataRetryAfter = 24 * time.Hour

	// campsiteDetailMaxAge is how long per-site details are kept before
	// being re-fetched, for providers that pay a request per site
	// (providers.IncrementalCampsiteFetcher). New sites are always fetched.
	campsiteDetailMaxAge = 365 * 24 * time.Hour

	// metadataRateLimitPause is how long a provider's refresh stops after
	// an upstream 403/429.
	metadataRateLimitPause = 6 * time.Hour

	// pollFailureQuiet skips a refresh tick if the provider's active poll
	// loop failed this recently.
	pollFailureQuiet = time.Hour
)

// RunMetadataRefresh starts the background metadata refresh loop for every
// registered provider.
func (m *Manager) RunMetadataRefresh(ctx context.Context) {
	for _, name := range m.reg.GetProviderNames() {
		go m.runMetadataRefresh(ctx, name)
	}
}

func (m *Manager) runMetadataRefresh(ctx context.Context, providerName string) {
	prov, ok := m.reg.Get(providerName)
	if !ok {
		return
	}
	// Tag metadata traffic separately so its 403/429s don't bench proxy
	// IPs for the active polls (proxypool backs off per target).
	ctx = proxypool.WithProvider(ctx, providerName+"_metadata")

	var pausedUntil, lastListAttempt time.Time
	timer := time.NewTimer(metadataStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		switch {
		case time.Now().Before(pausedUntil):
			m.logger.Info("metadata refresh paused after rate limit", slog.String("provider", providerName), slog.Time("until", pausedUntil))
		case m.recentPollFailure(providerName):
			m.logger.Info("metadata refresh skipped; active polls are failing", slog.String("provider", providerName))
		default:
			err := m.refreshMetadataTick(ctx, providerName, prov, &lastListAttempt)
			if errors.Is(err, providers.ErrRateLimited) {
				pausedUntil = time.Now().Add(metadataRateLimitPause)
				m.notifyOps(fmt.Sprintf("⚠️ %s metadata refresh hit a rate limit; pausing it for %v. Err: %v", providerName, metadataRateLimitPause, err))
			} else if err != nil && ctx.Err() == nil {
				m.logger.Warn("metadata refresh tick failed", slog.String("provider", providerName), slog.Any("err", err))
			}
		}
		timer.Reset(metadataTickInterval)
	}
}

// refreshMetadataTick does one hour's share of metadata work: the
// campground list if it's due, then the batch of campgrounds whose campsite
// metadata is oldest. Returns early with ErrRateLimited on a 403/429.
func (m *Manager) refreshMetadataTick(ctx context.Context, providerName string, prov providers.Provider, lastListAttempt *time.Time) error {
	last, ok, err := m.store.GetLastSuccessfulMetadataSync(ctx, db.MetadataSyncTypeAllCampgrounds, providerName, nil)
	if err != nil {
		return fmt.Errorf("get last campground list sync: %w", err)
	}
	// lastListAttempt stops a failing list pull from retrying every tick.
	if (!ok || time.Since(last) > metadataCycle) && time.Since(*lastListAttempt) > metadataRetryAfter {
		*lastListAttempt = time.Now()
		if err := m.refreshCampgroundList(ctx, providerName, prov); err != nil {
			if errors.Is(err, providers.ErrRateLimited) {
				return err
			}
			m.logger.Warn("campground list refresh failed", slog.String("provider", providerName), slog.Any("err", err))
		}
	}

	total, err := m.store.CountListedCampgrounds(ctx, providerName)
	if err != nil {
		return fmt.Errorf("count campgrounds: %w", err)
	}
	ticksPerCycle := int(metadataCycle / metadataTickInterval)
	batch := (total + ticksPerCycle - 1) / ticksPerCycle
	ids, err := m.store.CampgroundsDueForMetadata(ctx, providerName, batch)
	if err != nil {
		return fmt.Errorf("pick campgrounds: %w", err)
	}

	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		checkedAt := time.Now()
		err := m.refreshCampsites(ctx, providerName, prov, id)
		if errors.Is(err, providers.ErrRateLimited) {
			return err
		}
		if err != nil {
			m.logger.Warn("campsite metadata refresh failed", slog.String("provider", providerName), slog.String("campground", id), slog.Any("err", err))
			// Queue position is age, so backdating puts it back round in
			// about metadataRetryAfter rather than a full cycle.
			checkedAt = checkedAt.Add(-metadataCycle + metadataRetryAfter)
		}
		if err := m.store.SetCampgroundMetadataChecked(ctx, providerName, id, checkedAt); err != nil {
			m.logger.Warn("record metadata check failed", slog.String("campground", id), slog.Any("err", err))
		}
	}
	return nil
}

// refreshCampgroundList re-pulls the provider's campground list, upserts
// it, and flags campgrounds the provider no longer lists as removed.
func (m *Manager) refreshCampgroundList(ctx context.Context, providerName string, prov providers.Provider) error {
	started := time.Now()
	all, fetchErr := prov.FetchAllCampgrounds(ctx)
	incomplete := errors.Is(fetchErr, providers.ErrIncomplete)
	if fetchErr != nil && !incomplete {
		return fetchErr
	}
	seen := make([]string, 0, len(all))
	for _, cg := range all {
		if err := m.store.UpsertCampground(ctx, providerName, cg.ID, cg.Name, cg.Lat, cg.Lon, cg.Rating, cg.Amenities, cg.ImageURL, cg.PriceMin, cg.PriceMax, cg.PriceUnit); err != nil {
			return fmt.Errorf("upsert campground %s: %w", cg.ID, err)
		}
		seen = append(seen, cg.ID)
	}

	// A list much shorter than what we have is more likely an upstream
	// hiccup than half the campgrounds closing; don't act on it.
	listed, err := m.store.CountListedCampgrounds(ctx, providerName)
	if err != nil {
		return fmt.Errorf("count campgrounds: %w", err)
	}
	var removed int64
	if incomplete {
		// Keep what we got, but a partial list can't show what's gone.
		m.logger.Warn("campground list incomplete; not marking removals",
			slog.String("provider", providerName), slog.Any("err", fetchErr))
	} else if len(all)*2 >= listed {
		if removed, err = m.store.MarkCampgroundsRemoved(ctx, providerName, seen); err != nil {
			return fmt.Errorf("mark removed campgrounds: %w", err)
		}
	} else {
		m.logger.Warn("campground list suspiciously short; not marking removals",
			slog.String("provider", providerName), slog.Int("fetched", len(all)), slog.Int("listed", listed))
	}

	if err := m.store.RecordMetadataSync(ctx, db.MetadataSyncLog{
		SyncType:   db.MetadataSyncTypeAllCampgrounds,
		Provider:   providerName,
		StartedAt:  started,
		FinishedAt: time.Now(),
		Count:      len(all),
	}); err != nil {
		m.logger.Warn("record campground list sync failed", slog.Any("err", err))
	}
	m.logger.Info("campground list refreshed",
		slog.String("provider", providerName),
		slog.Int("campgrounds", len(all)),
		slog.Int64("removed", removed),
		slog.Duration("duration", time.Since(started)))
	return nil
}

// refreshCampsites re-pulls one campground's campsite metadata, drops sites
// the provider no longer lists, and recomputes the campground's aggregates.
func (m *Manager) refreshCampsites(ctx context.Context, providerName string, prov providers.Provider, campgroundID string) error {
	var fetched []providers.CampsiteInfo
	var seen []string
	if inc, ok := prov.(providers.IncrementalCampsiteFetcher); ok {
		skip, err := m.store.CampsiteIDsUpdatedSince(ctx, providerName, campgroundID, time.Now().Add(-campsiteDetailMaxAge))
		if err != nil {
			return fmt.Errorf("load recent campsites: %w", err)
		}
		if fetched, seen, err = inc.FetchCampsitesSkipping(ctx, campgroundID, skip); err != nil {
			return err
		}
	} else {
		var err error
		if fetched, err = prov.FetchCampsites(ctx, campgroundID); err != nil {
			return err
		}
		for _, c := range fetched {
			seen = append(seen, c.ID)
		}
	}

	// An empty answer is as likely an off-season or upstream quirk as a
	// campground with no sites; leave what we have.
	if len(seen) == 0 {
		return nil
	}

	if err := m.store.UpsertCampsiteMetadataBatch(ctx, providerName, campgroundID, fetched); err != nil {
		return fmt.Errorf("store campsites: %w", err)
	}
	removed, err := m.store.DeleteCampsitesNotIn(ctx, providerName, campgroundID, seen)
	if err != nil {
		return fmt.Errorf("delete removed campsites: %w", err)
	}
	if err := m.store.RefreshCampgroundAggregates(ctx, providerName, campgroundID); err != nil {
		return fmt.Errorf("refresh campground aggregates: %w", err)
	}
	m.logger.Info("campsite metadata refreshed",
		slog.String("provider", providerName),
		slog.String("campground", campgroundID),
		slog.Int("sites", len(seen)),
		slog.Int("fetched", len(fetched)),
		slog.Int64("removed", removed))
	return nil
}

// recentPollFailure reports whether the provider's active poll loop has
// failed within pollFailureQuiet.
func (m *Manager) recentPollFailure(providerName string) bool {
	v, ok := m.lastPollFailure.Load(providerName)
	return ok && time.Since(v.(time.Time)) < pollFailureQuiet
}
