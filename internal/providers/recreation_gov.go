package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// CampgroundURL implements providers.Provider
func (r *RecreationGov) CampgroundURL(campgroundID string) string {
	if campgroundID == "" {
		return ""
	}
	return "https://www.recreation.gov/camping/campgrounds/" + campgroundID
}

// minimal response structs following campbot logic: availability is monthly and keyed by campsite id and date
type recGovResp struct {
	Campsites map[string]struct {
		Availabilities map[string]string `json:"availabilities"`
		CampsiteType   string            `json:"campsite_type"`
	} `json:"campsites"`
}

// FetchAvailability fetches monthly availability pages between start and end (inclusive by month).
func (r *RecreationGov) FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]CampsiteAvailability, error) {
	var out []CampsiteAvailability
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
			slog.Error("availability GET failed", slog.Any("err", err))
			return nil, fmt.Errorf("availability GET failed: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			slog.Error("availability read body failed", slog.Any("err", err))
			return nil, fmt.Errorf("availability read body failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			slog.Error("availability request failed, not ok", slog.Int("status", resp.StatusCode), slog.String("body", clipBody(body)))
			return nil, fmt.Errorf("recreation.gov availability status %d; body: %s", resp.StatusCode, clipBody(body))
		}
		var parsed recGovResp
		err = json.Unmarshal(body, &parsed)
		if err != nil {
			slog.Error("availability JSON decode failed", slog.Any("err", err), slog.String("body", clipBody(body)))
			return nil, fmt.Errorf("availability JSON decode failed: %w; body: %s", err, clipBody(body))
		}
		for siteID, data := range parsed.Campsites {
			for dateStr, status := range data.Availabilities {
				d, err := time.Parse(time.RFC3339, dateStr)
				if err != nil {
					slog.Error("bad date from rec.gov", slog.String("date", dateStr))
					continue
				}
				out = append(out, CampsiteAvailability{
					ID:        siteID,
					Date:      d,
					Available: status == "Available",
				})
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
	slog.Info("starting recreation.gov campground sync")
	start := 0
	size := 100
	var all []CampgroundInfo
	totalPages := 0

	for {
		totalPages++
		endpoint := fmt.Sprintf("https://www.recreation.gov/api/search?fq=entity_type%%3Acampground&size=%d&start=%d", size, start)
		slog.Debug("fetching recreation.gov campgrounds page", slog.Int("page", totalPages), slog.Int("start", start))

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
				Name          string  `json:"name"`
				EntityID      string  `json:"entity_id"`
				Latitude      string  `json:"latitude"`
				Longitude     string  `json:"longitude"`
				ParentID      string  `json:"parent_id"`
				ParentName    string  `json:"parent_name"`
				Reservable    bool    `json:"reservable"`
				AverageRating float64 `json:"average_rating"`
				Activities    []struct {
					ActivityName string `json:"activity_name"`
				} `json:"activities"`
				CampsiteEquipmentName []string `json:"campsite_equipment_name"`
				Description           string   `json:"description"`
				PreviewImageURL       string   `json:"preview_image_url"`
				PriceRange            struct {
					AmountMax float64 `json:"amount_max"`
					AmountMin float64 `json:"amount_min"`
					PerUnit   string  `json:"per_unit"`
				} `json:"price_range"`
			} `json:"results"`
			Size int `json:"size"`
		}
		if decErr := json.Unmarshal(body, &page); decErr != nil {
			return nil, fmt.Errorf("search JSON decode failed: %w; body: %s", decErr, clipBody(body))
		}

		slog.Debug("processed recreation.gov page",
			slog.Int("page", totalPages),
			slog.Int("results", len(page.Results)),
			slog.Int("size", page.Size))

		// Process this page's campgrounds
		processedOnPage := 0
		for _, result := range page.Results {
			if !result.Reservable {
				continue
			}
			var lat, lon float64
			if result.Latitude != "" {
				v, err := strconv.ParseFloat(result.Latitude, 64)
				if err == nil {
					lat = v
				}
			}
			if result.Longitude != "" {
				v, err := strconv.ParseFloat(result.Longitude, 64)
				if err == nil {
					lon = v
				}
			}

			// Create final name with parent info if available
			name := result.Name
			if result.ParentName != "" {
				name = result.ParentName + ": " + result.Name
			}

			// Build amenities list from activities only
			var amenities []string
			for _, activity := range result.Activities {
				amenities = append(amenities, strings.ToLower(activity.ActivityName))
			}

			campground := CampgroundInfo{
				ID:        result.EntityID,
				Name:      name,
				Lat:       lat,
				Lon:       lon,
				Rating:    result.AverageRating,
				Amenities: amenities,
				ImageURL:  result.PreviewImageURL,
				// PriceMin:  result.PriceRange.AmountMin,
				// PriceMax:  result.PriceRange.AmountMax,
				// PriceUnit: result.PriceRange.PerUnit,
			}

			all = append(all, campground)
			processedOnPage++
		}

		slog.Info("recreation.gov page processed",
			slog.Int("page", totalPages),
			slog.Int("processed_campgrounds", processedOnPage),
			slog.Int("total_campgrounds", len(all)))

		// Break if we got fewer results than requested, or no results at all
		if len(page.Results) < size || len(page.Results) == 0 {
			break
		}
		start += len(page.Results)
	}

	slog.Info("recreation.gov campground sync completed",
		slog.Int("total_pages", totalPages),
		slog.Int("total_campgrounds", len(all)))

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

// FetchCampsites fetches campsite metadata for storage in the database
func (r *RecreationGov) FetchCampsites(ctx context.Context, campgroundID string) ([]CampsiteInfo, error) {
	endpoint := fmt.Sprintf("https://www.recreation.gov/api/search/campsites?fq=asset_id%%3A%s&size=1000", campgroundID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create campsite metadata request: %w", err)
	}
	httpx.SpoofChromeHeaders(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch campsite metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("campsite metadata request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read campsite metadata response: %w", err)
	}

	var response struct {
		Campsites []struct {
			CampsiteID          string  `json:"campsite_id"`
			Name                string  `json:"name"`
			Type                string  `json:"type"`
			AverageRating       float64 `json:"average_rating"`
			PreviewImageURL     string  `json:"preview_image_url"`
			Reservable          bool    `json:"reservable"`
			CampsiteReserveType string  `json:"campsite_reserve_type"`
			CampsiteStatus      string  `json:"campsite_status"`
			TypeOfUse           string  `json:"type_of_use"`
			PermittedEquipment  []struct {
				EquipmentName string `json:"equipment_name"`
				MaxLength     int    `json:"max_length"`
			} `json:"permitted_equipment"`
			Attributes []struct {
				AttributeName  string `json:"attribute_name"`
				AttributeValue string `json:"attribute_value"`
			} `json:"attributes"`
			FeeTemplates map[string]string `json:"fee_templates"`
		} `json:"campsites"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse campsite metadata response: %w", err)
	}

	// --- Load rates once for the campground ---
	rateLookup, err := r.fetchCampgroundRatesLookup(ctx, campgroundID)
	if err != nil {
		slog.Warn("fetch campground rates failed", slog.String("campgroundID", campgroundID), slog.Any("err", err))
	}

	var campsiteInfos []CampsiteInfo
	for _, site := range response.Campsites {
		if !site.Reservable {
			continue
		}

		// Build Features
		// From attributes (keep API-provided human names as-is)
		var features []Features
		for _, attr := range site.Attributes {
			features = append(features, parseFeature(attr.AttributeName, attr.AttributeValue))
		}

		// Extra fields with friendly names
		features = append(features,
			parseFeature(friendlyName("campsite_reserve_type"), site.CampsiteReserveType),
			parseFeature(friendlyName("campsite_status"), site.CampsiteStatus),
			parseFeature(friendlyName("type"), site.Type),
			parseFeature(friendlyName("type_of_use"), site.TypeOfUse),
		)

		// Permitted equipment (friendly constant name)
		for _, eq := range site.PermittedEquipment {
			features = append(features, parseFeature("Permitted Equipment", eq.EquipmentName))
		}

		// Collect prices for this campsite
		var prices []float64
		for _, templateID := range site.FeeTemplates {
			if p, ok := rateLookup[templateID]; ok {
				prices = append(prices, p)
			}
		}

		var priceMin, priceMax float64
		if len(prices) > 0 {
			priceMin, priceMax = prices[0], prices[0]
			for _, p := range prices[1:] {
				if p < priceMin {
					priceMin = p
				}
				if p > priceMax {
					priceMax = p
				}
			}
		}

		campsiteInfo := CampsiteInfo{
			ID:              site.CampsiteID,
			Name:            site.Name,
			Type:            strings.ToLower(site.Type),
			PriceMin:        priceMin,
			PriceMax:        priceMax,
			Rating:          site.AverageRating,
			Features:        features,
			PreviewImageURL: site.PreviewImageURL,
		}
		campsiteInfos = append(campsiteInfos, campsiteInfo)
	}

	slog.Debug("fetched campsite metadata for campground",
		slog.String("campgroundID", campgroundID),
		slog.Int("campsite_count", len(campsiteInfos)))

	return campsiteInfos, nil
}

// friendlyName converts keys like "campsite_reserve_type" -> "Campsite Reserve Type".
func friendlyName(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	// Title-case each word using unicode
	var b strings.Builder
	space := true
	for _, r := range s {
		if space && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToTitle(r))
			space = false
			continue
		}
		if r == ' ' {
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseFeature converts a raw string into a typed Features value.
// Order: bool (stdlib) -> bool (yes/no) -> number -> text.
func parseFeature(name, raw string) Features {
	trim := strings.TrimSpace(raw)
	// 1) stdlib bool parse (true/false, 1/0, t/f with case variants)
	if bv, err := strconv.ParseBool(trim); err == nil {
		return Features{Name: name, ValueBoolean: &bv}
	}
	// 2) yes/no handling (case-insensitive, anywhere in the string)
	lower := strings.ToLower(trim)
	if strings.Contains(lower, "yes") {
		v := true
		return Features{Name: name, ValueBoolean: &v}
	}
	if strings.Contains(lower, "no") {
		v := false
		return Features{Name: name, ValueBoolean: &v}
	}
	// 3) numeric
	if num, err := strconv.ParseFloat(trim, 64); err == nil {
		return Features{Name: name, ValueNumeric: &num}
	}
	// 4) text fallback
	return Features{Name: name, ValueText: &trim}
}

// fetchCampgroundRatesLookup returns template_id -> max nightly price seen across all seasons
func (r *RecreationGov) fetchCampgroundRatesLookup(ctx context.Context, campgroundID string) (map[string]float64, error) {
	endpoint := fmt.Sprintf("https://www.recreation.gov/api/camps/campgrounds/%s/rates", campgroundID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create rates request: %w", err)
	}
	httpx.SpoofChromeHeaders(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch campground rates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rates request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read rates response: %w", err)
	}

	var rates struct {
		RatesList []struct {
			PriceMap map[string]float64 `json:"price_map"`
			RateMap  map[string]struct {
				PerNight float64 `json:"per_night"`
			} `json:"rate_map"`
		} `json:"rates_list"`
	}

	if err := json.Unmarshal(body, &rates); err != nil {
		return nil, fmt.Errorf("failed to parse rates response: %w", err)
	}

	lookup := make(map[string]float64) // template_id -> max price
	for _, season := range rates.RatesList {
		for templateID, rm := range season.RateMap {
			if rm.PerNight > lookup[templateID] {
				lookup[templateID] = rm.PerNight
			}
		}
		for templateID, price := range season.PriceMap {
			if price > lookup[templateID] {
				lookup[templateID] = price
			}
		}
	}

	return lookup, nil
}
