package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type RecreationGov struct {
	client *http.Client
}

func NewRecreationGov() *RecreationGov {
	return &RecreationGov{client: &http.Client{Timeout: 15 * time.Second}}
}

func (r *RecreationGov) Name() string { return "recreation_gov" }

// minimal response structs following campbot logic: availability is monthly and keyed by campsite id and date

type recGovResp struct {
	Campsites map[string]struct {
		Availabilities map[string]string `json:"availabilities"`
	} `json:"campsites"`
}

func (r *RecreationGov) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]Campsite, error) {
	// Query per-month range windows
	var out []Campsite
	cur := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	endMonth := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !cur.After(endMonth) {
		url := fmt.Sprintf("https://www.recreation.gov/api/camps/availability/campground/%s/month?start_date=%s", campgroundID, cur.Format(time.RFC3339))
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "schniffer/1.0")
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("recreation.gov status %d", resp.StatusCode)
		}
		var parsed recGovResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		for siteID, data := range parsed.Campsites {
			for dateStr, status := range data.Availabilities {
				d, err := time.Parse(time.RFC3339, dateStr)
				if err != nil {
					continue
				}
				if d.Before(start) || d.After(end) {
					continue
				}
				out = append(out, Campsite{ID: siteID, Date: d, Available: status == "Available"})
			}
		}
		cur = cur.AddDate(0, 1, 0)
	}
	return out, nil
}
