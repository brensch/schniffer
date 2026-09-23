package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/httpx"
)

// ReserveCalifornia implements the Provider interface using the UseDirect endpoints.
// Docs are inferred from examples in reservecalifornia_examples.md.
//
// ReserveCalifornia's API sits behind a CloudFront WAF that blocks
// datacenter/cloud IPs by reputation (403 "Request blocked"), independent
// of rate or headers. Measured 2026-07-08: the residential host IP
// succeeds ~100% while pooled Cloud Run IPs get 403s ~60%. By 2026-07-31 the
// WAF blocked the pool entirely (0/1100 fetches across all 42 regions), so
// routing RC through the pool and letting the per-IP backoff find allowed
// IPs no longer works. RC fetches go out via the direct (host-IP) client,
// with the proxy pool kept only as a fallback if the host IP ever degrades.
type ReserveCalifornia struct {
	direct *http.Client // primary: host IP, not WAF-blocked
	pool   *http.Client // fallback: proxy pool (rotating cloud IPs)
}

func NewReserveCalifornia() *ReserveCalifornia {
	return &ReserveCalifornia{direct: httpx.Direct(), pool: httpx.Default()}
}

// clientForAttempt routes early attempts through the direct host IP (which
// RC's WAF allows) and later retries alternately through the proxy pool, so
// RC still has a path if the host IP is ever blocked.
func (r *ReserveCalifornia) clientForAttempt(i int) *http.Client {
	if i%2 == 1 {
		return r.pool
	}
	return r.direct
}

func (r *ReserveCalifornia) Name() string { return "reservecalifornia" }

// reserveCaliforniaSiteOrigin is the browser-facing site (distinct from the API
// base URL). In August 2026 ReserveCalifornia replaced the old Angular app —
// which served a hash router under /Web/ — with a React app served from /.
// The old "/Web/#!park/x/y" deep links now 404.
const reserveCaliforniaSiteOrigin = "https://www.reservecalifornia.com"

// parkPath renders the site's facility route, "/park/:parkId/:facilityId".
// campgroundID format: "parentID-facilityID" (e.g., "1260-2181")
func reserveCaliforniaParkPath(campgroundID string) string {
	// Parse composite ID: parentID-facilityID
	parts := strings.Split(campgroundID, "-")
	if len(parts) != 2 {
		return "" // unexpected ID format; caller falls back to the site root
	}
	parentID := parts[0]
	facilityID := parts[1]
	return fmt.Sprintf("/park/%s/%s", parentID, facilityID)
}

// CampsiteURL returns a ReserveCalifornia URL for the campground.
// ReserveCalifornia has no per-unit route, so the campsite ID is ignored and
// this lands on the campground's availability grid.
func (r *ReserveCalifornia) CampsiteURL(campgroundID string, _ string) string {
	return r.CampgroundURL(campgroundID)
}

// CampgroundURL returns a ReserveCalifornia URL for the campground.
func (r *ReserveCalifornia) CampgroundURL(campgroundID string) string {
	path := reserveCaliforniaParkPath(campgroundID)
	if path == "" {
		return reserveCaliforniaSiteOrigin + "/"
	}
	return reserveCaliforniaSiteOrigin + path
}

// CampgroundURLForStay implements providers.StayURLProvider. The facility page
// reads "date" (arrival, YYYY-MM-DD) and "night" off the query string and opens
// the grid already scoped to that stay, which saves the user from re-entering
// their dates while the site is racing them for the campsite.
func (r *ReserveCalifornia) CampgroundURLForStay(campgroundID string, checkin time.Time, nights int) string {
	path := reserveCaliforniaParkPath(campgroundID)
	if path == "" || checkin.IsZero() || nights <= 0 {
		return r.CampgroundURL(campgroundID)
	}
	return fmt.Sprintf("%s%s?date=%s&night=%d",
		reserveCaliforniaSiteOrigin, path, checkin.UTC().Format("2006-01-02"), nights)
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
			UnitId int    `json:"UnitId"`
			Name   string `json:"Name"` // e.g., "Tent Campsite #C36"
			Slices map[string]struct {
				Date      string `json:"Date"` // YYYY-MM-DD
				IsFree    bool   `json:"IsFree"`
				IsBlocked bool   `json:"IsBlocked"`
			} `json:"Slices"`
		} `json:"Units"`
	} `json:"Facility"`
}

const maxRetriesAvailability = 5

// Base URL for the ReserveCalifornia API (Tyler Technologies platform)
const reserveCaliforniaBaseURL = "https://california-rdr.prod.cali.rd12.recreation-management.tylerapp.com"

// FetchAvailability calls the search/grid endpoint for the given FacilityId (campgroundID) and range.
func (r *ReserveCalifornia) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]CampsiteAvailability, error) {
	if campgroundID == "" {
		return nil, fmt.Errorf("facility/campground id required")
	}

	// Extract facility ID from composite ID format "parentID-facilityID"
	facilityID := campgroundID
	if parts := strings.Split(campgroundID, "-"); len(parts) == 2 {
		facilityID = parts[1]
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
		FacilityId:        facilityID,
		RestrictADA:       false,
	}
	body, _ := json.Marshal(payload)

	var intErr error
	var parsed gridResponse
	failedAttempts := 0
	for i := 0; i < maxRetriesAvailability; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reserveCaliforniaBaseURL+"/rdr/search/grid", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}

		httpx.SpoofChromeHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://www.reservecalifornia.com")
		req.Header.Set("Referer", "https://www.reservecalifornia.com/")
		req.Header.Set("tenantid", "cali")
		// This is an XHR/fetch API call, not a page navigation. Override the
		// navigation Sec-Fetch-* headers SpoofChromeHeaders may set so the
		// request matches what the real site's JS sends (a WAF tell).
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Del("Upgrade-Insecure-Requests")

		time.Sleep(time.Duration(i) * 500 * time.Millisecond) // small per-retry backoff between attempts

		slog.Info("Fetching RC grid", slog.String("facility", facilityID), slog.String("start", payload.StartDate), slog.String("end", payload.EndDate))
		fetchStart := time.Now()
		resp, err := r.clientForAttempt(i).Do(req)
		if err != nil {
			recordFetch("reservecalifornia", fetchStart, false)
			failedAttempts++
			slog.Warn("grid POST failed", slog.Any("err", err), slog.String("facility", campgroundID))
			intErr = err
			continue
		}
		observeUpstream("reservecalifornia", resp)
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			recordFetch("reservecalifornia", fetchStart, false)
			failedAttempts++
			slog.Warn("grid read body failed", slog.Any("err", rerr), slog.String("facility", campgroundID))
			intErr = fmt.Errorf("grid read body failed: %w", rerr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			recordFetch("reservecalifornia", fetchStart, false)
			failedAttempts++
			slog.Warn("grid status not OK", slog.Int("status", resp.StatusCode), slog.String("facility", campgroundID), slog.String("body", string(b)))
			intErr = fmt.Errorf("grid status %d; body: %s", resp.StatusCode, clipBody(b))
			continue
		}

		err = json.Unmarshal(b, &parsed)
		if err != nil {
			recordFetch("reservecalifornia", fetchStart, false)
			failedAttempts++
			slog.Warn("grid JSON decode failed", slog.Any("err", err), slog.String("body", string(b)))
			intErr = fmt.Errorf("grid JSON decode failed: %w; body: %s", err, clipBody(b))
			continue
		}

		recordFetch("reservecalifornia", fetchStart, true)
		intErr = nil
		break
	}

	if intErr != nil {
		return nil, fmt.Errorf("grid fetch failed after %d attempts: %w", maxRetriesAvailability, intErr)
	}
	if failedAttempts > 0 {
		// Succeeded, but only after the retry loop rotated past blocked
		// egress IPs. Surface it — a rising count here is the early
		// warning that the WAF is closing in on the proxy pool.
		slog.Warn("RC grid degraded: succeeded after retries",
			slog.Int("failed_attempts", failedAttempts),
			slog.String("facility", facilityID))
	}

	var out []CampsiteAvailability
	for _, u := range parsed.Facility.Units {
		siteID := strconv.Itoa(u.UnitId)
		for _, s := range u.Slices {
			// s.Date is YYYY-MM-DD; interpret as UTC midnight
			d, err := time.Parse("2006-01-02", s.Date)
			if err != nil {
				continue
			}
			out = append(out, CampsiteAvailability{
				ID:        siteID,
				Date:      d.UTC(),
				Available: s.IsFree && !s.IsBlocked,
			})
		}
	}
	return out, nil
}

// FetchAllCampgrounds enumerates city parks, then places and facilities to build a list of campgrounds keyed by FacilityId.
func (r *ReserveCalifornia) FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error) {
	// 1) Fetch all city parks
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reserveCaliforniaBaseURL+"/rdr/fd/citypark", nil)
	if err != nil {
		return nil, err
	}
	httpx.SpoofChromeHeaders(req)
	req.Header.Set("tenantid", "cali")
	resp, err := r.direct.Do(req)
	if err != nil {
		return nil, fmt.Errorf("citypark GET failed: %w", err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("citypark read body failed: %w", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("citypark", resp.StatusCode, body)
	}
	var parks map[string]struct {
		CityParkId int     `json:"CityParkId"`
		Name       string  `json:"Name"`
		Latitude   float64 `json:"Latitude"`
		Longitude  float64 `json:"Longitude"`
		PlaceId    int     `json:"PlaceId"`
		IsActive   bool    `json:"IsActive"`
	}
	err = json.Unmarshal(body, &parks)
	if err != nil {
		slog.Error("citypark JSON decode failed", slog.Any("err", err), slog.String("body", string(body)))
		return nil, fmt.Errorf("citypark JSON decode failed: %w", err)
	}

	// 2) For each park/place, fetch facilities via search/place
	type placeResp struct {
		SelectedPlace struct {
			PlaceId       int     `json:"PlaceId"`
			Name          string  `json:"Name"`
			Description   string  `json:"Description"`
			Latitude      float64 `json:"Latitude"`
			Longitude     float64 `json:"Longitude"`
			ImageUrl      string  `json:"ImageUrl"`
			Allhighlights string  `json:"Allhighlights"`
			Facilities    map[string]struct {
				FacilityId    int     `json:"FacilityId"`
				Name          string  `json:"Name"`
				Description   string  `json:"Description"`
				Latitude      float64 `json:"Latitude"`
				Longitude     float64 `json:"Longitude"`
				Category      string  `json:"Category"`
				Allhighlights string  `json:"Allhighlights"`
			} `json:"Facilities"`
		} `json:"SelectedPlace"`
	}

	// One attempt per park, paced: every call leaves from the host IP that
	// the active schniff polls also depend on. Any failure aborts the whole
	// list, since callers treat a missing campground as removed.
	var out []CampgroundInfo
	for _, p := range parks {

		// Skip inactive parks or parks without a PlaceId
		if !p.IsActive || p.PlaceId == 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(metadataRequestSpacing):
		}

		pb, _ := json.Marshal(map[string]string{"PlaceId": strconv.Itoa(p.PlaceId)})
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, reserveCaliforniaBaseURL+"/rdr/search/place", bytes.NewReader(pb))
		if err != nil {
			return nil, err
		}
		httpx.SpoofChromeHeaders(req2)
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Origin", "https://www.reservecalifornia.com")
		req2.Header.Set("Referer", "https://www.reservecalifornia.com/")
		req2.Header.Set("tenantid", "cali")

		resp2, err := r.direct.Do(req2)
		if err != nil {
			return nil, fmt.Errorf("place POST failed for PlaceId %d: %w", p.PlaceId, err)
		}
		b2, err := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("place read body failed for PlaceId %d: %w", p.PlaceId, err)
		}
		if resp2.StatusCode != http.StatusOK {
			return nil, statusError(fmt.Sprintf("place %d", p.PlaceId), resp2.StatusCode, b2)
		}
		var prParsed placeResp
		err = json.Unmarshal(b2, &prParsed)
		if err != nil {
			slog.Warn("place JSON decode failed", slog.Any("err", err), slog.Int("placeId", p.PlaceId))
			return nil, fmt.Errorf("place JSON decode failed: %w; body: %s", err, clipBody(b2))
		}

		parentName := prParsed.SelectedPlace.Name
		parentID := strconv.Itoa(prParsed.SelectedPlace.PlaceId)
		parentImageURL := prParsed.SelectedPlace.ImageUrl

		for _, f := range prParsed.SelectedPlace.Facilities {
			// Only include campground facilities
			if !strings.Contains(strings.ToLower(f.Category), "campground") {
				continue
			}

			// Create composite ID and name for ReserveCalifornia
			compositeID := parentID + "-" + strconv.Itoa(f.FacilityId)
			compositeName := parentName + ": " + f.Name

			// Extract amenities from highlights if available
			var amenities []string
			highlights := f.Allhighlights
			// If facility doesn't have highlights, try using parent place highlights
			if highlights == "" {
				highlights = prParsed.SelectedPlace.Allhighlights
			}

			if highlights != "" {
				// Parse highlights like "Birdwatching<br>Boating<br>Boat launch<br>..."
				highlightParts := strings.Split(highlights, "<br>")
				for _, highlight := range highlightParts {
					highlight = strings.TrimSpace(highlight)
					if highlight != "" {
						amenities = append(amenities, strings.ToLower(highlight))
					}
				}
			}

			// Use facility image if available, otherwise use parent image
			imageURL := parentImageURL
			if f.Latitude != 0 && f.Longitude != 0 {
				// Use facility coordinates if available, otherwise use parent coordinates
				out = append(out, CampgroundInfo{
					ID:        compositeID,
					Name:      compositeName,
					Lat:       f.Latitude,
					Lon:       f.Longitude,
					Rating:    0.0, // ReserveCalifornia doesn't provide ratings in their API
					Amenities: amenities,
					ImageURL:  imageURL,
					PriceMin:  0.0, // Would need separate API call to get pricing
					PriceMax:  0.0,
					PriceUnit: "night",
				})
			} else {
				out = append(out, CampgroundInfo{
					ID:        compositeID,
					Name:      compositeName,
					Lat:       prParsed.SelectedPlace.Latitude,
					Lon:       prParsed.SelectedPlace.Longitude,
					Rating:    0.0,
					Amenities: amenities,
					ImageURL:  imageURL,
					PriceMin:  0.0,
					PriceMax:  0.0,
					PriceUnit: "night",
				})
			}
		}
	}
	return out, nil
}

// metadataRequestSpacing paces metadata calls (place lookups, per-site
// details). They share the host IP with the active grid polls, and the
// CloudFront WAF in front of the API blocks bursts.
const metadataRequestSpacing = 10 * time.Second

// FetchCampsites returns detailed campsite metadata for storage in the database
func (r *ReserveCalifornia) FetchCampsites(ctx context.Context, campgroundID string) ([]CampsiteInfo, error) {
	fetched, _, err := r.FetchCampsitesSkipping(ctx, campgroundID, nil)
	return fetched, err
}

// FetchCampsitesSkipping implements IncrementalCampsiteFetcher. Listing a
// facility's sites is one grid call, but each site's details are another
// call, so sites in skip are only reported in seen.
func (r *ReserveCalifornia) FetchCampsitesSkipping(ctx context.Context, campgroundID string, skip map[string]bool) ([]CampsiteInfo, []string, error) {
	// Extract facility ID from composite ID format "parentID-facilityID"
	var facilityID string
	if parts := strings.Split(campgroundID, "-"); len(parts) == 2 {
		facilityID = parts[1]
	}

	// Use current date as start date to get campsite structure
	start := time.Now()
	end := start.AddDate(0, 0, 7) // One week window to get campsite structure

	// Build grid request to get campsite information
	payload := gridRequest{
		IsADA:             false,
		MinVehicleLength:  0,
		UnitCategoryId:    0,
		StartDate:         start.Format("2006-01-02"),
		WebOnly:           true,
		UnitTypesGroupIds: []int{},
		SleepingUnitId:    0,
		EndDate:           end.Format("2006-01-02"),
		UnitSort:          "orderby",
		InSeasonOnly:      true,
		FacilityId:        facilityID,
		RestrictADA:       false,
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reserveCaliforniaBaseURL+"/rdr/search/grid", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpx.SpoofChromeHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.reservecalifornia.com")
	req.Header.Set("Referer", "https://www.reservecalifornia.com/")
	req.Header.Set("tenantid", "cali")

	resp, err := r.direct.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("campsite metadata grid request failed: %w", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read campsite metadata response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, statusError("campsite metadata grid", resp.StatusCode, respBody)
	}

	// Parse using expanded structure to get unit details
	var gridResp struct {
		Facility struct {
			Units map[string]struct {
				UnitId          int    `json:"UnitId"`
				Name            string `json:"Name"`
				ShortName       string `json:"ShortName"`
				IsAda           bool   `json:"IsAda"`
				UnitTypeId      int    `json:"UnitTypeId"`
				UnitTypeGroupId int    `json:"UnitTypeGroupId"`
				VehicleLength   int    `json:"VehicleLength"`
			} `json:"Units"`
		} `json:"Facility"`
	}

	if err := json.Unmarshal(respBody, &gridResp); err != nil {
		return nil, nil, fmt.Errorf("failed to parse campsite metadata response: %w", err)
	}

	slog.Info("Retrieved campsite grid data",
		slog.String("facilityId", facilityID),
		slog.Int("unitCount", len(gridResp.Facility.Units)))

	var campsiteInfos []CampsiteInfo
	var seen []string
	for _, unit := range gridResp.Facility.Units {
		unitID := strconv.Itoa(unit.UnitId)
		seen = append(seen, unitID)
		if skip[unitID] {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(metadataRequestSpacing):
		}

		// Get detailed campsite information with retries
		detailsURL := fmt.Sprintf("%s/rdr/search/details/%d/startdate/%s",
			reserveCaliforniaBaseURL, unit.UnitId, start.Format("2006-01-02"))

		slog.Info("Fetching campsite details",
			slog.String("unitId", fmt.Sprintf("%d", unit.UnitId)),
			slog.String("url", detailsURL))

		// Try to get details with exponential backoff
		var detailsResp struct {
			Unit struct {
				UnitId          int    `json:"UnitId"`
				Name            string `json:"Name"`
				DescriptionHtml string `json:"DescriptionHtml"`
				IsADA           bool   `json:"IsADA"`
				IsTentSite      bool   `json:"IsTentSite"`
				IsRVSite        bool   `json:"IsRVSite"`
				VehicleLength   int    `json:"VehicleLength"`
			} `json:"Unit"`
			Rate        string `json:"Rate"`
			Fee         string `json:"Fee"`
			UnitImage   string `json:"UnitImage"`
			NightlyUnit struct {
				MaxOccupancy int `json:"MaxOccupancy"`
				MaxVehicles  int `json:"MaxVehicles"`
			} `json:"NightlyUnit"`
			UnitType struct {
				Name string `json:"Name"`
			} `json:"UnitType"`
			Amenities map[string]struct {
				AmenityId   int    `json:"AmenityId"`
				Name        string `json:"Name"`
				ShortName   string `json:"ShortName"`
				Description string `json:"Description"`
				Value       string `json:"Value"`
			} `json:"Amenities"`
		}

		detailReq, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
		if err != nil {
			return nil, nil, err
		}
		httpx.SpoofChromeHeaders(detailReq)
		detailReq.Header.Set("Origin", "https://www.reservecalifornia.com")
		detailReq.Header.Set("Referer", "https://www.reservecalifornia.com/")
		detailReq.Header.Set("tenantid", "cali")

		detailResp, err := r.direct.Do(detailReq)
		if err != nil {
			slog.Warn("failed to fetch campsite details", slog.Int("unitId", unit.UnitId), slog.Any("err", err))
			continue
		}
		detailBody, err := io.ReadAll(detailResp.Body)
		detailResp.Body.Close()
		if err != nil {
			slog.Warn("failed to read campsite details", slog.Int("unitId", unit.UnitId), slog.Any("err", err))
			continue
		}
		if detailResp.StatusCode != http.StatusOK {
			err := statusError(fmt.Sprintf("campsite details %d", unit.UnitId), detailResp.StatusCode, detailBody)
			if errors.Is(err, ErrRateLimited) {
				return nil, nil, err
			}
			// One bad site shouldn't sink the facility; it stays in seen
			// and gets another go next cycle.
			slog.Warn("campsite details failed", slog.Any("err", err))
			continue
		}
		if err := json.Unmarshal(detailBody, &detailsResp); err != nil {
			slog.Warn("failed to parse campsite details", slog.Int("unitId", unit.UnitId), slog.Any("err", err))
			continue
		}

		// Determine equipment types based on site characteristics
		var equipment []string
		if detailsResp.Unit.IsTentSite {
			equipment = append(equipment, "tent")
		}
		if detailsResp.Unit.IsRVSite {
			equipment = append(equipment, "rv")
			if detailsResp.Unit.VehicleLength > 0 {
				equipment = append(equipment, fmt.Sprintf("rv up to %d ft", detailsResp.Unit.VehicleLength))
			}
		}
		if len(equipment) == 0 {
			equipment = append(equipment, "standard")
		}

		// Parse cost per night
		var costPerNight float64
		if detailsResp.Rate != "" {
			if cost, err := strconv.ParseFloat(detailsResp.Rate, 64); err == nil {
				costPerNight = cost
			}
		}

		// Determine campsite type from unit type name or characteristics (convert to lowercase)
		campsiteType := strings.ToLower(detailsResp.UnitType.Name)
		if campsiteType == "" {
			// Inline campsite type detection (returning lowercase)
			unitLower := strings.ToLower(detailsResp.Unit.Name)
			switch {
			case strings.Contains(unitLower, "tent"):
				campsiteType = "tent"
			case strings.Contains(unitLower, "rv"):
				campsiteType = "rv"
			case strings.Contains(unitLower, "cabin"):
				campsiteType = "cabin"
			case strings.Contains(unitLower, "group"):
				campsiteType = "group"
			case strings.Contains(unitLower, "primitive"):
				campsiteType = "primitive"
			case strings.Contains(unitLower, "yurt"):
				campsiteType = "yurt"
			case strings.Contains(unitLower, "camp"):
				campsiteType = "campsite"
			default:
				campsiteType = "standard"
			}
		}

		// Extract amenities from the detailed response
		var amenities []string
		for _, amenity := range detailsResp.Amenities {
			// Convert amenity names to lowercase and add to list
			amenityName := strings.ToLower(amenity.Name)
			if amenityName != "" {
				amenities = append(amenities, amenityName)
			}
		}

		campsiteInfos = append(campsiteInfos, CampsiteInfo{
			ID:              unitID,
			Name:            detailsResp.Unit.Name,
			Type:            campsiteType,
			CostPerNight:    costPerNight,
			Rating:          0.0, // ReserveCalifornia doesn't provide ratings
			Equipment:       equipment,
			Amenities:       amenities,
			PreviewImageURL: detailsResp.UnitImage,
		})
	}

	slog.Info("Completed campsite metadata fetch",
		slog.String("facilityId", facilityID),
		slog.Int("totalUnits", len(gridResp.Facility.Units)),
		slog.Int("skipped", len(seen)-len(campsiteInfos)),
		slog.Int("fetchedDetails", len(campsiteInfos)))

	return campsiteInfos, seen, nil
}
