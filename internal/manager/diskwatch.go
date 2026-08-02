package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Host disk watcher. Polls the node-exporter metrics already scraped by the
// schniffer prometheus and routes low-disk alerts through notifyOps, so they
// DM the admin rather than pinging the summary channel.

const (
	diskWatchInterval = 15 * time.Minute
	diskAlertReminder = 24 * time.Hour

	diskWarnFreePct = 15.0
	diskCritFreePct = 10.0
	// Recovery needs headroom above warn so a reading wobbling around the
	// threshold doesn't flap alert/recovered pairs.
	diskRecoverFreePct = 17.0
)

type diskState int

const (
	diskOK diskState = iota
	diskWarn
	diskCrit
)

// SetDiskWatch enables the host disk watcher against a Prometheus base URL
// (e.g. http://prometheus:9090). Empty leaves it disabled.
func (m *Manager) SetDiskWatch(promURL string) {
	m.diskPromURL = promURL
}

func (m *Manager) runDiskWatch(ctx context.Context) {
	t := time.NewTicker(diskWatchInterval)
	defer t.Stop()
	state := diskOK
	var lastAlert time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			freePct, freeGB, err := m.queryRootDiskFree(ctx)
			if err != nil {
				m.logger.Warn("disk watch: prometheus query failed", slog.Any("err", err))
				continue
			}
			next, msg := diskTransition(state, freePct, freeGB, time.Since(lastAlert))
			if msg != "" {
				m.notifyOps(msg)
				lastAlert = time.Now()
			}
			state = next
		}
	}
}

// diskTransition decides the next state and what (if anything) to say. Pure
// so the hysteresis is testable: getting worse alerts immediately, an
// unchanged degraded state re-alerts daily, recovery announces once, and a
// crit->warn improvement stays quiet. Between the warn and recover
// thresholds the previous state holds.
func diskTransition(prev diskState, freePct, freeGB float64, sinceLastAlert time.Duration) (diskState, string) {
	next := prev
	switch {
	case freePct < diskCritFreePct:
		next = diskCrit
	case freePct < diskWarnFreePct:
		next = diskWarn
	case freePct >= diskRecoverFreePct:
		next = diskOK
	}
	switch {
	case next > prev:
		return next, diskAlertMessage(next, freePct, freeGB)
	case next == diskOK && prev != diskOK:
		return next, fmt.Sprintf("✅ host disk recovered: %.1f%% free (%.0f GB)", freePct, freeGB)
	case next == prev && next != diskOK && freePct < diskWarnFreePct && sinceLastAlert >= diskAlertReminder:
		return next, diskAlertMessage(next, freePct, freeGB)
	}
	return next, ""
}

func diskAlertMessage(s diskState, freePct, freeGB float64) string {
	if s == diskCrit {
		return fmt.Sprintf("🚨 host disk critically low: %.1f%% free (%.0f GB), below %.0f%%", freePct, freeGB, diskCritFreePct)
	}
	return fmt.Sprintf("⚠️ host disk low: %.1f%% free (%.0f GB), below %.0f%%", freePct, freeGB, diskWarnFreePct)
}

// queryRootDiskFree returns free space on the host root filesystem as a
// percentage and in GB, via the node job's filesystem gauges.
func (m *Manager) queryRootDiskFree(ctx context.Context) (pct, gb float64, err error) {
	avail, err := m.promScalar(ctx, `node_filesystem_avail_bytes{mountpoint="/"}`)
	if err != nil {
		return 0, 0, err
	}
	size, err := m.promScalar(ctx, `node_filesystem_size_bytes{mountpoint="/"}`)
	if err != nil {
		return 0, 0, err
	}
	if size == 0 {
		return 0, 0, fmt.Errorf("prometheus reported zero-size root filesystem")
	}
	return avail / size * 100, avail / 1e9, nil
}

func (m *Manager) promScalar(ctx context.Context, query string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := m.diskPromURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus returned %s", resp.Status)
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	if body.Status != "success" || len(body.Data.Result) == 0 {
		return 0, fmt.Errorf("no result for query %q", query)
	}
	s, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value type for query %q", query)
	}
	return strconv.ParseFloat(s, 64)
}
