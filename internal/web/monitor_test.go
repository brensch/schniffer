package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brensch/schniffer/internal/monitor"
)

func authedServer() *Server {
	s := &Server{}
	s.SetMonitor(monitor.NewAuth([]byte("test-signing-key-0123456789abcdef")), nil)
	return s
}

func TestMonitorPageRejectsNoToken(t *testing.T) {
	s := authedServer()
	rr := httptest.NewRecorder()
	s.handleMonitorPage(rr, httptest.NewRequest("GET", "/monitor", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no token/cookie should be 403, got %d", rr.Code)
	}
}

func TestMonitorPageRejectsBadToken(t *testing.T) {
	s := authedServer()
	rr := httptest.NewRecorder()
	s.handleMonitorPage(rr, httptest.NewRequest("GET", "/monitor?token=deadbeef", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bad token should be 403, got %d", rr.Code)
	}
}

func TestMonitorPageValidTokenSetsCookieAndRedirects(t *testing.T) {
	s := authedServer()
	tok, err := s.monAuth.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handleMonitorPage(rr, httptest.NewRequest("GET", "/monitor?token="+tok, nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("valid token should redirect (303), got %d", rr.Code)
	}
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == monitorCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("expected a session cookie to be set")
	}
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie must be HttpOnly+Secure+SameSite=Lax, got %+v", sess)
	}
	if !s.monAuth.ValidSession(sess.Value) {
		t.Fatal("session cookie should be valid")
	}
	// Reusable within TTL (tolerates a preview-fetch consuming a click):
	// the same token redeems again and issues another session.
	rr2 := httptest.NewRecorder()
	s.handleMonitorPage(rr2, httptest.NewRequest("GET", "/monitor?token="+tok, nil))
	if rr2.Code != http.StatusSeeOther {
		t.Fatalf("reused token within TTL should still redirect (303), got %d", rr2.Code)
	}
}

func TestInstrumentPreservesFlusher(t *testing.T) {
	// SSE needs http.Flusher to survive the metrics middleware wrapper.
	var gotFlusher bool
	h := instrument("test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotFlusher = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !gotFlusher {
		t.Fatal("instrumented writer must expose http.Flusher for SSE streaming")
	}
}

func TestMonitorStreamRejectsNoSession(t *testing.T) {
	s := authedServer()
	rr := httptest.NewRecorder()
	s.handleMonitorStream(rr, httptest.NewRequest("GET", "/api/monitor/stream", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("stream without session should be 401, got %d", rr.Code)
	}
}
