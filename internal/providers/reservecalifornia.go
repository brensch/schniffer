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
	"strings"
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

// CampsiteURL returns a ReserveCalifornia URL for the campground.
// campgroundID format: "parentID/facilityID" (e.g., "1260/2181")
func (r *ReserveCalifornia) CampsiteURL(campgroundID string, _ string) string {
	// Parse composite ID: parentID/facilityID
	parts := strings.Split(campgroundID, "/")
	if len(parts) != 2 {
		return "https://reservecalifornia.com/" // fallback if ID format is unexpected
	}
	parentID := parts[0]
	facilityID := parts[1]
	return fmt.Sprintf("https://reservecalifornia.com/Web/#!park/%s/%s", parentID, facilityID)
}

// CampgroundURL returns a ReserveCalifornia URL for the campground.
// campgroundID format: "parentID/facilityID" (e.g., "1260/2181")
func (r *ReserveCalifornia) CampgroundURL(campgroundID string) string {
	// Parse composite ID: parentID/facilityID
	parts := strings.Split(campgroundID, "/")
	if len(parts) != 2 {
		return "https://reservecalifornia.com/" // fallback if ID format is unexpected
	}
	parentID := parts[0]
	facilityID := parts[1]
	return fmt.Sprintf("https://reservecalifornia.com/Web/#!park/%s/%s", parentID, facilityID)
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

// FetchAvailability calls the search/grid endpoint for the given FacilityId (campgroundID) and range.
func (r *ReserveCalifornia) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]CampsiteAvailability, error) {
	if campgroundID == "" {
		return nil, fmt.Errorf("facility/campground id required")
	}

	// Extract facility ID from composite ID format "parentID/facilityID"
	facilityID := campgroundID
	if parts := strings.Split(campgroundID, "/"); len(parts) == 2 {
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
	err = json.Unmarshal(b, &parsed)
	if err != nil {
		return nil, fmt.Errorf("grid JSON decode failed: %w; body: %s", err, clipBody(b))
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
		IsActive   bool    `json:"IsActive"`
	}
	err = json.Unmarshal(body, &parks)
	if err != nil {
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

	var out []CampgroundInfo
	// modest cap to avoid hammering if something goes wrong
	checked := 0
	for _, p := range parks {
		// Skip inactive parks or parks without a PlaceId
		if !p.IsActive || p.PlaceId == 0 {
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
		err = json.Unmarshal(b2, &prParsed)
		if err != nil {
			slog.Warn("place JSON decode failed", slog.Any("err", err), slog.Int("placeId", p.PlaceId))
			continue
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
			compositeID := parentID + "/" + strconv.Itoa(f.FacilityId)
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
		checked++
		// Soft limit to keep the sync bounded if citypark is huge; remove or raise later
		if checked >= 2000 {
			break
		}

		// Add small delay to be respectful to the API
		time.Sleep(50 * time.Millisecond)
	}
	return out, nil
}

// FetchCampsites returns detailed campsite metadata for storage in the database
func (r *ReserveCalifornia) FetchCampsites(ctx context.Context, campgroundID string) ([]CampsiteInfo, error) {
	// Extract facility ID from composite ID format "parentID/facilityID"
	facilityID := campgroundID
	if parts := strings.Split(campgroundID, "/"); len(parts) == 2 {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://calirdr.usedirect.com/RDR/rdr/search/grid", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpx.SpoofChromeHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://reservecalifornia.com")
	req.Header.Set("Referer", "https://reservecalifornia.com/")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("campsite metadata grid request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read campsite metadata response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("campsite metadata request failed with status %d: %s", resp.StatusCode, clipBody(respBody))
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
		return nil, fmt.Errorf("failed to parse campsite metadata response: %w", err)
	}

	slog.Info("Retrieved campsite grid data",
		slog.String("facilityId", facilityID),
		slog.Int("unitCount", len(gridResp.Facility.Units)))

	var campsiteInfos []CampsiteInfo
	for _, unit := range gridResp.Facility.Units {
		// Get detailed campsite information with retries
		detailsURL := fmt.Sprintf("https://calirdr.usedirect.com/RDR/rdr/search/details/%d/startdate/%s",
			unit.UnitId, start.Format("2006-01-02"))

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

		maxRetries := 3
		var detailErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			detailReq, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
			if err != nil {
				detailErr = err
				break
			}
			httpx.SpoofChromeHeaders(detailReq)
			detailReq.Header.Set("Origin", "https://reservecalifornia.com")
			detailReq.Header.Set("Referer", "https://reservecalifornia.com/")

			detailResp, err := r.client.Do(detailReq)
			if err != nil {
				detailErr = err
				if attempt < maxRetries-1 {
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
					continue
				}
				break
			}

			detailBody, err := io.ReadAll(detailResp.Body)
			detailResp.Body.Close()

			if err != nil {
				detailErr = err
				if attempt < maxRetries-1 {
					time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
					continue
				}
				break
			}

			if detailResp.StatusCode == http.StatusTooManyRequests || detailResp.StatusCode >= 500 {
				detailErr = fmt.Errorf("server error %d: %s", detailResp.StatusCode, clipBody(detailBody))
				slog.Warn("server error for campsite details",
					slog.Int("unitId", unit.UnitId),
					slog.Int("status", detailResp.StatusCode),
					slog.Int("attempt", attempt+1),
					slog.String("response", clipBody(detailBody)))
				if attempt < maxRetries-1 {
					time.Sleep(time.Duration(attempt+1) * 1000 * time.Millisecond)
					continue
				}
				break
			}

			if detailResp.StatusCode != http.StatusOK {
				detailErr = fmt.Errorf("status %d: %s", detailResp.StatusCode, clipBody(detailBody))
				slog.Warn("non-200 status for campsite details",
					slog.Int("unitId", unit.UnitId),
					slog.Int("status", detailResp.StatusCode),
					slog.String("response", clipBody(detailBody)))
				break
			}

			if err := json.Unmarshal(detailBody, &detailsResp); err != nil {
				detailErr = err
				break
			}

			// Success!
			detailErr = nil
			break
		}

		if detailErr != nil {
			// If we can't get details, create a basic campsite info from the grid data
			var equipment []string

			// Determine campsite type from unit name (inline, returning lowercase)
			unitLower := strings.ToLower(unit.Name)
			var campsiteType string
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

			// Make some educated guesses based on the name and unit type
			if strings.Contains(strings.ToLower(unit.Name), "tent") {
				equipment = append(equipment, "tent")
			}
			if strings.Contains(strings.ToLower(unit.Name), "rv") || unit.VehicleLength > 0 {
				equipment = append(equipment, "rv")
				if unit.VehicleLength > 0 {
					equipment = append(equipment, fmt.Sprintf("rv up to %d ft", unit.VehicleLength))
				}
			}
			if len(equipment) == 0 {
				equipment = append(equipment, "standard")
			}

			// cost per night is rate+fee i think
			rateFloat, err := strconv.ParseFloat(detailsResp.Rate, 64)
			if err != nil {
				rateFloat = 0.0
			}
			feeFloat, err := strconv.ParseFloat(detailsResp.Fee, 64)
			if err != nil {
				feeFloat = 0.0
			}
			costPerNight := rateFloat + feeFloat

			campsiteInfos = append(campsiteInfos, CampsiteInfo{
				ID:              strconv.Itoa(unit.UnitId),
				Name:            unit.Name,
				Type:            campsiteType,
				CostPerNight:    costPerNight,
				Rating:          0.0,
				Equipment:       equipment,
				Amenities:       []string{}, // No amenities available without details
				PreviewImageURL: "",
			})
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
			ID:              strconv.Itoa(detailsResp.Unit.UnitId),
			Name:            detailsResp.Unit.Name,
			Type:            campsiteType,
			CostPerNight:    costPerNight,
			Rating:          0.0, // ReserveCalifornia doesn't provide ratings
			Equipment:       equipment,
			Amenities:       amenities,
			PreviewImageURL: detailsResp.UnitImage,
		})

		// Add progressive delay to be respectful to the API
		time.Sleep(200 * time.Millisecond)
	}

	slog.Info("Completed campsite metadata fetch",
		slog.String("facilityId", facilityID),
		slog.Int("totalUnits", len(gridResp.Facility.Units)),
		slog.Int("successfulDetails", len(campsiteInfos)))

	return campsiteInfos, nil
}
