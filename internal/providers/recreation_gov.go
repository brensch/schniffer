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

// ResolveCampgrounds: recreation.gov does not expose an easy unauthenticated search endpoint here; as a fallback,
// if the query looks like an ID, return it with a placeholder name. Real implementation could scrape or use cached DB.
func (r *RecreationGov) ResolveCampgrounds(ctx context.Context, query string, limit int) ([]CampgroundInfo, error) {
	if query == "" {
		return nil, nil
	}
	return []CampgroundInfo{{ID: query, Name: "Campground " + query}}, nil
}

// FetchCampsiteMetadata: We'll hit one month (current month) to list campsite IDs; names are not available in this endpoint,
// so we set name to the site ID.
func (r *RecreationGov) FetchCampsiteMetadata(ctx context.Context, campgroundID string) ([]CampsiteInfo, error) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	url := fmt.Sprintf("https://www.recreation.gov/api/camps/availability/campground/%s/month?start_date=%s", campgroundID, start.Format(time.RFC3339))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "schniffer/1.0")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("recreation.gov status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed recGovResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]CampsiteInfo, 0, len(parsed.Campsites))
	for siteID := range parsed.Campsites {
		out = append(out, CampsiteInfo{ID: siteID, Name: siteID})
	}
	return out, nil
}

// FetchAllCampgrounds scrapes the recreation.gov search API, paging through all results.
func (r *RecreationGov) FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error) {
	start := 0
	size := 100
	var all []CampgroundInfo
	for {
		endpoint := fmt.Sprintf("https://recreation.gov/api/search?fq=entity_type%%3Acampground&size=%d&start=%d", size, start)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil { return nil, err }
		req.Header.Set("User-Agent", "schniffer/1.0")
		resp, err := r.client.Do(req)
		if err != nil { return nil, err }
		if resp.StatusCode != 200 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("status %d", resp.StatusCode)
		}
		var page struct {
			Results []struct{
				Name string `json:"name"`
				EntityID string `json:"entity_id"`
			} `json:"results"`
			Size int `json:"size"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil { resp.Body.Close(); return nil, err }
		resp.Body.Close()
		for _, r := range page.Results {
			all = append(all, CampgroundInfo{ID: r.EntityID, Name: r.Name})
		}
		if page.Size < size { break }
		start += page.Size
	}
	return all, nil
}
