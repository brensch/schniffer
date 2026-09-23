package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/providers"
)

// metaProvider serves canned metadata; incrementalMetaProvider adds
// IncrementalCampsiteFetcher on top.
type metaProvider struct {
	fakeProvider
	campgrounds []providers.CampgroundInfo
	sites       map[string][]providers.CampsiteInfo
	siteErr     map[string]error
	fetchedCGs  []string
	skipped     map[string]map[string]bool
}

func (p *metaProvider) FetchAllCampgrounds(context.Context) ([]providers.CampgroundInfo, error) {
	return p.campgrounds, nil
}

func (p *metaProvider) FetchCampsites(_ context.Context, id string) ([]providers.CampsiteInfo, error) {
	p.fetchedCGs = append(p.fetchedCGs, id)
	if err := p.siteErr[id]; err != nil {
		return nil, err
	}
	return p.sites[id], nil
}

type incrementalMetaProvider struct{ metaProvider }

func (p *incrementalMetaProvider) FetchCampsitesSkipping(_ context.Context, id string, skip map[string]bool) ([]providers.CampsiteInfo, []string, error) {
	p.fetchedCGs = append(p.fetchedCGs, id)
	if p.skipped == nil {
		p.skipped = map[string]map[string]bool{}
	}
	p.skipped[id] = skip
	var fetched []providers.CampsiteInfo
	var seen []string
	for _, s := range p.sites[id] {
		seen = append(seen, s.ID)
		if !skip[s.ID] {
			fetched = append(fetched, s)
		}
	}
	return fetched, seen, nil
}

func newSyncTestManager(t *testing.T, prov providers.Provider) (*Manager, *db.Store) {
	t.Helper()
	store := newTestStore(t)
	reg := providers.NewRegistry()
	reg.Register(prov.Name(), prov)
	return &Manager{store: store, reg: reg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, store
}

func cgInfo(id string, price float64) providers.CampgroundInfo {
	return providers.CampgroundInfo{ID: id, Name: "cg " + id, Lat: 37, Lon: -120, PriceMin: price, PriceMax: price, PriceUnit: "night"}
}

func listedIDs(t *testing.T, store *db.Store, provider string) map[string]bool {
	t.Helper()
	rows, err := store.DB.Query(`SELECT campground_id FROM campgrounds WHERE provider=? AND removed_at IS NULL`, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out[id] = true
	}
	return out
}

func TestRefreshCampgroundListPreservesSiteDataAndMarksRemovals(t *testing.T) {
	ctx := context.Background()
	prov := &metaProvider{fakeProvider: fakeProvider{name: "p"}}
	m, store := newSyncTestManager(t, prov)

	prov.campgrounds = []providers.CampgroundInfo{cgInfo("a", 0), cgInfo("b", 0), cgInfo("c", 0)}
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	// Site-derived columns set by a campsite refresh must survive the next
	// list refresh.
	if _, err := store.DB.Exec(`UPDATE campgrounds SET campsite_types='["tent"]', price_min=10, price_max=20, metadata_checked_at=? WHERE campground_id='a'`, time.Now()); err != nil {
		t.Fatal(err)
	}

	prov.campgrounds = []providers.CampgroundInfo{cgInfo("a", 0), cgInfo("b", 0)}
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, "p"); len(got) != 2 || got["c"] {
		t.Fatalf("listed after removal = %v, want a,b", got)
	}
	var types string
	var pmin, pmax float64
	var checked *time.Time
	store.DB.QueryRow(`SELECT campsite_types, price_min, price_max, metadata_checked_at FROM campgrounds WHERE campground_id='a'`).Scan(&types, &pmin, &pmax, &checked)
	if types != `["tent"]` || pmin != 10 || pmax != 20 || checked == nil {
		t.Fatalf("site-derived columns clobbered: types=%s price=%v-%v checked=%v", types, pmin, pmax, checked)
	}

	// Reappearing un-removes; a real list price overwrites.
	prov.campgrounds = []providers.CampgroundInfo{cgInfo("a", 35), cgInfo("b", 0), cgInfo("c", 0)}
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, "p"); len(got) != 3 {
		t.Fatalf("listed after return = %v, want a,b,c", got)
	}
	store.DB.QueryRow(`SELECT price_min, price_max FROM campgrounds WHERE campground_id='a'`).Scan(&pmin, &pmax)
	if pmin != 35 || pmax != 35 {
		t.Fatalf("list price not applied: %v-%v", pmin, pmax)
	}
}

func TestRefreshCampgroundListIgnoresSuspiciouslyShortList(t *testing.T) {
	ctx := context.Background()
	prov := &metaProvider{fakeProvider: fakeProvider{name: "p"}}
	m, store := newSyncTestManager(t, prov)
	for i := range 10 {
		prov.campgrounds = append(prov.campgrounds, cgInfo(fmt.Sprint(i), 0))
	}
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	prov.campgrounds = prov.campgrounds[:2]
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	if got := listedIDs(t, store, "p"); len(got) != 10 {
		t.Fatalf("short list removed campgrounds: %d listed, want 10", len(got))
	}
}

func TestRefreshCampsitesIncremental(t *testing.T) {
	ctx := context.Background()
	prov := &incrementalMetaProvider{metaProvider{fakeProvider: fakeProvider{name: "p"}}}
	m, store := newSyncTestManager(t, prov)
	if err := store.UpsertCampground(ctx, "p", "a", "A", 37, -120, 0, nil, "", 0, 0, "night"); err != nil {
		t.Fatal(err)
	}

	prov.sites = map[string][]providers.CampsiteInfo{"a": {
		{ID: "1", Name: "one", Type: "tent", CostPerNight: 30, Equipment: []string{"tent"}},
		{ID: "2", Name: "two", Type: "rv", CostPerNight: 50, Equipment: []string{"rv"}},
		{ID: "3", Name: "three", Type: "cabin", CostPerNight: 90, Equipment: []string{"standard"}},
	}}
	if err := m.refreshCampsites(ctx, "p", prov, "a"); err != nil {
		t.Fatal(err)
	}

	// Next round: site 3 is gone, site 4 is new. 1 and 2 are recent, so
	// only 4 should be fetched, and 1/2 keep their equipment.
	prov.sites["a"] = []providers.CampsiteInfo{
		{ID: "1", Name: "one", Type: "tent", CostPerNight: 30, Equipment: []string{"tent"}},
		{ID: "2", Name: "two", Type: "rv", CostPerNight: 50, Equipment: []string{"rv"}},
		{ID: "4", Name: "four", Type: "yurt", CostPerNight: 70, Equipment: []string{"standard"}},
	}
	if err := m.refreshCampsites(ctx, "p", prov, "a"); err != nil {
		t.Fatal(err)
	}
	if skip := prov.skipped["a"]; !skip["1"] || !skip["2"] || skip["4"] {
		t.Fatalf("skip set = %v, want 1 and 2", skip)
	}

	var sites, equip int
	store.DB.QueryRow(`SELECT COUNT(*) FROM campsite_metadata WHERE campground_id='a'`).Scan(&sites)
	store.DB.QueryRow(`SELECT COUNT(*) FROM campsite_equipment WHERE campground_id='a'`).Scan(&equip)
	if sites != 3 || equip != 3 {
		t.Fatalf("sites=%d equipment=%d, want 3 and 3", sites, equip)
	}
	var typesJSON, equipJSON string
	var pmin, pmax float64
	store.DB.QueryRow(`SELECT campsite_types, equipment, price_min, price_max FROM campgrounds WHERE campground_id='a'`).Scan(&typesJSON, &equipJSON, &pmin, &pmax)
	var types, eq []string
	json.Unmarshal([]byte(typesJSON), &types)
	json.Unmarshal([]byte(equipJSON), &eq)
	if len(types) != 3 || len(eq) != 3 || pmin != 30 || pmax != 70 {
		t.Fatalf("aggregates types=%v equipment=%v price=%v-%v", types, eq, pmin, pmax)
	}
}

func TestRefreshCampsitesKeepsListPriceWhenSitesHaveNone(t *testing.T) {
	ctx := context.Background()
	prov := &metaProvider{fakeProvider: fakeProvider{name: "p"}}
	m, store := newSyncTestManager(t, prov)
	if err := store.UpsertCampground(ctx, "p", "a", "A", 37, -120, 0, nil, "", 25, 40, "night"); err != nil {
		t.Fatal(err)
	}
	prov.sites = map[string][]providers.CampsiteInfo{"a": {{ID: "1", Name: "one", Type: "tent"}}}
	if err := m.refreshCampsites(ctx, "p", prov, "a"); err != nil {
		t.Fatal(err)
	}
	var pmin, pmax float64
	store.DB.QueryRow(`SELECT price_min, price_max FROM campgrounds WHERE campground_id='a'`).Scan(&pmin, &pmax)
	if pmin != 25 || pmax != 40 {
		t.Fatalf("price = %v-%v, want list price 25-40 kept", pmin, pmax)
	}
}

func TestRefreshMetadataTickOldestFirstAndStopsOnRateLimit(t *testing.T) {
	ctx := context.Background()
	prov := &metaProvider{fakeProvider: fakeProvider{name: "p"}}
	m, store := newSyncTestManager(t, prov)

	// 721 campgrounds -> 2 per hourly tick over a 720-tick cycle.
	for i := range 721 {
		prov.campgrounds = append(prov.campgrounds, cgInfo(fmt.Sprintf("cg%03d", i), 0))
	}
	var lastList time.Time
	prov.sites = map[string][]providers.CampsiteInfo{}
	prov.siteErr = map[string]error{}

	// Everything freshly checked except two old ones and one never-checked.
	if err := m.refreshCampgroundList(ctx, "p", prov); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store.DB.Exec(`UPDATE campgrounds SET metadata_checked_at=?`, now)
	store.DB.Exec(`UPDATE campgrounds SET metadata_checked_at=? WHERE campground_id='cg005'`, now.Add(-40*24*time.Hour))
	store.DB.Exec(`UPDATE campgrounds SET metadata_checked_at=NULL WHERE campground_id='cg009'`)
	prov.siteErr["cg005"] = errors.New("boom")

	if err := m.refreshMetadataTick(ctx, "p", prov, &lastList); err != nil {
		t.Fatal(err)
	}
	if len(prov.fetchedCGs) != 2 || prov.fetchedCGs[0] != "cg009" || prov.fetchedCGs[1] != "cg005" {
		t.Fatalf("fetched %v, want [cg009 cg005]", prov.fetchedCGs)
	}
	// The failed one is backdated so it comes round again in about a day.
	var checked time.Time
	store.DB.QueryRow(`SELECT metadata_checked_at FROM campgrounds WHERE campground_id='cg005'`).Scan(&checked)
	if age := time.Since(checked); age < metadataCycle-metadataRetryAfter-time.Minute || age > metadataCycle {
		t.Fatalf("failed campground checked_at age = %v", age)
	}

	// A rate limit ends the tick without touching the rest of the batch.
	prov.fetchedCGs = nil
	store.DB.Exec(`UPDATE campgrounds SET metadata_checked_at=NULL WHERE campground_id='cg001'`)
	store.DB.Exec(`UPDATE campgrounds SET metadata_checked_at=? WHERE campground_id='cg002'`, now.Add(-50*24*time.Hour))
	prov.siteErr["cg001"] = fmt.Errorf("status 429: %w", providers.ErrRateLimited)
	err := m.refreshMetadataTick(ctx, "p", prov, &lastList)
	if !errors.Is(err, providers.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if len(prov.fetchedCGs) != 1 {
		t.Fatalf("fetched %v after rate limit, want just cg001", prov.fetchedCGs)
	}
}
