package providers

import (
	"context"
	"time"
)

type Campsite struct {
	ID        string
	Date      time.Time
	Available bool
}

type Provider interface {
	Name() string
	// FetchAvailability returns campsite availability for the given campground and date range.
	FetchAvailability(ctx context.Context, campgroundID string, start, end time.Time) ([]Campsite, error)
	// FetchAllCampgrounds returns the full list of campgrounds and names from the provider.
	FetchAllCampgrounds(ctx context.Context) ([]CampgroundInfo, error)
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }

func (r *Registry) Register(name string, p Provider) { r.providers[name] = p }

func (r *Registry) Get(name string) (Provider, bool) { p, ok := r.providers[name]; return p, ok }

type CampgroundInfo struct {
	ID   string
	Name string
}
