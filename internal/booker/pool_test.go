package booker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestSessionOperationContextPreservesChromedpContext(t *testing.T) {
	sessionCtx, closeSession := chromedp.NewContext(context.Background())
	defer closeSession()

	opCtx, cancel := sessionOperationContext(sessionCtx, context.Background())
	defer cancel()

	if chromedp.FromContext(opCtx) == nil {
		t.Fatal("operation context lost chromedp session metadata")
	}
}

func TestSessionOperationContextPropagatesCallerCancellation(t *testing.T) {
	sessionCtx, closeSession := chromedp.NewContext(context.Background())
	defer closeSession()
	callerCtx, cancelCaller := context.WithCancel(context.Background())

	opCtx, cancel := sessionOperationContext(sessionCtx, callerCtx)
	defer cancel()
	cancelCaller()

	select {
	case <-opCtx.Done():
		if !errors.Is(opCtx.Err(), context.Canceled) {
			t.Fatalf("operation context error = %v, want context canceled", opCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("operation context was not canceled with caller")
	}
}

func TestSessionOperationContextPropagatesCallerDeadline(t *testing.T) {
	sessionCtx, closeSession := chromedp.NewContext(context.Background())
	defer closeSession()
	wantDeadline := time.Now().Add(time.Minute)
	callerCtx, cancelCaller := context.WithDeadline(context.Background(), wantDeadline)
	defer cancelCaller()

	opCtx, cancel := sessionOperationContext(sessionCtx, callerCtx)
	defer cancel()

	gotDeadline, ok := opCtx.Deadline()
	if !ok {
		t.Fatal("operation context has no deadline")
	}
	if !gotDeadline.Equal(wantDeadline) {
		t.Fatalf("operation deadline = %v, want %v", gotDeadline, wantDeadline)
	}
}
