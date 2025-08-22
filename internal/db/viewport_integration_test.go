package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Tunables (override via env for quicker iteration).
func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

var (
	cgCount         = envOrInt("CG_COUNT", 6000)
	campsitesPerCG  = envOrInt("CAMSITES_PER", 20)
	featuresPerSite = envOrInt("FEATURES_PER", 5)
)

func TestViewportIntegration(t *testing.T) {
	t.Helper()

	// open sqlite with performance-oriented pragmas
	db, err := sql.Open("sqlite3", "file:test.sqlite?_journal_mode=WAL&_synchronous=OFF")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	// extra pragmas for speed
	pragmas := []string{
		"PRAGMA page_size = 32768;",
		"PRAGMA mmap_size = 268435456;", // 256MB
		"PRAGMA locking_mode = EXCLUSIVE;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			t.Logf("pragma failed (%s): %v", p, err)
		}
	}

	ctx := context.Background()
	path := "schema.sql" // same dir as the test & viewport.go
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(b)); err != nil {
		t.Fatalf("apply schema.sql: %v", err)
	}

	seed := int64(42)
	rnd := rand.New(rand.NewSource(seed))

	t.Logf("generating data: %d campgrounds × %d campsites × %d features", cgCount, campsitesPerCG, featuresPerSite)
	startGen := time.Now()
	if err := populateData(ctx, db, rnd, cgCount, campsitesPerCG, featuresPerSite); err != nil {
		t.Fatalf("populate data: %v", err)
	}
	t.Logf("data generation finished in %v", time.Since(startGen))

	store := &Store{DB: db}

	// representative filters & timings
	tests := []struct {
		name   string
		filter ViewportFilters
	}{
		{
			name: "bbox_only",
			filter: ViewportFilters{
				South: -10, North: 10, West: -20, East: 20,
			},
		},
		{
			name: "rating_and_price",
			filter: ViewportFilters{
				South: -60, North: 60, West: -120, East: 120,
				MinRating: 3.5,
				MinPrice:  30, // aggregated over campsite_metadata
				MaxPrice:  120,
			},
		},
		{
			name: "type_and_equipment",
			filter: ViewportFilters{
				South: -45, North: 45, West: -90, East: 90,
				CampsiteTypes: []string{"group standard nonelectric", "standard nonelectric"},
				Equipment:     []string{"Tent", "RV"},
			},
		},
		{
			name: "feature_text_and_bool",
			filter: ViewportFilters{
				South: -50, North: 50, West: -100, East: 100,
				Features: []FeatureFilter{
					{Name: "Campsite Reserve Type", TextIn: []string{"Site-Specific"}},
					{Name: "Campfire Allowed", Bool: ptrBool(true)},
				},
			},
		},
		{
			name: "feature_numeric_range",
			filter: ViewportFilters{
				South: -30, North: 30, West: -60, East: 60,
				Features: []FeatureFilter{
					{Name: "Max Num of People", NumericMin: ptrFloat(4), NumericMax: ptrFloat(20)},
				},
			},
		},

		{
			name: "tight_bbox_and_all_filters",
			filter: ViewportFilters{
				South: -5, North: 5, West: -5, East: 5,
				MinRating:     4.0,
				MinPrice:      50,
				MaxPrice:      200,
				CampsiteTypes: []string{"standard nonelectric"},
				Equipment:     []string{"Trailer"},
				Features: []FeatureFilter{
					{Name: "Campsite Status", TextIn: []string{"Open"}},
					{Name: "Pets Allowed", Bool: ptrBool(true)},
					{Name: "Max Vehicle Length", NumericMin: ptrFloat(10)},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			rows, err := store.GetCampgroundsInViewport(ctx, tc.filter)
			dur := time.Since(start)
			if err != nil {
				t.Fatalf("query failed: %v", err)
			}
			// Simple sanity checks
			if len(rows) == 0 {
				t.Logf("[%s] returned 0 rows in %v", tc.name, dur)
			} else {
				// Log a couple of sample features to prove assembly worked
				totalFeatures := 0
				if len(rows) > 0 && len(rows[0].Features) > 0 {
					totalFeatures = len(rows[0].Features)
				}
				t.Logf("[%s] rows=%d, firstRowFeatures=%d, took=%v", tc.name, len(rows), totalFeatures, dur)
			}
		})
	}
}

func populateData(ctx context.Context, db *sql.DB, rnd *rand.Rand, cgCount, sitesPerCG, featuresPer int) error {
	provider := "recreation_gov"
	now := time.Now().UTC().Format(time.RFC3339)

	campsiteTypes := []string{
		"standard nonelectric", "group standard nonelectric", "tent only",
	}
	equipment := []string{"Tent", "RV", "Trailer"}
	reserveType := []string{"Site-Specific", "Non Site-Specific"}
	statusVals := []string{"Open", "Closed"}
	boolFeatures := []string{"Campfire Allowed", "Pets Allowed", "Shade"}
	textFeatures := []string{"Campsite Reserve Type", "Campsite Status", "Type", "Type Of Use", "Permitted Equipment"}
	numericFeatures := []string{"Max Num of People", "Max Vehicle Length"}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insCG, err := tx.PrepareContext(ctx, `
INSERT INTO campgrounds(provider, campground_id, name, latitude, longitude, rating, amenities, image_url, last_updated)
VALUES (?,?,?,?,?,?,?,?,?)
`)
	if err != nil {
		return err
	}
	defer insCG.Close()

	insCM, err := tx.PrepareContext(ctx, `
INSERT INTO campsite_metadata(provider, campground_id, campsite_id, name, price, rating, last_updated, image_url)
VALUES (?,?,?,?,?,?,?,?)
`)
	if err != nil {
		return err
	}
	defer insCM.Close()

	insCF, err := tx.PrepareContext(ctx, `
INSERT OR REPLACE INTO campsite_features(provider, campground_id, campsite_id, feature, value_text, value_numeric, value_boolean)
VALUES (?,?,?,?,?,?,?)
`)
	if err != nil {
		return err
	}
	defer insCF.Close()

	// Insert campgrounds
	for i := 0; i < cgCount; i++ {
		cgID := fmt.Sprintf("%d", 100000+i)
		name := fmt.Sprintf("Campground %d", i)
		lat := rndFloatRange(rnd, -80, 80)
		lon := rndFloatRange(rnd, -170, 170)
		rating := rndFloatRange(rnd, 2.0, 5.0)
		image := fmt.Sprintf("https://example.com/cg/%s.jpg", cgID)

		if _, err := insCG.ExecContext(ctx, provider, cgID, name, lat, lon, rating, "[]", image, now); err != nil {
			return fmt.Errorf("insert campground %d: %w", i, err)
		}

		// Insert campsites & features
		for s := 0; s < sitesPerCG; s++ {
			siteID := fmt.Sprintf("%s-%d", cgID, s)
			siteName := fmt.Sprintf("Site %d", s)
			minPrice := rndFloatRange(rnd, 10, 150)
			siteRating := rndFloatRange(rnd, 2.0, 5.0)
			siteImg := fmt.Sprintf("https://example.com/cs/%s.jpg", siteID)

			if _, err := insCM.ExecContext(ctx, provider, cgID, siteID, siteName, minPrice, siteRating, now, siteImg); err != nil {
				return fmt.Errorf("insert campsite %s: %w", siteID, err)
			}

			// Deterministically compose a reasonable set of features for this site
			// Always include a Type + Permitted Equipment + Reserve Type + Status
			tp := campsiteTypes[rnd.Intn(len(campsiteTypes))]
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Type", tp, nil, nil); err != nil {
				return err
			}
			eq := equipment[rnd.Intn(len(equipment))]
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Permitted Equipment", eq, nil, nil); err != nil {
				return err
			}
			rt := reserveType[rnd.Intn(len(reserveType))]
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Campsite Reserve Type", rt, nil, nil); err != nil {
				return err
			}
			st := statusVals[rnd.Intn(len(statusVals))]
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Campsite Status", st, nil, nil); err != nil {
				return err
			}

			// Numeric features
			nPeople := rndFloatRange(rnd, 1, 30)
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Max Num of People", nil, nPeople, nil); err != nil {
				return err
			}
			vehLen := rndFloatRange(rnd, 0, 40)
			if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, "Max Vehicle Length", nil, vehLen, nil); err != nil {
				return err
			}

			// Bool features
			for _, bf := range boolFeatures {
				b := rnd.Intn(2) == 0
				if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, bf, nil, nil, b); err != nil {
					return err
				}
			}

			// Fill the remainder up to featuresPer with random picks from text/numeric/bool lists.
			// We already inserted several deterministic ones; top up:
			inserted := 4 + 2 + len(boolFeatures)
			for f := inserted; f < featuresPer; f++ {
				switch rnd.Intn(3) {
				case 0: // text
					name := textFeatures[rnd.Intn(len(textFeatures))]
					val := randomTextValue(rnd, name, campsiteTypes, equipment, reserveType, statusVals)
					if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, name, val, nil, nil); err != nil {
						return err
					}
				case 1: // numeric
					name := numericFeatures[rnd.Intn(len(numericFeatures))]
					val := rndFloatRange(rnd, 0, 100)
					if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, name, nil, val, nil); err != nil {
						return err
					}
				default: // bool
					name := boolFeatures[rnd.Intn(len(boolFeatures))]
					b := rnd.Intn(2) == 0
					if _, err := insCF.ExecContext(ctx, provider, cgID, siteID, name, nil, nil, b); err != nil {
						return err
					}
				}
			}
		}

		// Periodic commit to keep WAL manageable
		if i%250 == 0 && i != 0 {
			if err := tx.Commit(); err != nil {
				return err
			}
			// begin new tx and reprepare (sqlite requires fresh statements after commit in some cases)
			tx, err = db.BeginTx(ctx, &sql.TxOptions{})
			if err != nil {
				return err
			}
			insCG, err = tx.PrepareContext(ctx, `
INSERT INTO campgrounds(provider, campground_id, name, latitude, longitude, rating, amenities, image_url, last_updated)
VALUES (?,?,?,?,?,?,?,?,?)
`)
			if err != nil {
				return err
			}
			insCM, err = tx.PrepareContext(ctx, `
INSERT INTO campsite_metadata(provider, campground_id, campsite_id, name, price, rating, last_updated, image_url)
VALUES (?,?,?,?,?,?,?,?)
`)
			if err != nil {
				return err
			}
			insCF, err = tx.PrepareContext(ctx, `
INSERT OR REPLACE INTO campsite_features(provider, campground_id, campsite_id, feature, value_text, value_numeric, value_boolean)
VALUES (?,?,?,?,?,?,?)
`)
			if err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func rndFloatRange(rnd *rand.Rand, lo, hi float64) float64 {
	return lo + rnd.Float64()*(hi-lo)
}

func randomTextValue(rnd *rand.Rand, name string, types, equip, rt, st []string) string {
	switch name {
	case "Type":
		return types[rnd.Intn(len(types))]
	case "Permitted Equipment":
		return equip[rnd.Intn(len(equip))]
	case "Campsite Reserve Type":
		return rt[rnd.Intn(len(rt))]
	case "Campsite Status":
		return st[rnd.Intn(len(st))]
	case "Type Of Use":
		alts := []string{"Overnight", "Day", "Overnight/Day"}
		return alts[rnd.Intn(len(alts))]
	default:
		// generic label value
		return fmt.Sprintf("%s-%d", name, rnd.Intn(5))
	}
}

func ptrBool(b bool) *bool        { return &b }
func ptrFloat(f float64) *float64 { return &f }
