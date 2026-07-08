package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brensch/schniffer/internal/monitor"
)

func authedServer() *Server {
	s := &Server{}
	s.SetMonitor(monitor.NewAuth(), nil)
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
	// The single-use token must not redeem again.
	rr2 := httptest.NewRecorder()
	s.handleMonitorPage(rr2, httptest.NewRequest("GET", "/monitor?token="+tok, nil))
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("reused token should be 403, got %d", rr2.Code)
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
