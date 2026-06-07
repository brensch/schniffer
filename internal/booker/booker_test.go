package booker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBookingResponseErrorRejectsLogicalFailure(t *testing.T) {
	err := bookingResponseError(map[string]any{
		"status": float64(200),
		"ok":     false,
		"error":  "Our system has detected abnormal activity from your computer network. Please try again.",
	}, "")
	if !errors.Is(err, ErrHumanVerification) {
		t.Fatalf("bookingResponseError() = %v, want ErrHumanVerification", err)
	}
}

func TestBookingResponseErrorRequiresReservationID(t *testing.T) {
	err := bookingResponseError(map[string]any{"status": float64(200)}, "")
	if err == nil || !strings.Contains(err.Error(), "no reservation id") {
		t.Fatalf("bookingResponseError() = %v, want missing reservation id error", err)
	}
}

func TestBookingResponseErrorAcceptsReservation(t *testing.T) {
	if err := bookingResponseError(map[string]any{"status": float64(200)}, "reservation-123"); err != nil {
		t.Fatalf("bookingResponseError() = %v, want nil", err)
	}
}

func TestExtractOrderIDPrefersReservationID(t *testing.T) {
	got := ExtractOrderID(map[string]any{
		"reservation_id": "reservation-123",
		"order_id":       "order-456",
		"id":             "generic-789",
	})
	if got != "reservation-123" {
		t.Fatalf("ExtractOrderID() = %q, want reservation-123", got)
	}
}

func TestExtractOrderIDFallsBackToOrderID(t *testing.T) {
	got := ExtractOrderID(map[string]any{"order_id": "order-456"})
	if got != "order-456" {
		t.Fatalf("ExtractOrderID() = %q, want order-456", got)
	}
}

func TestOrderURLUsesReservationID(t *testing.T) {
	got := OrderURL("reservation-123")
	want := SiteOrigin + "/camping/reservations/orderdetails?id=reservation-123"
	if got != want {
		t.Fatalf("OrderURL() = %q, want %q", got, want)
	}
}

func TestCampsiteBookingURL(t *testing.T) {
	checkIn := time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC)
	checkOut := checkIn.AddDate(0, 0, 1)
	got := CampsiteBookingURL("10085607", checkIn, checkOut)
	for _, want := range []string{
		"/camping/campsites/10085607?",
		"book_now=true",
		"startDate=09%2F22%2F2026",
		"endDate=09%2F23%2F2026",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("CampsiteBookingURL() = %q, missing %q", got, want)
		}
	}
}
