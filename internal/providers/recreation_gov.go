package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"log/slog"

	"github.com/brensch/schniffer/internal/httpx"
)

type RecreationGov struct {
	client *http.Client
}

func NewRecreationGov() *RecreationGov {
	return &RecreationGov{client: httpx.Default()}
}

func (r *RecreationGov) Name() string { return "recreation_gov" }

// CampsiteURL implements providers.Provider
func (r *RecreationGov) CampsiteURL(_ string, campsiteID string) string {
	if campsiteID == "" {
		return ""
	}
	return "https://www.recreation.gov/camping/campsites/" + campsiteID
}

// minimal response structs following campbot logic: availability is monthly and keyed by campsite id and date
type recGovResp struct {
	Campsites map[string]struct {
		Availabilities map[string]string `json:"availabilities"`
	} `json:"campsites"`
}

// FetchAvailability fetches monthly availability pages between start and end (inclusive by month).
func (r *RecreationGov) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]Campsite, error) {
	var out []Campsite
	cur := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	endMonth := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !cur.After(endMonth) {
		base := fmt.Sprintf("https://www.recreation.gov/api/camps/availability/campground/%s/month", campgroundID)
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("invalid base url: %w", err)
		}
		q := u.Query()
		// Recreation.gov expects RFC3339 with milliseconds and Zulu time.
		q.Set("start_date", cur.UTC().Format("2006-01-02T15:04:05.000Z"))
		u.RawQuery = q.Encode()
		slog.Info("Fetching availability", slog.String("url", u.String()))
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		httpx.SpoofChromeHeaders(req)
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("availability GET failed: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("availability read body failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("recreation.gov availability status %d; body: %s", resp.StatusCode, clipBody(body))
		}
		var parsed recGovResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("availability JSON decode failed: %w; body: %s", err, clipBody(body))
		}
		for siteID, data := range parsed.Campsites {
			for dateStr, status := range data.Availabilities {
				d, err := time.Parse(time.RFC3339, dateStr)
				if err != nil {
					slog.Error("bad date from rec.gov", slog.String("date", dateStr))
					continue
				}
				out = append(out, Campsite{ID: siteID, Date: d, Available: status == "Available"})
			}
		}
		cur = cur.AddDate(0, 1, 0)
	}
	return out, nil
}

// PlanBuckets groups dates by month and returns one monthly range per group from day 1 to last day of month.
func (r *RecreationGov) PlanBuckets(dates []time.Time) []DateRange {
	if len(dates) == 0 {
		return nil
	}
	// Normalize to month keys
	seen := map[time.Time]struct{}{}
	for i := range dates {
		d := dates[i].UTC()
		dates[i] = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	}
	for _, d := range dates {
		m := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
		seen[m] = struct{}{}
	}
	out := make([]DateRange, 0, len(seen))
	for m := range seen {
		out = append(out, DateRange{Start: m, End: m.AddDate(0, 1, -1)})
	}
	return out
}

// FetchAllCampgrounds scrapes the recreation.gov search API, paging through all results.
func (r *RecreationGov) FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error) {
	start := 0
	size := 100
	var all []CampgroundInfo
	for {
		endpoint := fmt.Sprintf("https://recreation.gov/api/search?fq=entity_type%%3Acampground&size=%d&start=%d", size, start)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		httpx.SpoofChromeHeaders(req)
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("search GET failed: %w", err)
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("search read body failed: %w", rerr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("recreation.gov search status %d; body: %s", resp.StatusCode, clipBody(body))
		}
		var page struct {
			Results []struct {
				Name       string `json:"name"`
				EntityID   string `json:"entity_id"`
				Latitude   string `json:"latitude"`
				Longitude  string `json:"longitude"`
				ParentID   string `json:"parent_id"`
				ParentName string `json:"parent_name"`
				Reservable bool   `json:"reservable"`
			} `json:"results"`
			Size int `json:"size"`
		}
		if decErr := json.Unmarshal(body, &page); decErr != nil {
			return nil, fmt.Errorf("search JSON decode failed: %w; body: %s", decErr, clipBody(body))
		}
		for _, r := range page.Results {
			if !r.Reservable {
				continue
			}
			var lat, lon float64
			if r.Latitude != "" {
				if v, err := strconv.ParseFloat(r.Latitude, 64); err == nil {
					lat = v
				}
			}
			if r.Longitude != "" {
				if v, err := strconv.ParseFloat(r.Longitude, 64); err == nil {
					lon = v
				}
			}
			all = append(all, CampgroundInfo{
				ID:         r.EntityID,
				Name:       r.Name,
				Lat:        lat,
				Lon:        lon,
				ParentID:   r.ParentID,
				ParentName: r.ParentName,
			})
		}
		if page.Size < size {
			break
		}
		start += page.Size
	}
	return all, nil
}

// clipBody returns a short string version of a response body for error messages.
// It limits to a reasonable size to avoid logging huge payloads.
func clipBody(b []byte) string {
	const max = 2048
	if len(b) == 0 {
		return ""
	}
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
