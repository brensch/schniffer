package providers

import (
	"context"
	"time"
)

type Campsite struct {
	ID           string
	Date         time.Time
	Available    bool
	Type         string  // e.g., "TENT ONLY NONELECTRIC", "RV NONELECTRIC", "STANDARD NONELECTRIC"
	CostPerNight float64 // Cost per night in USD, 0 if unknown
}

type Provider interface {
	Name() string
	// FetchAvailability returns campsite availability for the given campground and date range.
	FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]Campsite, error)
	// FetchAllCampgrounds returns the full list of campgrounds and names from the provider.
	FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error)
	// CampsiteURL returns a link to the campsite details page for this provider.
	// campgroundID may be ignored by providers that only key by campsiteID.
	CampsiteURL(campgroundID, campsiteID string) string
	// CampgroundURL returns a link to the campground page for this provider.
	CampgroundURL(campgroundID string) string
	// PlanBuckets tells the manager how to split a set of exact dates (UTC days) into
	// the minimal set of upstream requests (inclusive day ranges) for this provider.
	// The input dates are unique and normalized to YYYY-MM-DD UTC.
	PlanBuckets(dates []time.Time) []DateRange
}

// DateRange represents an inclusive date span [Start..End] at day granularity.
// Providers that can efficiently fetch data in fixed windows (e.g., month, week)
// can declare their preferred batching by implementing Bucketizer.
type DateRange struct {
	Start time.Time
	End   time.Time
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

func (r *Registry) Register(name string, p Provider) { r.providers[name] = p }

func (r *Registry) Get(name string) (Provider, bool) { p, ok := r.providers[name]; return p, ok }

type CampgroundInfo struct {
	ID        string
	Name      string
	Lat       float64
	Lon       float64
	Rating    float64           // Campground rating (0-5), 0 if unknown
	Amenities map[string]string // Campground amenities
}
