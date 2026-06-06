package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type fetchReq struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type fetchResp struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body"`
	Error   string              `json:"error,omitempty"`
	Elapsed int64               `json:"elapsed_ms"`
}

type batchReq struct {
	Requests []fetchReq `json:"requests"`
}

type batchResp struct {
	Responses []fetchResp `json:"responses"`
	Region    string      `json:"region,omitempty"`
}

var (
	client = &http.Client{Timeout: 25 * time.Second}
	secret = os.Getenv("PROXY_SECRET")
	region = os.Getenv("CLOUD_RUN_REGION")
)

func main() {
	if secret == "" {
		slog.Error("PROXY_SECRET env var is required")
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/fetch", handleFetch)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("proxy starting", "port", port, "region", region)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var br batchReq
	if err := json.NewDecoder(r.Body).Decode(&br); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(br.Requests) == 0 {
		http.Error(w, "no requests", http.StatusBadRequest)
		return
	}
	if len(br.Requests) > 100 {
		http.Error(w, "too many requests in batch (max 100)", http.StatusBadRequest)
		return
	}

	resps := make([]fetchResp, len(br.Requests))
	var wg sync.WaitGroup
	wg.Add(len(br.Requests))
	for i, req := range br.Requests {
		go func(i int, req fetchReq) {
			defer wg.Done()
			resps[i] = doOne(r.Context(), req)
		}(i, req)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(batchResp{Responses: resps, Region: region})
}

func doOne(ctx context.Context, req fetchReq) fetchResp {
	start := time.Now()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return fetchResp{Error: "bad request: " + err.Error(), Elapsed: time.Since(start).Milliseconds()}
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fetchResp{Error: "upstream: " + err.Error(), Elapsed: time.Since(start).Milliseconds()}
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fetchResp{Status: resp.StatusCode, Error: "read body: " + err.Error(), Elapsed: time.Since(start).Milliseconds()}
	}
	return fetchResp{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    string(bodyBytes),
		Elapsed: time.Since(start).Milliseconds(),
	}
}
