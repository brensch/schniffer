package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"

	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
)

type Server struct {
	store *db.Store
	mgr   *manager.Manager
	addr  string
}

type CampgroundMapData struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Provider      string   `json:"provider"`
	Lat           float64  `json:"lat"`
	Lon           float64  `json:"lon"`
	URL           string   `json:"url"`
	Rating        float64  `json:"rating"`
	Amenities     []string `json:"amenities"`
	CampsiteTypes []string `json:"campsite_types"`
	ImageURL      string   `json:"image_url"`
	PriceMin      float64  `json:"price_min"`
	PriceMax      float64  `json:"price_max"`
	PriceUnit     string   `json:"price_unit"`
}

type ClusterData struct {
	Lat   float64  `json:"lat"`
	Lon   float64  `json:"lon"`
	Count int      `json:"count"`
	Names []string `json:"names,omitempty"`
}

type ViewportRequest struct {
	North float64 `json:"north"`
	South float64 `json:"south"`
	East  float64 `json:"east"`
	West  float64 `json:"west"`
	Zoom  int     `json:"zoom"`
	// Filter parameters
	Amenities     []string `json:"amenities,omitempty"`
	CampsiteTypes []string `json:"campsite_types,omitempty"`
	MinRating     float64  `json:"min_rating,omitempty"`
	MinPrice      float64  `json:"min_price,omitempty"`
	MaxPrice      float64  `json:"max_price,omitempty"`
}

func NewServer(store *db.Store, mgr *manager.Manager, addr string) *Server {
	return &Server{
		store: store,
		mgr:   mgr,
		addr:  addr,
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Serve static files from the static directory
	fs := http.FileServer(http.Dir("./static/"))
	mux.Handle("/", fs)

	// API endpoint to get all campgrounds as JSON (for initial load/fallback)
	mux.HandleFunc("/api/campgrounds", s.handleCampgroundsAPI)

	// API endpoint to get campgrounds in viewport with clustering
	mux.HandleFunc("/api/viewport", s.handleViewportAPI)

	// API endpoint to get filter options
	mux.HandleFunc("/api/filter-options", s.handleFilterOptionsAPI)

	// API endpoint to get campground details
	mux.HandleFunc("/api/campground/", s.handleCampgroundDetail)

	// Group API endpoints
	mux.HandleFunc("/api/groups", s.handleGroups)
	mux.HandleFunc("/api/groups/create", s.handleCreateGroup)

	server := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		slog.Info("shutting down web server")
		server.Shutdown(context.Background())
	}()

	slog.Info("starting web server", slog.String("addr", s.addr))
	return server.ListenAndServe()
}

func (s *Server) handleCampgroundsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	campgrounds, err := s.store.GetAllCampgrounds(r.Context())
	if err != nil {
		slog.Error("failed to list campgrounds", slog.Any("err", err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var result []CampgroundMapData
	for _, c := range campgrounds {
		url := s.mgr.CampgroundURL(c.Provider, c.ID)
		result = append(result, CampgroundMapData{
			ID:       c.ID,
			Name:     c.Name,
			Provider: c.Provider,
			Lat:      c.Lat,
			Lon:      c.Lon,
			URL:      url,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(result)
	if err != nil {
		slog.Error("failed to encode campgrounds", slog.Any("err", err))
	}
}

func (s *Server) handleViewportAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ViewportRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Get campgrounds in viewport
	campgrounds, err := s.getCampgroundsInViewport(r.Context(), req)
	if err != nil {
		slog.Error("failed to get campgrounds in viewport", slog.Any("err", err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Determine if we should cluster based on count only
	shouldCluster := len(campgrounds) > 100

	w.Header().Set("Content-Type", "application/json")

	if shouldCluster {
		clusters := s.clusterCampgrounds(campgrounds, req.Zoom)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "clusters",
			"data": clusters,
		})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "individual",
			"data": campgrounds,
		})
	}
}

func (s *Server) getCampgroundsInViewport(ctx context.Context, req ViewportRequest) ([]CampgroundMapData, error) {
	rows, err := s.store.DB.QueryContext(ctx, `
		SELECT provider, campground_id, name, latitude, longitude, rating, amenities, campsite_types, image_url, price_min, price_max, price_unit
		FROM campgrounds
		WHERE latitude BETWEEN ? AND ?
		AND longitude BETWEEN ? AND ?
		AND latitude != 0 AND longitude != 0
	`, req.South, req.North, req.West, req.East)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var allCampgrounds []CampgroundMapData
	for rows.Next() {
		var c CampgroundMapData
		var amenitiesJSON, campsiteTypesJSON string
		err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon, &c.Rating, &amenitiesJSON, &campsiteTypesJSON, &c.ImageURL, &c.PriceMin, &c.PriceMax, &c.PriceUnit)
		if err != nil {
			return nil, err
		}

		// Parse amenities JSON
		if amenitiesJSON != "" {
			json.Unmarshal([]byte(amenitiesJSON), &c.Amenities)
		}

		// Parse campsite types JSON
		c.CampsiteTypes = []string{}
		if campsiteTypesJSON != "" {
			json.Unmarshal([]byte(campsiteTypesJSON), &c.CampsiteTypes)
		}

		c.URL = s.mgr.CampgroundURL(c.Provider, c.ID)
		allCampgrounds = append(allCampgrounds, c)
	}

	// Apply filters
	filteredCampgrounds := s.applyFilters(allCampgrounds, req)

	return filteredCampgrounds, rows.Err()
}

func (s *Server) applyFilters(campgrounds []CampgroundMapData, req ViewportRequest) []CampgroundMapData {
	var filtered []CampgroundMapData

	for _, campground := range campgrounds {
		// Check rating filter
		if req.MinRating > 0 && campground.Rating < req.MinRating {
			continue
		}

		// Check price filter
		if req.MinPrice > 0 && campground.PriceMin > 0 && campground.PriceMin < req.MinPrice {
			continue
		}
		if req.MaxPrice > 0 && campground.PriceMax > 0 && campground.PriceMax > req.MaxPrice {
			continue
		}

		// Check amenities filter
		if len(req.Amenities) > 0 {
			hasRequiredAmenity := false
			for _, reqAmenity := range req.Amenities {
				for _, campAmenity := range campground.Amenities {
					if reqAmenity == campAmenity {
						hasRequiredAmenity = true
						break
					}
				}
				if hasRequiredAmenity {
					break
				}
			}
			if !hasRequiredAmenity {
				continue
			}
		}

		// Check campsite types filter
		if len(req.CampsiteTypes) > 0 {
			hasRequiredType := false
			for _, reqType := range req.CampsiteTypes {
				for _, campType := range campground.CampsiteTypes {
					if reqType == campType {
						hasRequiredType = true
						break
					}
				}
				if hasRequiredType {
					break
				}
			}
			if !hasRequiredType {
				continue
			}
		}

		// If we get here, the campground passed all filters
		filtered = append(filtered, campground)
	}

	return filtered
}

func (s *Server) clusterCampgrounds(campgrounds []CampgroundMapData, zoom int) []ClusterData {
	if len(campgrounds) == 0 {
		return nil
	}

	// Grid size based on zoom level - much larger chunks when zoomed out
	var gridSize float64
	switch {
	case zoom <= 3:
		gridSize = 10.0 // Very large chunks for continent view
	case zoom <= 5:
		gridSize = 5.0 // Large chunks for country view
	case zoom <= 7:
		gridSize = 2.0 // Medium chunks for state/region view
	case zoom <= 9:
		gridSize = 1.0 // Smaller chunks for local area view
	default:
		gridSize = 0.5 // Fine clusters for detailed view
	}

	clusters := make(map[string]*ClusterData)

	for _, camp := range campgrounds {
		// Create grid cell coordinates
		gridLat := math.Floor(camp.Lat/gridSize) * gridSize
		gridLon := math.Floor(camp.Lon/gridSize) * gridSize
		key := fmt.Sprintf("%.4f,%.4f", gridLat, gridLon)

		if cluster, exists := clusters[key]; exists {
			cluster.Count++
			cluster.Lat = (cluster.Lat*float64(cluster.Count-1) + camp.Lat) / float64(cluster.Count)
			cluster.Lon = (cluster.Lon*float64(cluster.Count-1) + camp.Lon) / float64(cluster.Count)
			if len(cluster.Names) < 3 { // Limit preview names
				cluster.Names = append(cluster.Names, camp.Name)
			}
		} else {
			clusters[key] = &ClusterData{
				Lat:   camp.Lat,
				Lon:   camp.Lon,
				Count: 1,
				Names: []string{camp.Name},
			}
		}
	}

	var result []ClusterData
	for _, cluster := range clusters {
		result = append(result, *cluster)
	}
	return result
}

func (s *Server) handleCampgroundDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract provider and ID from URL path
	// Expected format: /api/campground/{provider}/{id}
	path := r.URL.Path[len("/api/campground/"):]
	if path == "" {
		http.Error(w, "Missing campground identifier", http.StatusBadRequest)
		return
	}

	// For now, just return a simple response
	// This could be expanded to show availability, campsites, etc.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Campground detail endpoint - to be implemented",
		"path":    path,
	})
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user")
	if userID == "" {
		http.Error(w, "user parameter required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		groups, err := s.store.GetUserGroups(r.Context(), userID)
		if err != nil {
			slog.Error("Failed to get user groups", "error", err)
			http.Error(w, "Failed to get groups", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(groups)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

type CreateGroupRequest struct {
	Name        string             `json:"name"`
	Campgrounds []db.CampgroundRef `json:"campgrounds"`
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := r.URL.Query().Get("user")
	if userID == "" {
		http.Error(w, "user parameter required", http.StatusBadRequest)
		return
	}

	var req CreateGroupRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Name == "" {
		http.Error(w, "Group name is required", http.StatusBadRequest)
		return
	}

	if len(req.Campgrounds) == 0 {
		http.Error(w, "At least one campground is required", http.StatusBadRequest)
		return
	}

	if len(req.Campgrounds) > 10 {
		http.Error(w, "Maximum 10 campgrounds allowed per group", http.StatusBadRequest)
		return
	}

	group, err := s.store.CreateGroup(r.Context(), userID, req.Name, req.Campgrounds)
	if err != nil {
		slog.Error("Failed to create group", "error", err)
		http.Error(w, "Failed to create group", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(group)
}

type FilterOptions struct {
	Amenities     []string `json:"amenities"`
	CampsiteTypes []string `json:"campsite_types"`
	PriceRange    struct {
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	} `json:"price_range"`
	RatingRange struct {
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	} `json:"rating_range"`
}

func (s *Server) handleFilterOptionsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Get all unique amenities
	amenitiesRows, err := s.store.DB.QueryContext(ctx, `
		SELECT DISTINCT amenities 
		FROM campgrounds 
		WHERE amenities IS NOT NULL AND amenities != '' AND amenities != '{}'
	`)
	if err != nil {
		http.Error(w, "Failed to fetch amenities", http.StatusInternalServerError)
		return
	}
	defer amenitiesRows.Close()

	amenitiesSet := make(map[string]bool)
	for amenitiesRows.Next() {
		var amenitiesJSON string
		if err := amenitiesRows.Scan(&amenitiesJSON); err != nil {
			continue
		}
		var amenities []string
		if err := json.Unmarshal([]byte(amenitiesJSON), &amenities); err != nil {
			continue
		}
		for _, amenity := range amenities {
			if amenity != "" {
				amenitiesSet[amenity] = true
			}
		}
	}

	// Get all unique campsite types
	campsiteTypesRows, err := s.store.DB.QueryContext(ctx, `
		SELECT DISTINCT campsite_types 
		FROM campgrounds 
		WHERE campsite_types IS NOT NULL AND campsite_types != '' AND campsite_types != '{}'
	`)
	if err != nil {
		http.Error(w, "Failed to fetch campsite types", http.StatusInternalServerError)
		return
	}
	defer campsiteTypesRows.Close()

	campsiteTypesSet := make(map[string]bool)
	for campsiteTypesRows.Next() {
		var campsiteTypesJSON string
		if err := campsiteTypesRows.Scan(&campsiteTypesJSON); err != nil {
			continue
		}
		var campsiteTypes []string
		if err := json.Unmarshal([]byte(campsiteTypesJSON), &campsiteTypes); err != nil {
			continue
		}
		for _, campsiteType := range campsiteTypes {
			if campsiteType != "" {
				campsiteTypesSet[campsiteType] = true
			}
		}
	}

	// Get price and rating ranges
	var priceMin, priceMax, ratingMin, ratingMax float64
	err = s.store.DB.QueryRowContext(ctx, `
		SELECT 
			COALESCE(MIN(CASE WHEN price_min > 0 THEN price_min END), 0),
			COALESCE(MAX(price_max), 0),
			COALESCE(MIN(rating), 0),
			COALESCE(MAX(rating), 5)
		FROM campgrounds
	`).Scan(&priceMin, &priceMax, &ratingMin, &ratingMax)
	if err != nil {
		http.Error(w, "Failed to fetch price/rating ranges", http.StatusInternalServerError)
		return
	}

	// Convert sets to sorted slices
	var amenitiesList []string
	for amenity := range amenitiesSet {
		amenitiesList = append(amenitiesList, amenity)
	}

	var campsiteTypesList []string
	for campsiteType := range campsiteTypesSet {
		campsiteTypesList = append(campsiteTypesList, campsiteType)
	}

	options := FilterOptions{
		Amenities:     amenitiesList,
		CampsiteTypes: campsiteTypesList,
	}
	options.PriceRange.Min = priceMin
	options.PriceRange.Max = priceMax
	options.RatingRange.Min = ratingMin
	options.RatingRange.Max = ratingMax

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(options)
}
