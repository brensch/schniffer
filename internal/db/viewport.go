// Package db contains data-access helpers for viewport queries.
package db

import (
	"context"
	"database/sql"
	"strings"
)

// FeatureFilter enables filtering on specific (feature, value) constraints.
type FeatureFilter struct {
	Name       string
	TextIn     []string
	NumericMin *float64
	NumericMax *float64
	Bool       *bool
}

// ViewportFilters includes the spatial window and optional filters.
type ViewportFilters struct {
	North, South, East, West float64
	MinRating                float64
	MinPrice, MaxPrice       float64

	// Convenience shortcuts:
	CampsiteTypes []string
	Equipment     []string

	// Full feature filters:
	Features []FeatureFilter
}

// GetCampgroundsInViewport returns one CampgroundRow per campground inside the
// requested bounding box, with optional filters. Efficient because:
//
//   - All bbox/rating/price/feature filters are applied inside a CTE ("base")
//     that also computes MIN(cm.price) per campground.
//   - Features for the details-pane are returned via a deduplicated LEFT JOIN
//     that collects distinct (feature, value_*) tuples across the campground’s campsites.
func (s *Store) GetCampgroundsInViewport(ctx context.Context, f ViewportFilters) ([]CampgroundRow, error) {
	var base strings.Builder
	args := make([]any, 0, 64)

	// -------- Base CTE: bbox & filters + per-campground price aggregate --------
	base.WriteString(`
WITH base AS (
  SELECT
    c.provider,
    c.campground_id,
    c.name,
    c.latitude,
    c.longitude,
    c.rating,
    c.image_url,
    MIN(cm.price) AS price
  FROM campgrounds c
  LEFT JOIN campsite_metadata cm
    ON cm.provider = c.provider
   AND cm.campground_id = c.campground_id
  WHERE c.latitude  BETWEEN ? AND ?
    AND c.longitude BETWEEN ? AND ?
    AND c.latitude  != 0
    AND c.longitude != 0
`)
	args = append(args, f.South, f.North, f.West, f.East)

	// Rating filter
	if f.MinRating > 0 {
		base.WriteString(`    AND c.rating >= ?` + "\n")
		args = append(args, f.MinRating)
	}

	// Campsite type shortcut
	if len(f.CampsiteTypes) > 0 {
		base.WriteString(`
    AND EXISTS (
      SELECT 1
      FROM campsite_features cf_t
      WHERE cf_t.provider = c.provider
        AND cf_t.campground_id = c.campground_id
        AND (cf_t.feature = 'type' OR cf_t.feature = 'Type')
        AND cf_t.value_text IN (` + placeholders(len(f.CampsiteTypes)) + `)
    )
`)
		for _, v := range f.CampsiteTypes {
			args = append(args, v)
		}
	}

	// Equipment shortcut
	if len(f.Equipment) > 0 {
		base.WriteString(`
    AND EXISTS (
      SELECT 1
      FROM campsite_features cf_e
      WHERE cf_e.provider = c.provider
        AND cf_e.campground_id = c.campground_id
        AND cf_e.feature = 'Permitted Equipment'
        AND cf_e.value_text IN (` + placeholders(len(f.Equipment)) + `)
    )
`)
		for _, v := range f.Equipment {
			args = append(args, v)
		}
	}

	// Full feature filters
	for _, ff := range f.Features {
		base.WriteString(`
    AND EXISTS (
      SELECT 1
      FROM campsite_features cf
      WHERE cf.provider = c.provider
        AND cf.campground_id = c.campground_id
        AND cf.feature = ?
`)
		args = append(args, ff.Name)

		conds := make([]string, 0, 4)
		if len(ff.TextIn) > 0 {
			conds = append(conds, `cf.value_text IN (`+placeholders(len(ff.TextIn))+`)`)
		}
		if ff.NumericMin != nil {
			conds = append(conds, `cf.value_numeric >= ?`)
		}
		if ff.NumericMax != nil {
			conds = append(conds, `cf.value_numeric <= ?`)
		}
		if ff.Bool != nil {
			if *ff.Bool {
				conds = append(conds, `cf.value_boolean = 1`)
			} else {
				conds = append(conds, `cf.value_boolean = 0`)
			}
		}
		if len(conds) > 0 {
			base.WriteString(`        AND (` + strings.Join(conds, " AND ") + `)` + "\n")
			if len(ff.TextIn) > 0 {
				for _, t := range ff.TextIn {
					args = append(args, t)
				}
			}
			if ff.NumericMin != nil {
				args = append(args, *ff.NumericMin)
			}
			if ff.NumericMax != nil {
				args = append(args, *ff.NumericMax)
			}
		}
		base.WriteString(`    )` + "\n")
	}

	// Group base rows to compute MIN price per campground
	base.WriteString(`
  GROUP BY c.provider, c.campground_id, c.name, c.latitude, c.longitude, c.rating, c.image_url
`)

	// Price filters
	having := make([]string, 0, 2)
	if f.MinPrice > 0 {
		having = append(having, `(MIN(cm.price) IS NULL OR MIN(cm.price) = 0 OR MIN(cm.price) >= ?)`)
		args = append(args, f.MinPrice)
	}
	if f.MaxPrice > 0 {
		having = append(having, `(MIN(cm.price) IS NULL OR MIN(cm.price) = 0 OR MIN(cm.price) <= ?)`)
		args = append(args, f.MaxPrice)
	}
	if len(having) > 0 {
		base.WriteString(`  HAVING ` + strings.Join(having, " AND ") + "\n")
	}
	base.WriteString(`)` + "\n") // end CTE base

	// -------- Outer SELECT: return one row per (campground, feature value) --------
	var sb strings.Builder
	sb.WriteString(base.String())

	sb.WriteString(`
SELECT
  b.provider,
  b.campground_id,
  b.name,
  b.latitude,
  b.longitude,
  b.rating,
  b.image_url,
  b.price,
  cf.feature,
  cf.value_text,
  cf.value_numeric,
  cf.value_boolean
FROM base b
LEFT JOIN (
  SELECT provider, campground_id, feature, value_text, value_numeric, value_boolean
  FROM campsite_features
  GROUP BY provider, campground_id, feature, value_text, value_numeric, value_boolean
) cf
  ON cf.provider = b.provider AND cf.campground_id = b.campground_id
ORDER BY b.rating DESC, b.name ASC
`)

	q := sb.String()

	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ p, id string }
	acc := make(map[key]*CampgroundRow, 256)

	for rows.Next() {
		var (
			provider, id, name, img string
			lat, lon, rating        float64
			price                   sql.NullFloat64

			fname sql.NullString
			vtext sql.NullString
			vnum  sql.NullFloat64
			vbool sql.NullBool
		)
		if err := rows.Scan(&provider, &id, &name, &lat, &lon, &rating, &img, &price, &fname, &vtext, &vnum, &vbool); err != nil {
			return nil, err
		}

		k := key{p: provider, id: id}
		row, ok := acc[k]
		if !ok {
			row = &CampgroundRow{
				Provider: provider,
				ID:       id,
				Name:     name,
				Lat:      lat,
				Lon:      lon,
				Rating:   rating,
				ImageURL: img,
				Price:    price,
				Features: make([]CampgroundFeature, 0, 8),
			}
			acc[k] = row
		}

		if fname.Valid {
			feat := CampgroundFeature{Name: fname.String}
			if vtext.Valid {
				t := vtext.String
				feat.ValueText = &t
			}
			if vnum.Valid {
				n := vnum.Float64
				feat.ValueNumeric = &n
			}
			if vbool.Valid {
				b := vbool.Bool
				feat.ValueBoolean = &b
			}
			row.Features = append(row.Features, feat)
		}
	}

	out := make([]CampgroundRow, 0, len(acc))
	for _, v := range acc {
		out = append(out, *v)
	}
	return out, rows.Err()
}

// placeholders returns "?, ?, ?, ..." for n > 0.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return "?"
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
	}
	return b.String()
}
