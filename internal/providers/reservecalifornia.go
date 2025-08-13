package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/brensch/schniffer/internal/httpx"
)

// ReserveCalifornia implements the Provider interface using the UseDirect endpoints.
// Docs are inferred from examples in reservecalifornia_examples.md.
type ReserveCalifornia struct {
	client *http.Client
}

func NewReserveCalifornia() *ReserveCalifornia { return &ReserveCalifornia{client: httpx.Default()} }

func (r *ReserveCalifornia) Name() string { return "reservecalifornia" }

// CampsiteURL returns a generic ReserveCalifornia URL. Deep links are not stable across sessions.
func (r *ReserveCalifornia) CampsiteURL(campgroundID string, _ string) string {
	return "https://reservecalifornia.com/" // fallback landing page
}

// PlanBuckets: ReserveCalifornia can query an arbitrary date range per facility, so collapse to a single [min..max] range.
func (r *ReserveCalifornia) PlanBuckets(dates []time.Time) []DateRange {
	if len(dates) == 0 {
		return nil
	}
	min := dates[0].UTC()
	max := dates[0].UTC()
	min = time.Date(min.Year(), min.Month(), min.Day(), 0, 0, 0, 0, time.UTC)
	max = min
	for _, d := range dates[1:] {
		dd := d.UTC()
		dd = time.Date(dd.Year(), dd.Month(), dd.Day(), 0, 0, 0, 0, time.UTC)
		if dd.Before(min) {
			min = dd
		}
		if dd.After(max) {
			max = dd
		}
	}
	return []DateRange{{Start: min, End: max}}
}

// gridRequest is the payload for the search/grid endpoint.
type gridRequest struct {
	IsADA             bool   `json:"IsADA"`
	MinVehicleLength  int    `json:"MinVehicleLength"`
	UnitCategoryId    int    `json:"UnitCategoryId"`
	StartDate         string `json:"StartDate"` // YYYY-MM-DD
	WebOnly           bool   `json:"WebOnly"`
	UnitTypesGroupIds []int  `json:"UnitTypesGroupIds"`
	SleepingUnitId    int    `json:"SleepingUnitId"`
	EndDate           string `json:"EndDate"` // YYYY-MM-DD
	UnitSort          string `json:"UnitSort"`
	InSeasonOnly      bool   `json:"InSeasonOnly"`
	FacilityId        string `json:"FacilityId"`
	RestrictADA       bool   `json:"RestrictADA"`
}

// Partial response shape for search/grid sufficient to extract availability.
type gridResponse struct {
	Facility struct {
		Units map[string]struct {
			UnitId int `json:"UnitId"`
			Slices map[string]struct {
				Date      string `json:"Date"` // YYYY-MM-DD
				IsFree    bool   `json:"IsFree"`
				IsBlocked bool   `json:"IsBlocked"`
			} `json:"Slices"`
		} `json:"Units"`
	} `json:"Facility"`
}

// FetchAvailability calls the search/grid endpoint for the given FacilityId (campgroundID) and range.
func (r *ReserveCalifornia) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]Campsite, error) {
	if campgroundID == "" {
		return nil, fmt.Errorf("facility/campground id required")
	}
	// API expects inclusive dates in YYYY-MM-DD local to PST; using UTC dates is fine for midnight day granularity.
	payload := gridRequest{
		IsADA:             false,
		MinVehicleLength:  0,
		UnitCategoryId:    0,
		StartDate:         start.UTC().Format("2006-01-02"),
		WebOnly:           true,
		UnitTypesGroupIds: []int{},
		SleepingUnitId:    0,
		EndDate:           end.UTC().Format("2006-01-02"),
		UnitSort:          "orderby",
		InSeasonOnly:      true,
		FacilityId:        campgroundID,
		RestrictADA:       false,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://calirdr.usedirect.com/RDR/rdr/search/grid", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpx.SpoofChromeHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://reservecalifornia.com")
	req.Header.Set("Referer", "https://reservecalifornia.com/")

	slog.Info("Fetching RC grid", slog.String("facility", campgroundID), slog.String("start", payload.StartDate), slog.String("end", payload.EndDate))
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grid POST failed: %w", err)
	}
	b, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("grid read body failed: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reservecalifornia grid status %d; body: %s", resp.StatusCode, clipBody(b))
	}
	var parsed gridResponse
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, fmt.Errorf("grid JSON decode failed: %w; body: %s", err, clipBody(b))
	}
	var out []Campsite
	for _, u := range parsed.Facility.Units {
		siteID := strconv.Itoa(u.UnitId)
		for _, s := range u.Slices {
			// s.Date is YYYY-MM-DD; interpret as UTC midnight
			d, err := time.Parse("2006-01-02", s.Date)
			if err != nil {
				continue
			}
			out = append(out, Campsite{ID: siteID, Date: d.UTC(), Available: s.IsFree && !s.IsBlocked})
		}
	}
	return out, nil
}

// FetchAllCampgrounds enumerates city parks, then places and facilities to build a list of campgrounds keyed by FacilityId.
func (r *ReserveCalifornia) FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error) {
	// 1) Fetch all city parks
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://calirdr.usedirect.com/RDR/rdr/fd/citypark", nil)
	if err != nil {
		return nil, err
	}
	httpx.SpoofChromeHeaders(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("citypark GET failed: %w", err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("citypark read body failed: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("citypark status %d; body: %s", resp.StatusCode, clipBody(body))
	}
	var parks map[string]struct {
		CityParkId int     `json:"CityParkId"`
		Name       string  `json:"Name"`
		Latitude   float64 `json:"Latitude"`
		Longitude  float64 `json:"Longitude"`
		PlaceId    int     `json:"PlaceId"`
	}
	if err := json.Unmarshal(body, &parks); err != nil {
		return nil, fmt.Errorf("citypark JSON decode failed: %w", err)
	}

	// 2) For each park/place, fetch facilities via search/place
	type placeResp struct {
		SelectedPlace struct {
			PlaceId    int     `json:"PlaceId"`
			Name       string  `json:"Name"`
			Latitude   float64 `json:"Latitude"`
			Longitude  float64 `json:"Longitude"`
			Facilities map[string]struct {
				FacilityId int     `json:"FacilityId"`
				Name       string  `json:"Name"`
				Latitude   float64 `json:"Latitude"`
				Longitude  float64 `json:"Longitude"`
			} `json:"Facilities"`
		} `json:"SelectedPlace"`
	}

	var out []CampgroundInfo
	// modest cap to avoid hammering if something goes wrong
	checked := 0
	for _, p := range parks {
		// Skip parks without a PlaceId
		if p.PlaceId == 0 {
			continue
		}
		pr := map[string]string{"PlaceId": strconv.Itoa(p.PlaceId)}
		pb, _ := json.Marshal(pr)
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://calirdr.usedirect.com/RDR/rdr/search/place", bytes.NewReader(pb))
		if err != nil {
			slog.Warn("build place request failed", slog.Any("err", err))
			continue
		}
		httpx.SpoofChromeHeaders(req2)
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Origin", "https://reservecalifornia.com")
		req2.Header.Set("Referer", "https://reservecalifornia.com/")

		resp2, err := r.client.Do(req2)
		if err != nil {
			slog.Warn("place POST failed", slog.Any("err", err), slog.Int("placeId", p.PlaceId))
			continue
		}
		b2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			slog.Warn("place status not OK", slog.Int("status", resp2.StatusCode), slog.Int("placeId", p.PlaceId))
			continue
		}
		var prParsed placeResp
		if err := json.Unmarshal(b2, &prParsed); err != nil {
			slog.Warn("place JSON decode failed", slog.Any("err", err), slog.Int("placeId", p.PlaceId))
			continue
		}
		parentName := prParsed.SelectedPlace.Name
		parentID := strconv.Itoa(prParsed.SelectedPlace.PlaceId)
		for _, f := range prParsed.SelectedPlace.Facilities {
			out = append(out, CampgroundInfo{
				ID:         strconv.Itoa(f.FacilityId),
				Name:       f.Name,
				Lat:        f.Latitude,
				Lon:        f.Longitude,
				ParentID:   parentID,
				ParentName: parentName,
			})
		}
		checked++
		// Soft limit to keep the sync bounded if citypark is huge; remove or raise later
		if checked >= 2000 {
			break
		}
	}
	return out, nil
}
