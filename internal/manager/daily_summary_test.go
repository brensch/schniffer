package manager

import (
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

func TestDailySummaryScheduleParsesAt9pmPacific(t *testing.T) {
	loc, err := time.LoadLocation(dailySummaryTimezone)
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("timezone load returned %q, want America/Los_Angeles", loc.String())
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(dailySummarySchedule)
	if err != nil {
		t.Fatalf("parse schedule %q: %v", dailySummarySchedule, err)
	}

	// Pick a known moment in Pacific time and assert the next fire is at
	// 21:00 on the same/next day.
	base := time.Date(2026, 6, 7, 12, 0, 0, 0, loc) // noon PT
	next := sched.Next(base)
	wantHour := 21
	if h := next.In(loc).Hour(); h != wantHour {
		t.Errorf("next fire hour = %d, want %d (Pacific)", h, wantHour)
	}
	if next.In(loc).Minute() != 0 {
		t.Errorf("next fire minute = %d, want 0", next.In(loc).Minute())
	}
}
