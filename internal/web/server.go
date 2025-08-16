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
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Provider  string            `json:"provider"`
	Lat       float64           `json:"lat"`
	Lon       float64           `json:"lon"`
	URL       string            `json:"url"`
	Rating    float64           `json:"rating"`
	Amenities map[string]string `json:"amenities"`
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
		SELECT provider, campground_id, name, latitude, longitude, rating, amenities
		FROM campgrounds
		WHERE latitude BETWEEN ? AND ?
		AND longitude BETWEEN ? AND ?
		AND latitude != 0 AND longitude != 0
	`, req.South, req.North, req.West, req.East)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CampgroundMapData
	for rows.Next() {
		var c CampgroundMapData
		var amenitiesJSON string
		err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon, &c.Rating, &amenitiesJSON)
		if err != nil {
			return nil, err
		}

		// Parse amenities JSON
		c.Amenities = make(map[string]string)
		if amenitiesJSON != "" {
			json.Unmarshal([]byte(amenitiesJSON), &c.Amenities)
		}

		c.URL = s.mgr.CampgroundURL(c.Provider, c.ID)
		result = append(result, c)
	}
	return result, rows.Err()
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
