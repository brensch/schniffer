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
	// ResolveCampgrounds searches known campgrounds by name or ID and returns tuples of (id, name).
	ResolveCampgrounds(ctx context.Context, query string, limit int) ([]CampgroundInfo, error)
	// FetchCampsiteMetadata returns site-level names/labels for a campground.
	FetchCampsiteMetadata(ctx context.Context, campgroundID string) ([]CampsiteInfo, error)
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

type CampsiteInfo struct {
	ID   string
	Name string
}
