package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"

	"github.com/brensch/schniffer/internal/db"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	store *db.Store
	addr  string
	tmpl  *template.Template
	css   string
	js    string
}

type CampgroundMapData struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
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

func NewServer(store *db.Store, addr string) *Server {
	// Read CSS and JS files
	cssBytes, err := assets.ReadFile("assets/style.css")
	if err != nil {
		panic(fmt.Sprintf("failed to read CSS file: %v", err))
	}
	slog.Info("loaded CSS", slog.Int("bytes", len(cssBytes)))

	jsBytes, err := assets.ReadFile("assets/app.js")
	if err != nil {
		panic(fmt.Sprintf("failed to read JS file: %v", err))
	}
	slog.Info("loaded JS", slog.Int("bytes", len(jsBytes)))

	htmlBytes, err := assets.ReadFile("assets/index.html")
	if err != nil {
		panic(fmt.Sprintf("failed to read HTML file: %v", err))
	}
	slog.Info("loaded HTML", slog.Int("bytes", len(htmlBytes)))

	// Parse template
	tmpl, err := template.New("index").Parse(string(htmlBytes))
	if err != nil {
		panic(fmt.Sprintf("failed to parse template: %v", err))
	}

	// Create template data
	tmplData := struct {
		CSS template.CSS
		JS  template.JS
	}{
		CSS: template.CSS(string(cssBytes)),
		JS:  template.JS(string(jsBytes)),
	}

	// Pre-execute template to check for errors
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, tmplData); err != nil {
		panic(fmt.Sprintf("failed to execute template: %v", err))
	}
	slog.Info("template test execution successful", slog.Int("output_bytes", buf.Len()))

	return &Server{
		store: store,
		addr:  addr,
		tmpl:  tmpl,
		css:   string(cssBytes),
		js:    string(jsBytes),
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// Serve the main map page
	mux.HandleFunc("/", s.handleMapPage)

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

func (s *Server) handleMapPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Template data
	data := struct {
		CSS template.CSS
		JS  template.JS
	}{
		CSS: template.CSS(s.css),
		JS:  template.JS(s.js),
	}

	slog.Info("serving map page",
		slog.Int("css_length", len(data.CSS)),
		slog.Int("js_length", len(data.JS)))

	w.Header().Set("Content-Type", "text/html")
	if err := s.tmpl.Execute(w, data); err != nil {
		slog.Error("failed to execute template", slog.Any("err", err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
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
		result = append(result, CampgroundMapData{
			ID:       c.ID,
			Name:     c.Name,
			Provider: c.Provider,
			Lat:      c.Lat,
			Lon:      c.Lon,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("failed to encode campgrounds", slog.Any("err", err))
	}
}

func (s *Server) handleViewportAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ViewportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	// Determine if we should cluster based on zoom level and count
	shouldCluster := req.Zoom < 10 || len(campgrounds) > 50

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
		SELECT provider, id, name, lat, lon
		FROM campgrounds
		WHERE lat BETWEEN ? AND ?
		AND lon BETWEEN ? AND ?
		AND lat != 0 AND lon != 0
	`, req.South, req.North, req.West, req.East)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CampgroundMapData
	for rows.Next() {
		var c CampgroundMapData
		if err := rows.Scan(&c.Provider, &c.ID, &c.Name, &c.Lat, &c.Lon); err != nil {
			return nil, err
		}
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
